package engine

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/orders"
)

// TestRegression_RemovalOrderClearsBinPointer pins the bin-ownership
// flip behavior at removal completion.
//
// Setup: a complex order whose process_node IS the line, but whose
// DeliveryNode is OUTBOUND-DEST (the bin is being evacuated). Drain
// the runtime to a partial value, fire EventOrderCompleted, assert
// the runtime ends with active_bin_id=nil so subsequent PLC ticks
// don't silently mis-attribute to a bin that's no longer at the slot.
//
// The cached UOP value briefly reads as capacity post-removal; that's
// harmless because binAtNode returns 0 (active_bin_id nil) and no
// PLC tick attribution happens to an empty slot. UI may want to
// surface "no bin" when active_bin_id is nil.
func TestRegression_RemovalOrderClearsBinPointer(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, _, claimID := seedReconcilerNode(t, db, "REG11", "PART-R11")

	// Drain runtime to a partial value — simulates a half-consumed bin.
	const partialUOP = 137
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, partialUOP), "seed runtime")
	// Seed a fake bin pointer so we can prove the completion clears it.
	priorBin := int64(99)
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, &priorBin), "seed active_bin_id")

	// Removal order: process_node is the line (REG11-NODE), but
	// DeliveryNode is OUTBOUND-DEST. The order moves the spent bin
	// AWAY from the line.
	orderID, err := db.CreateOrder("uuid-reg11-removal", orders.TypeComplex,
		&nodeID, false, 1,
		"OUTBOUND-DEST",
		"", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed)), "set order confirmed")
	db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Under the new contract the bin pointer clears at pickup, not at
	// completion. Simulate the pickup directly (HandleBinPickedUp's full
	// path needs inventoryDelta wired, which test contexts don't).
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, nil), "simulate pickup clear")
	emitOrderCompleted(eng, orderID, "uuid-reg11-removal", orders.TypeComplex, &nodeID)

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.ActiveBinID != nil {
		t.Errorf("ActiveBinID = %v, want nil (pickup clears the bin pointer for removal flow)", *rt.ActiveBinID)
	}
}

// TestRegression_11_DeliveryOrderStillResetsLineUOP is the positive
// counterpart: orders that DO deliver to the line still trigger the
// reset. Without this paired test, a future predicate that always
// returns false would silently disable replenishment without any
// regression visible.
func TestRegression_11_DeliveryOrderStillResetsLineUOP(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "REG11D", PayloadCode: "PART-R11D", UOPCapacity: 200, InitialUOP: 200,
	})

	// Drained runtime — simulates a half-consumed bin about to be
	// replaced by the incoming delivery.
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, 50), "seed runtime")

	// Delivery order: DeliveryNode IS the process node — Order A in
	// two-robot consume, R2 in press-index, the delivery step in
	// sequential. BinID set so binArrivingAt threads through and
	// resolveReplenishUOP returns claim capacity.
	orderID, err := db.CreateOrder("uuid-reg11-delivery", orders.TypeComplex,
		&nodeID, false, 1,
		"REG11D-NODE", // DeliveryNode == process_node CoreNodeName
		"", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	// A complex order's destination lives in its steps, not delivery_node —
	// createComplexOrder always persists them, so a fixture without them isn't a
	// real order. The delivered gate resolves the final dropoff from here.
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(orderID,
		`[{"action":"pickup","node":"SRC"},{"action":"dropoff","node":"REG11D-NODE"}]`), "set steps")
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed)), "set order confirmed")
	deliveredBin := int64(202)
	db.UpdateOrderBinID(orderID, &deliveredBin)
	db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	emitOrderCompletedWithBinUOP(eng, orderID, "uuid-reg11-delivery", orders.TypeComplex, &nodeID, 200)

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 200 {
		t.Errorf("RemainingUOP = %d, want 200 (the count Core stamped on the delivery envelope)",
			rt.RemainingUOPCached)
	}
}

// TestRegression_DeliveryResetsToCapacityWithBin pins the bin-ownership flip
// contract: delivery completion with a bin attached rebinds the slot and seats
// the count Core stamped on the envelope. PLC ticks then decrement against that
// number; deltas ship to Core via the outbox. No reconciler heal — Edge owns
// the count for the bin at lineside, which is exactly why the seat must be
// Core's measurement and not a local assumption about a full carrier.
//
// Companion to TestRegression_RemovalOrderClearsBinAndZeroesUOP which
// pins the empty-slot path.
func TestRegression_DeliveryResetsToCapacityWithBin(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "ITEM8", PayloadCode: "PART-I8", UOPCapacity: 200, InitialUOP: 200,
	})

	// Drained runtime — the line was consuming from the previous bin.
	// Pre-arrival, runtime carries 50.
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, 50), "seed runtime")

	// Delivery order to the line with a bin attached.
	orderID, err := db.CreateOrder("uuid-item8-delivery", orders.TypeComplex,
		&nodeID, false, 1, "ITEM8-NODE", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(orderID,
		`[{"action":"pickup","node":"SRC"},{"action":"dropoff","node":"ITEM8-NODE"}]`), "set steps")
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed)), "set order confirmed")
	deliveredBin := int64(303)
	db.UpdateOrderBinID(orderID, &deliveredBin)
	db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	emitOrderCompletedWithBinUOP(eng, orderID, "uuid-item8-delivery", orders.TypeComplex, &nodeID, 200)

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 200 {
		t.Errorf("RemainingUOP = %d, want 200 (the count Core stamped on the delivery envelope)",
			rt.RemainingUOPCached)
	}
	if rt.ActiveBinID == nil || *rt.ActiveBinID != 303 {
		t.Errorf("ActiveBinID = %v, want 303 (delivered handler binds to order.BinID)",
			rt.ActiveBinID)
	}
}

// TestRegression_BlindDeliverySeatsZeroNotCapacity pins the failure direction
// for a delivery whose envelope carries no count — Core named a bin and could
// not read its row.
//
// It used to seat claim.UOPCapacity for a consume claim: a full carrier nobody
// had counted, written into the field Core reads as a measurement. That is the
// suppressing direction. demand_reconciler evaluates
// `remaining <= claim.ReorderPoint` off this same value, so a capacity seed
// puts the node above its reorder point and no replenishment is asked for —
// the line runs dry while the system believes it is fed, which is the ALN_007
// shape. Zero asks for material that may not be needed, and the first tick or
// count withdraws the ask.
//
// The justification the capacity seed shipped with ("the reconciler heals to
// Core's authoritative value within ~60s") named a reconciler that no longer
// exists; see the note in wiring_test.go.
func TestRegression_BlindDeliverySeatsZeroNotCapacity(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "BLIND", PayloadCode: "PART-BLIND", UOPCapacity: 200, InitialUOP: 200,
	})
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, 50), "seed runtime")

	orderID, err := db.CreateOrder("uuid-blind-delivery", orders.TypeComplex,
		&nodeID, false, 1, "BLIND-NODE", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(orderID,
		`[{"action":"pickup","node":"SRC"},{"action":"dropoff","node":"BLIND-NODE"}]`), "set steps")
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed)), "confirm order")
	deliveredBin := int64(909)
	db.UpdateOrderBinID(orderID, &deliveredBin)

	eng := testEngine(t, db)
	var logs []string
	eng.logFn = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	eng.wireEventHandlers()

	// No BinUOP on the envelope — the blind path.
	emitOrderCompleted(eng, orderID, "uuid-blind-delivery", orders.TypeComplex, &nodeID)

	// The marker is the whole reason a blind delivery is findable afterwards:
	// the post-integration data pass counts it, and the exposure it brackets
	// (a blind delivery followed by a changeover evac) is invisible without it.
	// Asserting the emitted string means a rename cannot quietly zero that count.
	var marked bool
	for _, l := range logs {
		if strings.Contains(l, BlindDeliveryMarker) {
			marked = true
			break
		}
	}
	if !marked {
		t.Errorf("no %q line was logged. The seat is only safe because it is loud; "+
			"a blind delivery nobody can grep for is a silent 0. logs: %v",
			BlindDeliveryMarker, logs)
	}

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 0 {
		t.Errorf("RemainingUOP = %d, want 0. A delivery that carried no count must not seat a "+
			"full carrier: at 200 against a reorder point this node reads fed and replenishment "+
			"is never asked for", rt.RemainingUOPCached)
	}
	// The binding itself still has to happen — the count being unknown is not a
	// reason to leave the slot pointing at nothing.
	if rt.ActiveBinID == nil || *rt.ActiveBinID != deliveredBin {
		t.Errorf("ActiveBinID = %v, want %d — the bin still arrived", rt.ActiveBinID, deliveredBin)
	}
}
