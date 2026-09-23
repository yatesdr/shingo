//go:build docker

package store_test

import (
	"bytes"
	"io"
	"log"
	"strings"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/loaders"
)

// dropHomeNodeFK lets a test delete a node a loader home still points at, the
// state a map re-import or a hand edit leaves behind.
func dropHomeNodeFK(t *testing.T, db *store.DB) {
	t.Helper()
	if _, err := db.DB.Exec(`ALTER TABLE bin_loader_homes DROP CONSTRAINT IF EXISTS bin_loader_homes_position_node_id_fkey`); err != nil {
		t.Fatalf("drop fk: %v", err)
	}
}

func deleteNode(t *testing.T, db *store.DB, id int64) {
	t.Helper()
	if _, err := db.DB.Exec(`DELETE FROM nodes WHERE id=$1`, id); err != nil {
		t.Fatalf("delete node: %v", err)
	}
}

// seedDedicated is one dedicated loader with one payload-bearing home per node.
func seedDedicated(t *testing.T, db *store.DB, name string, nodeIDs ...int64) int64 {
	t.Helper()
	id, err := db.CreateLoader(loaders.Loader{Name: name, Role: loaders.RoleProduce,
		Layout: loaders.LayoutDedicatedPositions, Replenishment: loaders.ReplenishmentThreshold})
	if err != nil {
		t.Fatalf("CreateLoader: %v", err)
	}
	for i, n := range nodeIDs {
		if err := db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: n,
			PayloadCode: name + "-P" + string(rune('A'+i)), UOPThreshold: 10 + i}); err != nil {
			t.Fatalf("UpsertLoaderHome: %v", err)
		}
	}
	return id
}

// Both directions of the refusal, and the report line, on the one function both
// writers call. Not parallel: the archived case reads the process-global logger.
func TestDeriveDemandRegistry_RefusesOnlyTheUnresolvedEmpty(t *testing.T) {
	t.Run("unresolvable node with prior rows refuses and keeps them", func(t *testing.T) {
		db := testdb.Open(t)
		n := seedRegistryNode(t, db, "NT-DR-R", "DR-R-1")
		seedDedicated(t, db, "DR-R", n)
		if _, _, err := db.DeriveDemandRegistry("ST-DR-R"); err != nil {
			t.Fatalf("seed derive: %v", err)
		}
		dropHomeNodeFK(t, db)
		deleteNode(t, db, n)

		rep, changes, err := db.DeriveDemandRegistry("ST-DR-R")
		if err != nil {
			t.Fatalf("DeriveDemandRegistry: %v", err)
		}
		if !rep.Refused || rep.RowsOut != 0 || rep.PriorRows != 1 || rep.Skips.NodeGone != 1 || rep.Changes != 0 {
			t.Errorf("report = %+v, want refused, rows 0, prior 1, node_gone 1, changes 0", rep)
		}
		if changes != nil {
			t.Errorf("a refused derive returned changes %+v, want none", changes)
		}
		if got := registryRowCount(t, db, "ST-DR-R"); got != 1 {
			t.Errorf("rows = %d, want 1 kept", got)
		}
	})

	t.Run("every loader archived commits empty and prints", func(t *testing.T) {
		db := testdb.Open(t)
		n := seedRegistryNode(t, db, "NT-DR-A", "DR-A-1")
		id := seedDedicated(t, db, "DR-A", n)
		if _, _, err := db.DeriveDemandRegistry("ST-DR-A"); err != nil {
			t.Fatalf("seed derive: %v", err)
		}
		if err := db.DeleteLoader(id); err != nil {
			t.Fatalf("DeleteLoader: %v", err)
		}

		var buf bytes.Buffer
		prev := log.Writer()
		log.SetOutput(io.MultiWriter(prev, &buf))
		rep, changes, err := db.DeriveDemandRegistry("ST-DR-A")
		log.SetOutput(prev)
		if err != nil {
			t.Fatalf("DeriveDemandRegistry: %v", err)
		}
		if rep.Refused || rep.LoadersIn != 0 || rep.RowsOut != 0 || rep.PriorRows != 1 || rep.Changes != 1 {
			t.Errorf("report = %+v, want committed, loaders 0, rows 0, prior 1, changes 1", rep)
		}
		if len(changes) != 1 || changes[0].OldThreshold != 10 || changes[0].NewThreshold != 0 {
			t.Errorf("changes = %+v, want DR-A-PA 10->0", changes)
		}
		if got := registryRowCount(t, db, "ST-DR-A"); got != 0 {
			t.Errorf("rows = %d, want 0", got)
		}
		want := "demand registry derive: station=ST-DR-A outcome=committed loaders=0 rows=0 thresholded=0 prior=1 changes=1 skips{}"
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log = %q, want a line containing %q", buf.String(), want)
		}
	})

	t.Run("a partial derivation commits what it resolved", func(t *testing.T) {
		db := testdb.Open(t)
		live := seedRegistryNode(t, db, "NT-DR-P", "DR-P-LIVE")
		gone := seedRegistryNode(t, db, "NT-DR-P", "DR-P-GONE")
		seedDedicated(t, db, "DR-P", live, gone)
		if _, _, err := db.DeriveDemandRegistry("ST-DR-P"); err != nil {
			t.Fatalf("seed derive: %v", err)
		}
		dropHomeNodeFK(t, db)
		deleteNode(t, db, gone)

		rep, changes, err := db.DeriveDemandRegistry("ST-DR-P")
		if err != nil {
			t.Fatalf("DeriveDemandRegistry: %v", err)
		}
		if rep.Refused || rep.RowsOut != 1 || rep.Skips.NodeGone != 1 || len(changes) != 1 {
			t.Errorf("report = %+v changes = %+v, want committed, rows 1, node_gone 1, one change", rep, changes)
		}
		if got := registryRowCount(t, db, "ST-DR-P"); got != 1 {
			t.Errorf("rows = %d, want 1", got)
		}
	})

	t.Run("an unresolvable derivation with no prior rows has nothing to refuse", func(t *testing.T) {
		db := testdb.Open(t)
		n := seedRegistryNode(t, db, "NT-DR-N", "DR-N-1")
		seedDedicated(t, db, "DR-N", n)
		dropHomeNodeFK(t, db)
		deleteNode(t, db, n)

		rep, changes, err := db.DeriveDemandRegistry("ST-DR-N")
		if err != nil {
			t.Fatalf("DeriveDemandRegistry: %v", err)
		}
		if rep.Refused || rep.PriorRows != 0 || rep.Skips.NodeGone != 1 || len(changes) != 0 {
			t.Errorf("report = %+v changes = %+v, want committed, prior 0, node_gone 1, no changes", rep, changes)
		}
	})
}
