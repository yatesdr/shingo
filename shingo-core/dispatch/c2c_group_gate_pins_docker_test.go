//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// c2c_group_gate_pins_docker_test.go — C2c (Lane C), P3 of DESIGN-C2c §9: a
// waiter is keyed by its group and released by the event that changes the
// group's room (ruling 4). Fixture and gate pass: c2c_group_choice_pins.

// c2cPickout is the engine's PICKOUT subscriber (engine/wiring_lane_gate.go,
// EventBinEnteredTransit), replayed verbatim on the dispatcher. A dispatch test
// cannot import engine (engine imports dispatch), so this is the subscriber's
// body, not a direct call of a chosen lane. CONVERTED WITH C2c's RE-KEY (§6.3:
// one group pass per event): the node the bin left gives its lane and its
// group, the group is evaluated once, and the lane's held legs are re-driven.
// The base's body was the lane's own pass then the group's dweller fan-out. The
// engine-side run of the same event is the budget scenario
// TestBudget_C2cGroupGateEvent (engine/scanner_pass_budget_docker_test.go).
func c2cPickout(d *Dispatcher, fromNodeID int64) {
	lane, group := d.GateKeysForNode(fromNodeID)
	if group != 0 {
		d.EvaluateGroupReleases(group)
	}
	if lane != 0 {
		d.RedriveHeldCompoundLegs(lane)
	}
}

// TestC2cPin_AWaiterIsReleasedByABinLeavingAnotherLaneOfItsGroup is P3, the
// brief's third proof line. W stands at G's spot with its drop pencilled in A.
// A refuses it lane-occupied (another order's robot is inside A and stays
// there); B is full. Then B's mouth bin leaves. Ruling 4: the waiter is keyed
// by G and released by the event that changed G's room; ruling 3: A's refused
// mouth is the next candidate, not a wait, while the group has one. So W is
// released into B on that event, and the floor never has to free it.
//
// RED AT BASE, by reading: the pickout's group fan-out
// (EvaluateDwellersSharingGroupWith) does evaluate lane A, W is refused
// lane-occupied again, and the old widen arm (deleted with C2c) refused to re-aim it — the widen
// re-aims only lane-deeper-pending / lane-dig-active / lane-held-dig — so W
// stays at the spot until A's occupant leaves. D13 F4 (the flip of
// TestWiden_ATransientRefusalIsNotReAimed) is this pin's twin.
func TestC2cPin_AWaiterIsReleasedByABinLeavingAnotherLaneOfItsGroup(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	g := c2cTwoLaneGroup(t, db, "C2CP3", 2, 2)
	press, press2 := lineNode(t, db, "C2CP3-PRESS"), lineNode(t, db, "C2CP3-PRESS2")
	elsewhere := lineNode(t, db, "C2CP3-EMPTYSRC")
	away := lineNode(t, db, "C2CP3-AWAY")
	testdb.CreateBinAtNode(t, db, "", elsewhere.ID, "C2CP3-EMPTY")

	// A's occupant: a robot inside lane A that does not leave in this test.
	occupant := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = g.a[0].Name
		o.Status = "in_transit"
	})
	if ok, err := reservations.AcquireOccupancy(db.DB, occupant.ID, g.laneA.ID); err != nil || !ok {
		t.Fatalf("fixture: put a robot inside lane A: ok=%v err=%v", ok, err)
	}
	// B is full.
	mouthBin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, g.b[0].ID, "C2CP3-B0")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, g.b[1].ID, "C2CP3-B1")

	w := c2cAtSpot(t, db, "c2cp3-w", sd.Payload.Code, 0,
		c2cSwapPlan(g, press.Name, g.a[1].Name, elsewhere.Name, press2.Name, true))
	c2cPaper(t, db, w.ID, g.a[1])

	// THE REFUSAL REALLY IS lane-occupied, with nowhere else in G. If it were
	// something else this pin would be about a different arm.
	c2cGatePass(d, g)
	before, _ := c2cReload(t, db, w)
	if before.WaitIndex != 1 || before.QueueCause != string(CauseLaneOccupied) {
		t.Fatalf("fixture: W must wait at G's spot under %q with B full, got wait index %d cause %q",
			CauseLaneOccupied, before.WaitIndex, before.QueueCause)
	}

	// THE EVENT: B's mouth bin leaves the group.
	testutil.MustNoErr(t, db.MoveBinClearingStaging(mouthBin.ID, away.ID, false), "B's mouth bin leaves")
	c2cPickout(d, g.b[0].ID)

	after, steps := c2cReload(t, db, w)
	_, drop := c2cInteriorDrop(steps)
	if drop.Node != g.b[0].Name || after.WaitIndex < 2 {
		t.Fatalf("after B's mouth bin left, W's drop is at %q at wait index %d (cause %q), want it released "+
			"into %s on that event: a bin leaving any lane of W's group is the event that changed its room, "+
			"and A's refused mouth is the next candidate, not a wait (rulings 3 and 4)",
			drop.Node, after.WaitIndex, after.QueueCause, g.b[0].Name)
	}
	if n := floorRecords(t, db, w.ID); n != 0 {
		t.Errorf("the floor recorded %d release(s) of W; the event released it, the floor stays a "+
			"should-be-zero (ruling 4)", n)
	}
	if occ, err := reservations.OccupantsOf(db.DB, g.laneA.ID); err != nil || len(occ) != 1 || occ[0] != occupant.ID {
		t.Errorf("fixture: lane A's occupant should still be inside (%v, %v); W went round it, not through it",
			occ, err)
	}
}
