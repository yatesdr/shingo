package engine

import (
	"path/filepath"
	"testing"

	"shingoedge/orders"
	"shingoedge/store"
)

// testEngineDB opens a fresh SQLite database with full schema applied.
func testEngineDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
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
	nid, err := db.CreateProcessNode(store.ProcessNodeInput{
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
		db.UpdateOrderStatus(orderID, orders.StatusSubmitted)
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
		db.UpdateOrderStatus(orderID, orders.StatusStaged)
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
		db.UpdateOrderStatus(orderID, orders.StatusConfirmed)
		db.EnsureProcessNodeRuntime(nodeID)
		db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)
		eng := &Engine{db: db}

		ok, reason := eng.CanAcceptOrders(nodeID)
		if !ok {
			t.Errorf("expected available with terminal order, got: %s", reason)
		}
	})

	t.Run("unavailable during changeover", func(t *testing.T) {
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

		// Create active changeover
		_, err = db.CreateChangeover(processID, &fromStyleID, toStyleID, "test", "test changeover", nil, nil, nil)
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

		coID, err := db.CreateChangeover(processID, &fromStyleID, toStyleID, "test", "test changeover", nil, nil, nil)
		if err != nil {
			t.Fatalf("create changeover: %v", err)
		}
		// Mark changeover completed
		db.UpdateProcessChangeoverState(coID, "completed")

		eng := &Engine{db: db}
		ok, reason := eng.CanAcceptOrders(nodeID)
		if !ok {
			t.Errorf("expected available after changeover completed, got: %s", reason)
		}
	})
}
