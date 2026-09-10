package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
)

// sequential_changeover_backfill_test.go — the double-supply interlock.
//
// handleSequentialBackfill mints Order B whenever a sequential node's active
// order reaches in_transit. In STEADY STATE that is right and necessary: Order A
// (BuildSequentialRemovalSteps) only takes the full bin away, so without Order B
// the position never gets a fresh carrier.
//
// At CHANGEOVER Order A is a different animal. buildSequentialPerPositionSwap is
// a round trip — lift the old bin, drop it at the outbound destination, fetch the
// new style's carrier, bring it back to the SAME position. It refills the node
// itself. Minting Order B there sends a second robot with a second carrier to a
// position that already has one coming; the two-step Order B wins the race, and
// the five-step Order A returns to an occupied position and holds forever. The
// carrier Order B left is claimed by nobody.
//
// Observed 8/8 on the demo sim across two trees
// (REPORT-agent-changeover-wedge-2026-09-10.md §3), and first hit — under a wrong
// attribution — on 2026-08-28
// (HANDOFF-sequential-and-lane-streams-2026-08-28.md §5 Run 2).
//
// Both halves live in one test file on purpose: the interlock is only correct if
// it refuses the changeover swap AND still admits the steady-state removal, and a
// fix that over-reaches breaks a press's ordinary A/B cycling rather than its
// changeover — a far worse failure, and a silent one.

// ordersDroppingAt counts the orders whose stored plan sets a bin down at
// coreNode. It decodes steps_json by hand rather than calling legPlacesBinAt, so
// the pin does not assert through the same predicate the fix is built on.
func ordersDroppingAt(t *testing.T, db *store.DB, coreNode string) []int64 {
	t.Helper()
	all, err := db.ListOrders()
	testutil.MustNoErr(t, err, "list orders")
	var hits []int64
	for _, o := range all {
		stepsJSON, err := db.GetOrderStepsJSON(o.ID)
		testutil.MustNoErr(t, err, "steps json")
		if stepsJSON == "" {
			continue
		}
		var steps []protocol.ComplexOrderStep
		testutil.MustNoErr(t, json.Unmarshal([]byte(stepsJSON), &steps), "decode steps")
		for _, s := range steps {
			if s.Action == protocol.ActionDropoff && s.Node == coreNode {
				hits = append(hits, o.ID)
				break
			}
		}
	}
	return hits
}

// driveToInTransit moves an order to in_transit and publishes the status change
// on the bus, which is the path wireEventHandlers subscribes
// handleSequentialBackfill to. The test engine's order manager holds a no-op
// emitter, so the event is raised here with the payload orderEmitter builds.
func driveToInTransit(t *testing.T, eng *Engine, orderID, nodeID int64) {
	t.Helper()
	o, err := eng.db.GetOrder(orderID)
	testutil.MustNoErr(t, err, "get order")
	old := string(o.Status)
	testutil.MustNoErr(t, eng.db.UpdateOrderStatus(orderID, string(orders.StatusInTransit)), "in_transit")
	eng.Events.Emit(Event{Type: EventOrderStatusChanged, Payload: OrderStatusChangedEvent{
		OrderID:       orderID,
		OrderUUID:     o.UUID,
		OrderType:     o.OrderType,
		OldStatus:     old,
		NewStatus:     string(orders.StatusInTransit),
		ProcessNodeID: &nodeID,
	}})
}

// TestSequentialChangeover_OnePositionGetsOneCarrier is the wedge, pinned.
//
// One sequential position taking a changeover must end up with exactly ONE order
// bringing it a carrier — its own swap. A second one is the deadlock.
func TestSequentialChangeover_OnePositionGetsOneCarrier(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, _, activeNodeID, parkedNodeID, co := seedSequentialScenario(t, db, false)
	orderA, _ := seqTaskOrders(t, db, co.ID, activeNodeID, parkedNodeID)

	// The changeover planner has run. SEQ-A's swap is the only thing bringing a
	// carrier to SEQ-A; if that is not true the fixture, not the fix, is wrong.
	if before := ordersDroppingAt(t, db, "SEQ-A"); len(before) != 1 || before[0] != orderA {
		t.Fatalf("fixture: orders dropping at SEQ-A = %v, want exactly [%d] (the changeover swap) "+
			"before anything is driven", before, orderA)
	}

	driveToInTransit(t, eng, orderA, activeNodeID)

	after := ordersDroppingAt(t, db, "SEQ-A")
	if len(after) != 1 {
		t.Errorf("after the changeover swap went in_transit, %d orders deliver a carrier to SEQ-A "+
			"(%v); want 1. Order %d is already carrying the new style's carrier back to SEQ-A itself "+
			"— a second one seats a carrier on the position while the swap robot is away, and the "+
			"swap returns to an occupied position and holds forever.", len(after), after, orderA)
	}

	rt, err := db.GetProcessNodeRuntime(activeNodeID)
	testutil.MustNoErr(t, err, "runtime")
	if rt.StagedOrderID != nil {
		t.Errorf("StagedOrderID = %d after a changeover swap went in_transit; want nil. The staged "+
			"slot is the backfill's, and a changeover swap has no backfill.", *rt.StagedOrderID)
	}
}

// TestSequentialSteadyState_RemovalStillMintsTheBackfill is the mirror, and the
// half that keeps the fix honest.
//
// The steady-state Order A only removes. Refusing its Order B would stop an A/B
// press feeding itself — the interlock has to read what Order A actually does,
// not that a sequential order went in_transit.
func TestSequentialSteadyState_RemovalStillMintsTheBackfill(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, protocol.SwapModeSequential)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	if _, err := eng.RequestProduceSwap(nodeID); err != nil {
		t.Fatalf("RequestProduceSwap: %v", err)
	}
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")
	if rt.ActiveOrderID == nil {
		t.Fatalf("fixture: the swap request left no active order on the node")
	}
	orderA := *rt.ActiveOrderID

	// Removal only: nothing is bringing a carrier to the node yet.
	if before := ordersDroppingAt(t, db, "PRODUCE-NODE"); len(before) != 0 {
		t.Fatalf("fixture: orders dropping at PRODUCE-NODE = %v, want none — the steady-state "+
			"Order A takes the full bin away and brings nothing back", before)
	}

	driveToInTransit(t, eng, orderA, nodeID)

	after := ordersDroppingAt(t, db, "PRODUCE-NODE")
	if len(after) != 1 {
		t.Fatalf("after the steady-state removal went in_transit, %d orders deliver a carrier to "+
			"PRODUCE-NODE (%v); want 1. Order %d only lifts the full bin out — without the backfill "+
			"the press has nothing to produce into.", len(after), after, orderA)
	}

	rt2, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime after")
	if rt2.StagedOrderID == nil || *rt2.StagedOrderID != after[0] {
		t.Errorf("StagedOrderID = %v, want the backfill order %d — the runtime has to point at it or "+
			"the next removal mints a second one", rt2.StagedOrderID, after[0])
	}
}
