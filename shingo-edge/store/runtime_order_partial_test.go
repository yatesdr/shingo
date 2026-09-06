package store

import (
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// The set path was never hardened the way the clear path was.
// ClearRuntimeOrderRefs carries the warning in prose — "a blanket (nil, nil)
// write would drop the live sibling too" — and UpdateRuntimeOrders, thirty
// lines above it, is exactly that blanket write. Twelve production sites called
// it as `(&order.ID, nil)` to mean "the active order is now N", which also said
// "and there is no staged order", a claim none of them was in a position to make:
// a two-robot swap's sibling leg lives in that slot.
//
// These pin the partial setters. Behaviour, not spelling: seat both pointers,
// write one, and require the other to still be there.
func TestSetRuntimeActiveOrder_LeavesTheSiblingAlone(t *testing.T) {
	t.Parallel()
	db, nodeID, supply, evac := seedRuntimeWithBothOrders(t, "PARTIAL-ACTIVE")

	if err := db.SetProcessNodeRuntimeActiveOrder(nodeID, &supply); err != nil {
		t.Fatalf("set active order: %v", err)
	}

	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveOrderID == nil || *rt.ActiveOrderID != supply {
		t.Errorf("active = %v, want %d", rt.ActiveOrderID, supply)
	}
	if rt.StagedOrderID == nil || *rt.StagedOrderID != evac {
		t.Fatalf("staged = %v, want %d — writing the active pointer must not destroy the "+
			"surviving evac leg. That is the whole defect.", rt.StagedOrderID, evac)
	}
}

func TestSetRuntimeStagedOrder_LeavesTheSiblingAlone(t *testing.T) {
	t.Parallel()
	db, nodeID, supply, evac := seedRuntimeWithBothOrders(t, "PARTIAL-STAGED")

	if err := db.SetProcessNodeRuntimeStagedOrder(nodeID, &evac); err != nil {
		t.Fatalf("set staged order: %v", err)
	}

	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveOrderID == nil || *rt.ActiveOrderID != supply {
		t.Fatalf("active = %v, want %d — writing the staged pointer must not destroy the "+
			"live supply leg.", rt.ActiveOrderID, supply)
	}
	if rt.StagedOrderID == nil || *rt.StagedOrderID != evac {
		t.Errorf("staged = %v, want %d", rt.StagedOrderID, evac)
	}
}

// A partial setter must also be able to clear just its own slot.
func TestSetRuntimeActiveOrder_NilClearsOnlyActive(t *testing.T) {
	t.Parallel()
	db, nodeID, _, evac := seedRuntimeWithBothOrders(t, "PARTIAL-NIL")

	if err := db.SetProcessNodeRuntimeActiveOrder(nodeID, nil); err != nil {
		t.Fatalf("clear active order: %v", err)
	}

	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveOrderID != nil {
		t.Errorf("active = %v, want nil", rt.ActiveOrderID)
	}
	if rt.StagedOrderID == nil || *rt.StagedOrderID != evac {
		t.Fatalf("staged = %v, want %d — clearing one slot is not a reason to clear the other.",
			rt.StagedOrderID, evac)
	}
}

// ClearRuntimeOrders keeps the blanket behaviour on purpose. It exists so that
// throwing away a slot you may not own is a decision with a word for it, and so
// the sites that mean it can be told from the sites that meant to set one
// pointer and took the other with them.
func TestClearRuntimeOrders_DropsBoth(t *testing.T) {
	t.Parallel()
	db, nodeID, _, _ := seedRuntimeWithBothOrders(t, "PARTIAL-CLEAR")

	if err := db.ClearProcessNodeRuntimeOrders(nodeID); err != nil {
		t.Fatalf("clear runtime orders: %v", err)
	}

	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveOrderID != nil || rt.StagedOrderID != nil {
		t.Errorf("clear left pointers behind: active=%v staged=%v", rt.ActiveOrderID, rt.StagedOrderID)
	}
}

// seedRuntimeWithBothOrders builds a node whose runtime holds BOTH pointers —
// the two-robot swap shape, where each slot belongs to a different leg.
func seedRuntimeWithBothOrders(t *testing.T, prefix string) (db *DB, nodeID, supply, evac int64) {
	t.Helper()
	db = coverageDB(t)
	pid, err := db.CreateProcess(prefix, "", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: pid, CoreNodeName: prefix + "-N", Code: prefix, Name: prefix,
		Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	// The order-pointer writes are bare UPDATEs — the runtime row has to exist
	// first or they silently affect nothing.
	testutil.MustNoErr(t, func() error { _, err := db.EnsureProcessNodeRuntime(nodeID); return err }(), "ensure runtime")

	supply, evac = int64(7101), int64(7102)
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &supply, &evac), "seed both legs")
	return db, nodeID, supply, evac
}
