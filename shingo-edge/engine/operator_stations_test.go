package engine

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/service"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// testEngineDB hands out a fresh SQLite database with the full schema applied.
//
// The schema comes from a TEMPLATE built once per test binary, not from running
// the migration chain per call. A migration-chain Open costs ~70ms solo and
// 350-400ms under this package's parallelism (119 DDL statements, and the
// Windows file I/O under parallel open inflates 5x+); this package opens
// hundreds of test databases, so that was most of its wall time. Copying a
// pre-migrated file is pure sequential I/O.
//
// The template is built with the real store.Open (so migrations AND the
// post-migration schema verification run for real, once) and closed cleanly —
// SQLite in WAL mode leaves the data in the main file after the last
// connection closes, so copying the single file is complete.
var (
	engineDBTemplateOnce sync.Once
	engineDBTemplatePath string
	engineDBTemplateErr  error
)

func buildEngineDBTemplate() error {
	dir, err := os.MkdirTemp("", "shingo-edge-engine-tpl-*")
	if err != nil {
		return err
	}
	db, err := store.Open(filepath.Join(dir, "template.db"))
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	if err := db.Close(); err != nil {
		os.RemoveAll(dir)
		return err
	}
	engineDBTemplatePath = filepath.Join(dir, "template.db")
	return nil
}

func testEngineDB(t *testing.T) *store.DB {
	t.Helper()
	engineDBTemplateOnce.Do(func() { engineDBTemplateErr = buildEngineDBTemplate() })
	if engineDBTemplateErr != nil {
		t.Fatalf("build db template: %v", engineDBTemplateErr)
	}
	dst := filepath.Join(t.TempDir(), "test.db")
	src, err := os.ReadFile(engineDBTemplatePath)
	if err != nil {
		t.Fatalf("read db template: %v", err)
	}
	if err := os.WriteFile(dst, src, 0o600); err != nil {
		t.Fatalf("copy db template: %v", err)
	}
	// OpenMigrated, not Open: the copy already carries the template's schema,
	// and re-running the migration chain per test was the cost this pool exists
	// to remove. verifySchema still runs inside, so a bad template fails loudly.
	db, err := store.OpenMigrated(dst)
	if err != nil {
		t.Fatalf("open copied db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedProcessNode creates a minimal process + node for testing.
// Returns the process ID and node ID.
func seedProcessNode(t *testing.T, db *store.DB) (processID, nodeID int64) {
	t.Helper()
	pid, err := db.CreateProcess("TEST-PROC", "test process", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nid, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    pid,
		CoreNodeName: "TEST-NODE",
		Code:         "TN1",
		Name:         "Test Node",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create process node: %v", err)
	}
	return pid, nid
}

func TestCanAcceptOrders(t *testing.T) {
	t.Parallel()
	t.Run("available when no orders and no changeover", func(t *testing.T) {
		db := testEngineDB(t)
		_, nodeID := seedProcessNode(t, db)
		eng := &Engine{db: db}

		ok, reason := eng.CanAcceptOrders(nodeID)
		if !ok {
			t.Errorf("expected available, got: %s", reason)
		}
	})

	t.Run("available with no runtime state", func(t *testing.T) {
		db := testEngineDB(t)
		_, nodeID := seedProcessNode(t, db)
		// Don't call EnsureProcessNodeRuntime — no runtime row exists
		eng := &Engine{db: db}

		ok, reason := eng.CanAcceptOrders(nodeID)
		if !ok {
			t.Errorf("expected available with no runtime, got: %s", reason)
		}
	})

	t.Run("unavailable with active order", func(t *testing.T) {
		db := testEngineDB(t)
		_, nodeID := seedProcessNode(t, db)
		orderID, err := db.CreateOrder("uuid-active", orders.TypeRetrieve, &nodeID, false, 1, "TEST-NODE", "", "", "", false, "")
		if err != nil {
			t.Fatalf("create order: %v", err)
		}
		db.UpdateOrderStatus(orderID, string(orders.StatusSubmitted))
		db.EnsureProcessNodeRuntime(nodeID)
		db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)
		eng := &Engine{db: db}

		ok, reason := eng.CanAcceptOrders(nodeID)
		if ok {
			t.Error("expected unavailable with active order")
		}
		if reason != "active order in progress" {
			t.Errorf("expected 'active order in progress', got: %s", reason)
		}
	})

	t.Run("unavailable with staged order", func(t *testing.T) {
		db := testEngineDB(t)
		_, nodeID := seedProcessNode(t, db)
		orderID, err := db.CreateOrder("uuid-staged", orders.TypeComplex, &nodeID, false, 1, "", "", "", "", false, "")
		if err != nil {
			t.Fatalf("create order: %v", err)
		}
		db.UpdateOrderStatus(orderID, string(orders.StatusStaged))
		db.EnsureProcessNodeRuntime(nodeID)
		db.UpdateProcessNodeRuntimeOrders(nodeID, nil, &orderID)
		eng := &Engine{db: db}

		ok, reason := eng.CanAcceptOrders(nodeID)
		if ok {
			t.Error("expected unavailable with staged order")
		}
		if reason != "staged order in progress" {
			t.Errorf("expected 'staged order in progress', got: %s", reason)
		}
	})

	t.Run("available when active order is terminal", func(t *testing.T) {
		db := testEngineDB(t)
		_, nodeID := seedProcessNode(t, db)
		orderID, err := db.CreateOrder("uuid-terminal", orders.TypeRetrieve, &nodeID, false, 1, "TEST-NODE", "", "", "", false, "")
		if err != nil {
			t.Fatalf("create order: %v", err)
		}
		db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed))
		db.EnsureProcessNodeRuntime(nodeID)
		db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)
		eng := &Engine{db: db}

		ok, reason := eng.CanAcceptOrders(nodeID)
		if !ok {
			t.Errorf("expected available with terminal order, got: %s", reason)
		}
	})

	t.Run("unavailable during changeover when node participates", func(t *testing.T) {
		db := testEngineDB(t)
		processID, nodeID := seedProcessNode(t, db)

		// Create two styles for changeover
		fromStyleID, err := db.CreateStyle("Style-A", "old style", processID)
		if err != nil {
			t.Fatalf("create from style: %v", err)
		}
		toStyleID, err := db.CreateStyle("Style-B", "new style", processID)
		if err != nil {
			t.Fatalf("create to style: %v", err)
		}

		// Create active changeover WITH a node task for this node — the node is
		// actually being swapped, so it must be blocked.
		existing, err := db.ListProcessNodesByProcess(processID)
		if err != nil {
			t.Fatalf("list process nodes: %v", err)
		}
		tasks := []processes.NodeTaskInput{{ProcessID: processID, CoreNodeName: "TEST-NODE", Situation: "swap", State: "swap_required"}}
		_, err = service.NewChangeoverService(db).Create(processID, &fromStyleID, toStyleID, "test", "test changeover", nil, tasks, nil, existing)
		if err != nil {
			t.Fatalf("create changeover: %v", err)
		}
		eng := &Engine{db: db}

		ok, reason := eng.CanAcceptOrders(nodeID)
		if ok {
			t.Error("expected unavailable during changeover")
		}
		if reason != "changeover in progress" {
			t.Errorf("expected 'changeover in progress', got: %s", reason)
		}
	})

	t.Run("available during changeover when node does not participate", func(t *testing.T) {
		db := testEngineDB(t)
		processID, nodeID := seedProcessNode(t, db)

		fromStyleID, err := db.CreateStyle("Style-A", "old style", processID)
		if err != nil {
			t.Fatalf("create from style: %v", err)
		}
		toStyleID, err := db.CreateStyle("Style-B", "new style", processID)
		if err != nil {
			t.Fatalf("create to style: %v", err)
		}

		// Active changeover, but NO node task references this node — e.g. a bin
		// loader that only supplies empties and isn't part of the swap. It must
		// stay available (regression guard for the "node X unavailable:
		// changeover in progress" loader bug).
		_, err = service.NewChangeoverService(db).Create(processID, &fromStyleID, toStyleID, "test", "loader not in changeover", nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("create changeover: %v", err)
		}
		eng := &Engine{db: db}

		ok, reason := eng.CanAcceptOrders(nodeID)
		if !ok {
			t.Errorf("expected available (node not in changeover), got: %s", reason)
		}
	})

	t.Run("available when changeover completed", func(t *testing.T) {
		db := testEngineDB(t)
		processID, nodeID := seedProcessNode(t, db)

		fromStyleID, err := db.CreateStyle("Style-A", "old style", processID)
		if err != nil {
			t.Fatalf("create from style: %v", err)
		}
		toStyleID, err := db.CreateStyle("Style-B", "new style", processID)
		if err != nil {
			t.Fatalf("create to style: %v", err)
		}

		coID, err := service.NewChangeoverService(db).Create(processID, &fromStyleID, toStyleID, "test", "test changeover", nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("create changeover: %v", err)
		}
		// Mark changeover completed
		db.UpdateProcessChangeoverState(coID, domain.ChangeoverCompleted)

		eng := &Engine{db: db}
		ok, reason := eng.CanAcceptOrders(nodeID)
		if !ok {
			t.Errorf("expected available after changeover completed, got: %s", reason)
		}
	})
}
