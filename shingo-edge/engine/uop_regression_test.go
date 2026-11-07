package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
)

// TestRegression_RemovalOrderClearsBinPointer pins the bin pointer through a
// removal: a complex order whose process_node IS the line, but whose
// DeliveryNode is OUTBOUND-DEST (the bin is being evacuated). The robot's lift
// at the line's slot clears active_bin_id (HandleBinPickedUp: the bound bin
// left), and the removal's completion does not bind it again, so later PLC
// ticks are held rather than charged to a bin no longer at the slot
// (binAtNode returns 0 for a nil pointer).
func TestRegression_RemovalOrderClearsBinPointer(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "REG11", PayloadCode: "PART-R11", UOPCapacity: 100,
	})

	// Drain runtime to a partial value — simulates a half-consumed bin.
	const partialUOP = 137
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, partialUOP), "seed runtime")
	// The half-consumed bin is the one bound at the slot.
	priorBin := int64(99)
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, &priorBin), "seed active_bin_id")

	// Removal order: process_node is the line (REG11-NODE), but
	// DeliveryNode is OUTBOUND-DEST. The order moves the spent bin
	// AWAY from the line.
	orderID, err := db.CreateOrder("uuid-reg11-removal", orders.TypeComplex,
		&nodeID, false, 1,
		"OUTBOUND-DEST",
		"", "", protocol.Height{}, false, "", "", "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed)), "set order confirmed")
	db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	eng := testEngine(t, db) // its delta sink writes the slot verbs through to db
	eng.wireEventHandlers()

	// The robot lifts the half-consumed bin at the line's slot.
	eng.HandleBinPickedUp("uuid-reg11-removal", priorBin, "REG11-NODE")
	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.ActiveBinID != nil {
		t.Fatalf("after the pickup ActiveBinID = %v, want nil (the bound bin left the slot)", *rt.ActiveBinID)
	}

	emitOrderCompleted(eng, orderID, "uuid-reg11-removal", orders.TypeComplex, &nodeID)
	rt, _ = db.GetProcessNodeRuntime(nodeID)
	if rt.ActiveBinID != nil {
		t.Errorf("after the completion ActiveBinID = %v, want nil (a removal binds nothing at the line)", *rt.ActiveBinID)
	}
}

// TestRegression_DeliveryResetsToCapacityWithBin pins the bin-ownership flip
// contract: a delivery to the line with a bin attached rebinds the slot to the
// order's bin and seats the count Core stamped on its landing
// (emitOrderCompletedWithBinUOP sends the landing, the delivered and the
// completion). PLC ticks then decrement against that number; deltas ship to
// Core via the outbox. No reconciler heal — Edge owns the count for the bin at
// lineside, which is exactly why the seat must be Core's measurement and not a
// local assumption about a full carrier.
//
// The delivery arm of TestRegression_RemovalOrderClearsBinPointer, which pins
// the removal: a removal filter that also swallowed deliveries would leave the
// line unbound and never replenished.
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
		&nodeID, false, 1, "ITEM8-NODE", "", "", protocol.Height{}, false, "", "", "")
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
// had counted, written into the field Core reads as a measurement — the
// suppressing direction (the ALN_007 shape). Then it seated 0 with a loud
// BLIND DELIVERY line. CONVERTED with Lane H (H.5, H.7): a delivered binds
// through the landing verb only for an older Core, and a carrier with no count
// is not bound at all — the blind seed is deleted. What the pin still holds is
// the direction and the noise: the node is NOT seated with a full carrier, and
// the gap is said, by name, through the delivered-not-bound alarm (the
// greppable line and event that replaced the marker).
func TestRegression_BlindDeliverySeatsZeroNotCapacity(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "BLIND", PayloadCode: "PART-BLIND", UOPCapacity: 200, InitialUOP: 200,
	})
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, 50), "seed runtime")

	orderID, err := db.CreateOrder("uuid-blind-delivery", orders.TypeComplex,
		&nodeID, false, 1, "BLIND-NODE", "", "", protocol.Height{}, false, "", "", "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(orderID,
		`[{"action":"pickup","node":"SRC"},{"action":"dropoff","node":"BLIND-NODE"}]`), "set steps")
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed)), "confirm order")
	deliveredBin := int64(909)
	db.UpdateOrderBinID(orderID, &deliveredBin)

	eng := testEngine(t, db)
	alarms := captureDeliveredNotBound(eng)
	eng.wireEventHandlers()

	// No BinUOP on the envelope — the blind path, which only an older Core's
	// delivered can take (a landing always carries Core's count).
	eng.Events.Emit(Event{Type: EventOrderDelivered, Payload: OrderDeliveredEvent{
		OrderID: orderID, OrderUUID: "uuid-blind-delivery", OrderType: orders.TypeComplex,
		ProcessNodeID: &nodeID, BinID: &deliveredBin,
	}})

	// Said, by name: the bin and the node, so the gap is findable afterwards.
	var named bool
	for _, a := range *alarms {
		if a.BinID != nil && *a.BinID == deliveredBin && a.CoreNodeName == "BLIND-NODE" {
			named = true
		}
	}
	if !named {
		t.Errorf("no delivered-not-bound alarm named bin %d at BLIND-NODE: a carrier with no count "+
			"bound nobody can grep for is a silent gap. alarms: %+v", deliveredBin, *alarms)
	}

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached == 200 {
		t.Errorf("RemainingUOP = %d: a delivery that carried no count must not seat a full carrier — "+
			"at 200 against a reorder point this node reads fed and replenishment is never asked for",
			rt.RemainingUOPCached)
	}
	// Nothing is bound on an unknown count (Lane H, H.7): the landing, or the
	// operator's count, binds it.
	if rt.ActiveBinID != nil && *rt.ActiveBinID == deliveredBin {
		t.Errorf("ActiveBinID = %d: a carrier with no count is not bound by the delivered", *rt.ActiveBinID)
	}
}
