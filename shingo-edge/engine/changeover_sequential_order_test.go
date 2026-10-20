package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
)

// changeover_sequential_order_test.go — on a sequential pair the pulling
// side's changeover leg is created when its partner's leg delivers, and only
// then. Before that it holds no order, so it hard-claims no bin at Core for a
// pickup it cannot reach: the A/B strand (FINDINGS-ab-strand-press10-2026-10-20).

// seqTaskOf is the order links of the node task of nodeID in the changeover.
func seqTaskOf(t *testing.T, db *store.DB, coID, nodeID int64) (next, old *int64) {
	t.Helper()
	task, err := db.GetChangeoverNodeTaskByNode(coID, nodeID)
	testutil.MustNoErr(t, err, "node task")
	return task.NextMaterialOrderID, task.OldMaterialReleaseOrderID
}

// driveTo moves an order to status and raises the status change on the bus.
func driveTo(t *testing.T, eng *Engine, orderID, nodeID int64, status protocol.Status) {
	t.Helper()
	o, err := eng.db.GetOrder(orderID)
	testutil.MustNoErr(t, err, "get order")
	old := string(o.Status)
	testutil.MustNoErr(t, eng.db.UpdateOrderStatus(orderID, string(status)), "set status")
	eng.Events.Emit(Event{Type: EventOrderStatusChanged, Payload: OrderStatusChangedEvent{
		OrderID: orderID, OrderUUID: o.UUID, OrderType: o.OrderType,
		OldStatus: old, NewStatus: string(status), ProcessNodeID: &nodeID,
	}})
}

// TestSequentialChangeover_ThePullingLegWaitsForThePartnersDelivery — at the
// start only the parked side has a leg; the pulling side has none. When the
// parked side's leg delivers, the pulling side's leg is created, one order
// bringing its carrier.
func TestSequentialChangeover_ThePullingLegWaitsForThePartnersDelivery(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, _, activeNodeID, parkedNodeID, co := seedSequentialScenarioRaw(t, db, false)

	if next, old := seqTaskOf(t, db, co.ID, activeNodeID); next != nil || old != nil {
		t.Fatalf("pulling side at start: next %v old %v, want no leg until its partner delivers", next, old)
	}
	parked, _ := seqTaskOf(t, db, co.ID, parkedNodeID)
	if parked == nil {
		t.Fatal("parked side at start has no leg")
	}
	if got := ordersDroppingAt(t, db, "SEQ-A"); len(got) != 0 {
		t.Fatalf("orders bringing SEQ-A a carrier at start = %v, want none", got)
	}

	driveTo(t, eng, *parked, parkedNodeID, orders.StatusInTransit)
	if next, _ := seqTaskOf(t, db, co.ID, activeNodeID); next != nil {
		t.Fatalf("pulling side has leg %d while its partner is only in transit", *next)
	}
	driveTo(t, eng, *parked, parkedNodeID, orders.StatusDelivered)

	next, _ := seqTaskOf(t, db, co.ID, activeNodeID)
	if next == nil {
		t.Fatal("pulling side has no leg after its partner delivered")
	}
	if got := ordersDroppingAt(t, db, "SEQ-A"); len(got) != 1 || got[0] != *next {
		t.Errorf("orders bringing SEQ-A a carrier = %v, want exactly its leg %d", got, *next)
	}
}

// TestSequentialChangeover_AFailedPartnerHoldsThePullingLegUntilItsRetryDelivers
// — the partner's leg fails: no pulling leg. A pulling leg created now would
// claim its bins ahead of the partner's retry and could form the same cycle.
// The operator retries the partner (a new next-material leg on its task); that
// leg delivers, and the pulling leg is created then.
func TestSequentialChangeover_AFailedPartnerHoldsThePullingLegUntilItsRetryDelivers(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, processID, activeNodeID, parkedNodeID, co := seedSequentialScenarioRaw(t, db, false)
	parked, _ := seqTaskOf(t, db, co.ID, parkedNodeID)

	driveTo(t, eng, *parked, parkedNodeID, orders.StatusFailed)
	if next, _ := seqTaskOf(t, db, co.ID, activeNodeID); next != nil {
		t.Fatalf("pulling side has leg %d after its partner's leg failed, want none until a partner leg delivers", *next)
	}

	retry, err := eng.StageNodeChangeoverMaterial(processID, parkedNodeID)
	testutil.MustNoErr(t, err, "the operator retries the partner")
	if again, _ := seqTaskOf(t, db, co.ID, parkedNodeID); again == nil || *again != retry.ID {
		t.Fatalf("partner task's leg = %v, want the retry %d", again, retry.ID)
	}
	if next, _ := seqTaskOf(t, db, co.ID, activeNodeID); next != nil {
		t.Fatalf("pulling side has leg %d while the partner's retry is out", *next)
	}
	driveTo(t, eng, retry.ID, parkedNodeID, orders.StatusDelivered)
	if next, _ := seqTaskOf(t, db, co.ID, activeNodeID); next == nil {
		t.Fatal("pulling side has no leg after the partner's retry delivered")
	}
}

// TestSequentialChangeover_AnEvacuateIsNotHeld — a tooling evacuate clears the
// pull from both positions (applyNodeAction), so there is no pulling side and
// nothing is held: both legs exist at the start.
func TestSequentialChangeover_AnEvacuateIsNotHeld(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, _, activeNodeID, parkedNodeID, co := seedSequentialScenarioRaw(t, db, true)
	for _, n := range []int64{activeNodeID, parkedNodeID} {
		if next, old := seqTaskOf(t, db, co.ID, n); next == nil && old == nil {
			t.Errorf("node %d has no leg at the start of an evacuate", n)
		}
	}
}
