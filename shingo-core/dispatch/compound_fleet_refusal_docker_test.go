//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
)

// TestCompound_FleetRefusalParksTheLegAndKeepsTheDemand — a dig leg the fleet
// will not take must WAIT, not kill the line's demand.
//
// ── THE DEFECT ────────────────────────────────────────────────────────────
//
// dispatchToFleet called failOrder on ANY error from the fleet create. On the
// dig path that is a failed leg, and a failed leg fails its parent through the
// sibling cascade (HandleChildOrderFailure) — and the parent IS the demand. So a
// robot system that was busy for one second terminated a request for material,
// with no fault anywhere and nothing a human asked for.
//
// `51a97a56` had already drawn the line for the plain path ("a fleet that will
// not take an order right now is congestion, not a permanent failure") and fixed
// DispatchDirect. The compound caller went through the OTHER wrapper and kept
// the terminal disposition.
//
// ── WHY IT ASSERTS THE RECOVERY AND NOT THE PARK ──────────────────────────
//
// A test that ends at "it was not failed" is green while the leg sits claimed
// and unreachable forever, which is the more expensive half of this bug: the
// leg has already been CAS-claimed to `dispatched` before the fleet call, and
// nothing selects a `dispatched` leg with no vendor order. So the park is the
// setup; the assertion is that the dig goes out once the fleet is willing.
//
// ── MUTATIONS RUN (all three fire) ────────────────────────────────────────
//
//  1. restore `d.failOrder(order, env, "fleet_failed", err.Error())` inside
//     dispatchToFleet  → assertion (a): leg `failed`, parent `failed`.
//  2. drop the MoveToSourcing rollback in parkLegOnFleetRefusal
//     → assertion (b): leg stranded at `dispatched`, and (d) never dispatches.
//  3. revert GetNextChild to `status='pending'`
//     → assertion (d): the willing fleet never sees the leg; it stays parked.
func TestCompound_FleetRefusalParksTheLegAndKeepsTheDemand(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	parent, children, lane, _ := twoLegCompound(t, db, "FLEETREFUSE")

	// The fleet is refusing everything, which is what a disconnected or saturated
	// RDS looks like from Core.
	backend := testdb.NewFailingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	if err := d.AdvanceCompoundOrder(parent.ID); err != nil {
		t.Fatalf("advance (fleet refusing): %v", err)
	}

	leg, err := db.GetOrder(children[0].ID)
	if err != nil {
		t.Fatalf("get leg: %v", err)
	}
	par, err := db.GetOrder(parent.ID)
	if err != nil {
		t.Fatalf("get parent: %v", err)
	}

	// (a) THE DEMAND SURVIVES. This is the whole finding: neither the leg nor the
	// parent may be terminal because a robot system was busy.
	if protocol.IsTerminal(leg.Status) {
		t.Errorf("leg status = %s — a fleet refusal terminated a dig leg. Congestion in the robot "+
			"system is not a fault in the plan, and a terminal leg fails the parent, which is the "+
			"line's demand", leg.Status)
	}
	if protocol.IsTerminal(par.Status) {
		t.Fatalf("parent status = %s — the demand was killed by a fleet refusal. This is the "+
			"wait-not-fail rule broken on the path that can least afford it", par.Status)
	}

	// (b) THE CLAIM IS UNDONE. handoverToFleet CASes the leg to `dispatched`
	// before it calls the fleet and leaves it there for its caller to dispose of.
	// A leg left at `dispatched` with no vendor order is tracked by nothing,
	// selected by no re-drive, and swept to `cancelled` by AbandonStuckOrders an
	// hour later — which fails the parent anyway, just slower.
	if leg.Status == StatusDispatched {
		t.Errorf("leg is stranded at `dispatched` with vendor %q — the fleet never took it, so the "+
			"claim must be rolled back or nothing will ever look at this leg again", leg.VendorOrderID)
	}
	if leg.VendorOrderID != "" {
		t.Errorf("leg carries vendor order %q after a refused create", leg.VendorOrderID)
	}

	// (c) THE WAIT IS READABLE. An engineer grouping by queue_cause must see this
	// as the same condition the plain path already reports, not as a new one.
	if leg.QueueCause != string(CauseFleetRefusedCreate) {
		t.Errorf("queue_cause = %q, want %q — the row must say the fleet refused, in the same "+
			"spelling fulfillment writes for the plain path", leg.QueueCause, CauseFleetRefusedCreate)
	}
	if leg.QueueCode != string(protocol.QueueFleetUnavailable) {
		t.Errorf("queue_code = %q, want %q", leg.QueueCode, protocol.QueueFleetUnavailable)
	}

	// (d) THE FLEET BECOMES WILLING AND THE DIG GOES OUT. The releaser is the one
	// that already exists — a lane-clearing event re-drives held legs — and it can
	// only find this leg because the re-drive population is "not yet handed to the
	// fleet" (orders.AwaitingFleetSQL) rather than `status='pending'`.
	backend.SetFail(false)
	d.RedriveHeldCompoundLegs(lane)

	if !inFlight(t, db, children[0].ID) {
		after, _ := db.GetOrder(children[0].ID)
		t.Fatalf("the parked leg never went out after the fleet became willing — status %s, "+
			"vendor %q. A leg claimed but unsent is invisible to a re-drive keyed on `pending`",
			after.Status, after.VendorOrderID)
	}

	// The cause does not outlive the wait.
	out, err := db.GetOrder(children[0].ID)
	if err != nil {
		t.Fatalf("get dispatched leg: %v", err)
	}
	if out.QueueCode != "" || out.QueueCause != "" {
		t.Errorf("dispatched leg still carries queue_code=%q queue_cause=%q; want both cleared",
			out.QueueCode, out.QueueCause)
	}
}
