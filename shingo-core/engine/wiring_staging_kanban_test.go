//go:build docker

package engine

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/store"
	"shingocore/store/nodes"
)

// wiring_staging_kanban_test.go — isStorageSlot coverage.
//
// Formerly wiring_kanban_test.go; the kanban demand-signal tests
// (handleKanbanDemand / sendDemandSignals) were deleted with the signal
// path itself — see the 2026-08 removal. What remains is the predicate
// those tests happened to sit beside: isStorageSlot, now living in
// wiring_staging.go and load-bearing for arrival staging.

// ── isStorageSlot — direct unit tests ───────────────────────────────

// seedLaneNodeType ensures the LANE node type exists. The store
// migrations already seed it on a fresh testDB, so we just verify
// it's present rather than re-inserting (which would violate the
// unique-code constraint).
func seedLaneNodeType(t *testing.T, db *store.DB) {
	t.Helper()
	types, err := db.ListNodeTypes()
	if err != nil {
		t.Fatalf("list node types: %v", err)
	}
	for _, nt := range types {
		if nt.Code == "LANE" {
			return
		}
	}
	// Fallback: create it if migrations didn't seed for some reason.
	testutil.MustNoErr(t, db.CreateNodeType(&nodes.NodeType{Code: "LANE", Name: "Lane", IsSynthetic: true}), "create LANE node type")
}

// TestIsStorageSlot_ChildOfLane returns true: the parent node is a LANE.
func TestIsStorageSlot_ChildOfLane(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	seedLaneNodeType(t, db)

	laneTypeID := mustGetNodeTypeID(t, db, "LANE")
	lane := &nodes.Node{Name: "LANE-K1", Enabled: true, NodeTypeID: &laneTypeID}
	testutil.MustNoErr(t, db.CreateNode(lane), "create lane")
	slot := &nodes.Node{Name: "LANE-K1-SLOT-1", Enabled: true, ParentID: &lane.ID}
	testutil.MustNoErr(t, db.CreateNode(slot), "create slot")

	if !eng.isStorageSlot(slot.ID) {
		t.Errorf("expected isStorageSlot(%d) true for child of LANE", slot.ID)
	}
}

// TestIsStorageSlot_ChildOfNonLane — parent exists but is not LANE.
func TestIsStorageSlot_ChildOfNonLane(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	parent := &nodes.Node{Name: "NOT-LANE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(parent), "create parent")
	child := &nodes.Node{Name: "NOT-LANE-CHILD", Enabled: true, ParentID: &parent.ID}
	testutil.MustNoErr(t, db.CreateNode(child), "create child")

	if eng.isStorageSlot(child.ID) {
		t.Errorf("isStorageSlot should be false when parent is not LANE")
	}
}

// TestIsStorageSlot_OrphanNode — no parent → not a storage slot.
func TestIsStorageSlot_OrphanNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	orphan := &nodes.Node{Name: "ORPHAN-1", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(orphan), "create orphan")

	if eng.isStorageSlot(orphan.ID) {
		t.Errorf("isStorageSlot must be false for orphan node")
	}
}

// TestIsStorageSlot_MissingNode — GetNode returns error → false.
func TestIsStorageSlot_MissingNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	if eng.isStorageSlot(987654321) {
		t.Errorf("isStorageSlot must be false for missing node")
	}
}

// mustGetNodeTypeID looks up the ID of a node type by its code — used
// because the Node struct takes NodeTypeID (the FK), not the string code.
func mustGetNodeTypeID(t *testing.T, db *store.DB, code string) int64 {
	t.Helper()
	types, err := db.ListNodeTypes()
	if err != nil {
		t.Fatalf("list node types: %v", err)
	}
	for _, nt := range types {
		if nt.Code == code {
			return nt.ID
		}
	}
	t.Fatalf("no node type with code %q in: %+v", code, types)
	return 0
}
