package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// deliverLegWithSteps creates a complex order carrying the given steps against
// nodeID and emits its OrderDelivered with NO per-bin id — the F1b backstop's
// entry condition. Returns the alarms the handler raised.
func deliverLegWithSteps(t *testing.T, steps []protocol.ComplexOrderStep) []DeliveredNotBoundEvent {
	t.Helper()
	db := testEngineDB(t)
	nodeID, _, _ := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	_, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	alarms := captureDeliveredNotBound(eng)

	leg := mkSwapLeg(t, db, nodeID, "uuid-f1b-leg", steps, "")

	eng.Events.Emit(Event{Type: EventOrderDelivered, Payload: OrderDeliveredEvent{
		OrderID:       leg.ID,
		OrderUUID:     leg.UUID,
		OrderType:     protocol.OrderTypeComplex,
		ProcessNodeID: &nodeID,
		BinID:         nil,
	}})
	return *alarms
}

// TestF1bBackstop_PressIndexR1IsSilent is the false-alarm fix.
//
// A press-index R1 lifts the full tote off the press and sets its two carried
// bins down at OUT and at the index position — neither of them at the process
// node the order names. Core correctly ships BinID nil, and before this fix the
// backstop alarmed on it, telling the operator to "Record Count to bind" a bin
// that was never coming. Every produce swap the plant runs raised one.
func TestF1bBackstop_PressIndexR1IsSilent(t *testing.T) {
	t.Parallel()
	// wait(PRESS) pickup(PRESS) dropoff(MARKET) pickup(IN-STAGING) dropoff(INDEX-B)
	// — the builder's R1 shape. Nothing comes to rest at PRESS.
	r1 := []protocol.ComplexOrderStep{
		{Action: protocol.ActionWait, Node: "PRESS"},
		{Action: protocol.ActionPickup, Node: "PRESS"},
		{Action: protocol.ActionDropoff, Node: "MARKET"},
		{Action: protocol.ActionPickup, Node: "IN-STAGING"},
		{Action: protocol.ActionDropoff, Node: "INDEX-B"},
	}
	if alarms := deliverLegWithSteps(t, r1); len(alarms) != 0 {
		t.Fatalf("press-index R1 raised %d alarm(s), want 0 — the leg leaves no bin at PRESS, so a nil bin id is the plan working: %+v",
			len(alarms), alarms)
	}
}

// The other half, and the reason this cannot be a blanket silence: when the leg
// WAS going to leave a bin at the node and none resolved, that is the real
// SNF3-shaped gap the backstop exists for. It must still alarm.
func TestF1bBackstop_SupplyLegWithNoBinStillAlarms(t *testing.T) {
	t.Parallel()
	// wait(INDEX-B) pickup(INDEX-B) dropoff(PRESS) — the R2 supply shape. A bin
	// was due at PRESS and none resolved.
	r2 := []protocol.ComplexOrderStep{
		{Action: protocol.ActionWait, Node: "INDEX-B"},
		{Action: protocol.ActionPickup, Node: "INDEX-B"},
		{Action: protocol.ActionDropoff, Node: "PRESS"},
	}
	alarms := deliverLegWithSteps(t, r2)
	if len(alarms) != 1 {
		t.Fatalf("supply leg raised %d alarm(s), want 1 — a bin was due at PRESS and none resolved: %+v",
			len(alarms), alarms)
	}
	if alarms[0].CoreNodeName != "PRESS" {
		t.Errorf("alarm node = %q, want PRESS", alarms[0].CoreNodeName)
	}
}

// A 3-position R2 sets a bin on the press MID-sequence and then carries on to
// re-index the next position, so its LAST dropoff is not the press. Keying the
// silence on "where does the leg end" instead of "where does the bin come to
// rest" would swallow this one — and this is the leg that genuinely supplies
// the press.
func TestF1bBackstop_ThreePositionR2StillAlarms(t *testing.T) {
	t.Parallel()
	r2 := []protocol.ComplexOrderStep{
		{Action: protocol.ActionWait, Node: "INDEX-B"},
		{Action: protocol.ActionPickup, Node: "INDEX-B"},
		{Action: protocol.ActionDropoff, Node: "PRESS"},
		{Action: protocol.ActionPickup, Node: "INDEX-C"},
		{Action: protocol.ActionDropoff, Node: "INDEX-B"},
	}
	if alarms := deliverLegWithSteps(t, r2); len(alarms) != 1 {
		t.Fatalf("3-position R2 raised %d alarm(s), want 1 — it leaves a bin ON the press even though it ends at the index node: %+v",
			len(alarms), alarms)
	}
}

// A leg that sets a bin down at the press and takes it away again leaves
// nothing there, so it is silent — the "no LATER pickup" half of the
// predicate, pinned so a simplification to "has any dropoff here" is caught.
func TestF1bBackstop_DropoffThenPickupIsSilent(t *testing.T) {
	t.Parallel()
	steps := []protocol.ComplexOrderStep{
		{Action: protocol.ActionDropoff, Node: "PRESS"},
		{Action: protocol.ActionPickup, Node: "PRESS"},
		{Action: protocol.ActionDropoff, Node: "MARKET"},
	}
	if alarms := deliverLegWithSteps(t, steps); len(alarms) != 0 {
		t.Fatalf("a bin set down and taken away again leaves nothing at PRESS; got %d alarm(s): %+v", len(alarms), alarms)
	}
}

// Every uncertain answer keeps the alarm. An order with no steps at all is a
// SIMPLE order, where a missing bin id is exactly the gap the backstop was
// written for — a check that cannot tell whether a bin was due must not report
// "nothing to see".
func TestF1bBackstop_NoStepsStillAlarms(t *testing.T) {
	t.Parallel()
	if alarms := deliverLegWithSteps(t, nil); len(alarms) != 1 {
		t.Fatalf("an order with no steps raised %d alarm(s), want 1 — undecidable must not silence: %+v",
			len(alarms), alarms)
	}
}
