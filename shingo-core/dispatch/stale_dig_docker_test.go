//go:build docker

package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// stale_dig_docker_test.go — a dig's plan is written once, and the lane does not
// hold still.
//
// The shape (sim scenario 13b, the owner's two-flows-one-lane what-if): dig A
// packs a blocker into lane B while B's own dig is mid-flight. B's remaining leg
// then has a bin in front of it that is not in B's plan — because B's plan was
// written before it landed — and nothing in B's compound will ever move it. The
// leg parks on a reachability refusal that cannot self-clear, and the demand that
// would plan a dig for the new blocker is the parent, imprisoned in `reshuffling`
// inside this very compound.
//
// Occupancy and a foreign dig do not do this: somebody leaves, the redrive
// re-admits. Reachability is the one refusal whose cause is a FACT ABOUT THE PLAN
// rather than about the moment.
//
// THE WINDOW IS AFTER A LEG PLACES, and the fixtures below reproduce that rather
// than a simpler-looking state that would not exercise the code. While leg 1 is
// in flight its occupancy row refuses leg 2 first (admission asks occupancy
// before reachability), so a walled leg 2 reads as lane-occupied and self-clears.
// The stale case needs leg 1 finished — its blocker parked, its occupancy gone —
// and the wall landing in the gap before leg 2 is admitted.

// staleDigLane builds an ungated 4-deep lane in a group with parking beside it,
// and returns the lane plus its slots shallowest-first.
func staleDigLane(t *testing.T, db *store.DB, prefix string) (grp *nodes.Node, lane *nodes.Node, slots []*nodes.Node, bp *payloads.Payload) {
	t.Helper()
	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp = &payloads.Payload{Code: prefix + "-P"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload")

	grp = &nodes.Node{Name: prefix + "-GRP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")

	lane = &nodes.Node{Name: prefix + "-LANE", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(lane), "create lane")
	for d := 1; d <= 4; d++ {
		depth := d
		s := &nodes.Node{Name: fmt.Sprintf("%s-LANE-S%d", prefix, d), ParentID: &lane.ID, Enabled: true, Depth: &depth}
		testutil.MustNoErr(t, db.CreateNode(s), "create slot")
		slots = append(slots, s)
	}
	// Parking, well clear of the lane, so a dig is never refused for want of a
	// shuffle slot — the only refusal these tests want is reachability.
	for i := 1; i <= 4; i++ {
		p := &nodes.Node{Name: fmt.Sprintf("%s-PARK-%d", prefix, i), ParentID: &grp.ID, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(p), "create parking")
	}
	grp, _ = db.GetNode(grp.ID)
	lane, _ = db.GetNode(lane.ID)
	return grp, lane, slots, bp
}

// planStaleDigFixture sets a lane up as [empty, blocker, target], plans the dig
// against it, and then RUNS LEG 1 TO COMPLETION — the blocker lands in parking,
// the leg terminalizes, and its occupancy row goes with it.
//
// That is the state the stale case needs: the dig is live, holds its lane, and
// its next leg is one admission away from entering a corridor that is, for this
// instant, clear.
func planStaleDigFixture(t *testing.T, db *store.DB, d *Dispatcher, prefix string) (parent *orders.Order, lane *nodes.Node, slots []*nodes.Node, bp *payloads.Payload) {
	t.Helper()
	_, lane, slots, bp = staleDigLane(t, db, prefix)

	blocker := testdb.CreateBinAtNode(t, db, bp.Code, slots[1].ID, prefix+"-BLK")
	target := testdb.CreateBinAtNode(t, db, bp.Code, slots[2].ID, prefix+"-TGT")

	lineNode(t, db, prefix+"-LINE")
	parent = testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = prefix + "-parent"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = prefix + "-LINE"
		o.Status = protocol.StatusPending
	})

	if _, pe := d.planner.planBuriedReshuffle(parent, &BuriedError{Bin: target, Slot: slots[2], LaneID: lane.ID}); pe != nil {
		t.Fatalf("fixture: the dig must plan cleanly, got %s: %s", pe.Code, pe.Detail)
	}
	if !d.laneLock.IsLocked(lane.ID) {
		t.Fatal("fixture: the dig must hold its lane")
	}

	// Leg 1 does its job: the blocker leaves the lane for parking and the leg
	// terminalizes, which releases its occupancy (TerminalizeOrder →
	// reservations.ReleaseByOrder is kind-agnostic).
	legs, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "fixture: list legs")
	if len(legs) < 2 {
		t.Fatalf("fixture: the dig planned %d leg(s), want at least an unbury and a retrieve", len(legs))
	}
	first := legs[0]
	if first.VendorOrderID == "" {
		t.Fatalf("fixture: leg %d was never dispatched (queue_cause %q) — the lane was supposed to be "+
			"clear at plan time", first.ID, first.QueueCause)
	}
	testutil.MustNoErr(t, db.MoveBinClearingStaging(blocker.ID, parkingNodeID(t, db, prefix+"-PARK-1"), false),
		"fixture: leg 1 parks its blocker")
	_, err = db.TerminalizeOrder(first.ID, protocol.StatusConfirmed, "delivered")
	testutil.MustNoErr(t, err, "fixture: leg 1 completes")

	return parent, lane, slots, bp
}

// parkingNodeID resolves one of the group's parking nodes by name.
func parkingNodeID(t *testing.T, db *store.DB, name string) int64 {
	t.Helper()
	n, err := db.GetNodeByDotName(name)
	if err != nil || n == nil {
		t.Fatalf("resolve parking node %s: %v", name, err)
	}
	return n.ID
}

// TestStaleDig_UnclaimedObstruction_DissolvesAndReplans is 13b, and its checker
// is the campaign's: the demand survives with zero human intervention.
//
// MUTATION (verified): make handleStaleDigLeg fall through to the ordinary
// setQueueReason hold instead of dissolving. No leg is cancelled, the parent
// never leaves `reshuffling`, and the "back in the acquiring set" assertion fires
// — which is the wedge, restated as a failing test.
func TestStaleDig_UnclaimedObstruction_DissolvesAndReplans(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent, lane, slots, bp := planStaleDigFixture(t, db, d, "SD-DISS")

	// DIG A PACKS INTO THE MOUTH. Nothing in this compound will move it: the
	// blocker list was written before it existed.
	testdb.CreateBinAtNode(t, db, bp.Code, slots[0].ID, "SD-DISS-WALL")

	// Redrive one: what the lane-clearing event fires. The leg is walled, nobody
	// is coming for the wall, so the dig dissolves — legs cancelled, lane
	// released. The parent is deliberately NOT transitioned here; see below.
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "redrive the walled compound")

	legs, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list legs after dissolve")
	sawDissolveMarker := false
	for _, l := range legs {
		if !protocol.IsTerminal(l.Status) {
			t.Errorf("leg %d is %q — a dissolve cancels the whole remaining set, or the re-plan races "+
				"a robot still executing the old plan", l.ID, l.Status)
		}
		if l.Status == protocol.StatusCancelled {
			if l.ErrorDetail != reshuffleDissolveDetail {
				t.Errorf("leg %d cancelled with %q, want the dissolve marker — without it the terminal "+
					"arm reads this as a leg going wrong and fails the parent", l.ID, l.ErrorDetail)
			}
			sawDissolveMarker = true
		}
	}
	if !sawDissolveMarker {
		t.Fatal("no leg was cancelled — the dig was not dissolved, so the walled leg is still waiting " +
			"on a bin nothing will ever move")
	}

	// THE LANE IS FREE. IsLocked is owner-blind, so a lock held past the dissolve
	// parks the re-plan on lane_locked with nothing alive to release it.
	if d.laneLock.IsLocked(lane.ID) {
		t.Fatal("the lane is still locked after the dissolve — planBuriedReshuffle refuses a locked " +
			"lane whoever holds it, including the parent about to re-plan, so this is the wedge rebuilt")
	}

	// Redrive two: what the engine's cancel wiring does on its own goroutine
	// (wiring.go, EventOrderCancelled → AdvanceCompoundOrder). It is a separate
	// call rather than part of the dissolve BECAUSE the parent transition fires
	// the synchronous scanner, and the dissolve is reachable from inside it.
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "the cancel wiring's re-drive")

	after, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload parent")
	if protocol.IsTerminal(after.Status) {
		t.Fatalf("parent is %q — the demand was terminated because its dig's plan went stale. "+
			"Nothing is wrong with the demand; the lane changed", after.Status)
	}
	if !protocol.IsAcquiring(after.Status) {
		t.Fatalf("parent is %q, want an acquiring status — `reshuffling` is outside IsAcquiring, so a "+
			"parent left there is one the scanner never looks at again", after.Status)
	}

	// AND THE RE-PLAN SEES THE NEW WORLD. The same planner, run again, plans
	// against the lane as it now stands — which is the whole point: the new plan
	// contains the bin that made the old one stale.
	target, err := db.GetBinByLabel("SD-DISS-TGT")
	testutil.MustNoErr(t, err, "reload target")
	if _, pe := d.planner.planBuriedReshuffle(after, &BuriedError{Bin: target, Slot: slots[2], LaneID: lane.ID}); pe != nil {
		t.Fatalf("the re-plan was refused (%s: %s) — the demand survived the dissolve and then could "+
			"not be served, which is the wedge one step further along", pe.Code, pe.Detail)
	}
	replanned, err := db.ListChildOrders(after.ID)
	testutil.MustNoErr(t, err, "list re-planned legs")
	live := 0
	for _, l := range replanned {
		if !protocol.IsTerminal(l.Status) {
			live++
		}
	}
	// The wall at depth 1 plus the retrieve of the target at depth 3. Depth 2 was
	// emptied by the first dig's leg 1 and stays empty — blockers lie where they
	// fall, and the re-plan simply sees fewer of them than the first plan did.
	if live < 2 {
		t.Errorf("the re-plan has %d live leg(s); it must cover the bin dig A landed AND the retrieve. "+
			"A plan that still misses the new bin goes stale again immediately", live)
	}
}

// TestStaleDig_HardClaimedObstruction_Waits is the other arm of the ruling, and
// the one that keeps the fix from thrashing.
//
// A bin with a hard claim has a robot on its way to it. The lane frees itself,
// the existing lane-clearing redrive re-admits the leg, and dissolving would
// throw away a good plan to re-plan a dig for a bin that is seconds from gone.
// Dissolve is never triggered BY a claim — only by the absence of anyone coming.
//
// MUTATION (verified): drop the `if claimed` arm from handleStaleDigLeg so every
// reachability refusal dissolves. The "legs survive" assertion fires — the dig is
// torn down while a robot is already carrying the obstruction out.
func TestStaleDig_HardClaimedObstruction_Waits(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent, lane, slots, bp := planStaleDigFixture(t, db, d, "SD-WAIT")

	// The wall lands — but a robot is already coming for it.
	wall := testdb.CreateBinAtNode(t, db, bp.Code, slots[0].ID, "SD-WAIT-WALL")
	hauler := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "sd-wait-hauler"
		o.Status = protocol.StatusDispatched
	})
	testdb.ClaimBinForTest(t, db, wall.ID, hauler.ID)

	before, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list legs before")

	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "redrive the walled compound")

	// THE DIG SURVIVES.
	after, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload parent")
	if after.Status != protocol.StatusReshuffling {
		t.Fatalf("parent is %q, want %q — the obstruction is leaving under its own power, so the dig "+
			"waits for it rather than re-planning around it", after.Status, protocol.StatusReshuffling)
	}
	legs, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list legs after")
	if len(legs) != len(before) {
		t.Fatalf("leg count changed %d → %d", len(before), len(legs))
	}
	for _, l := range legs {
		if l.Status == protocol.StatusCancelled {
			t.Errorf("leg %d was cancelled — the dig was torn down over a bin a robot is already "+
				"carrying out", l.ID)
		}
	}
	if !d.laneLock.IsLocked(lane.ID) {
		t.Error("the lane was released while the dig is still live — another flow can now pack into it")
	}

	// AND THEN IT PROCEEDS. The obstruction leaves the lane exactly as the robot
	// carrying it would take it out; the next redrive admits the held leg. Without
	// this half the "wait" arm is indistinguishable from a stall.
	testutil.MustNoErr(t, db.MoveBinClearingStaging(wall.ID, parkingNodeID(t, db, "SD-WAIT-PARK-2"), false),
		"the hauler takes the wall out")
	_, err = db.TerminalizeOrder(hauler.ID, protocol.StatusConfirmed, "delivered")
	testutil.MustNoErr(t, err, "hauler completes")

	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "redrive after the lane cleared")

	resumed, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list legs after the clear")
	held := 0
	for _, l := range resumed {
		if l.Status == protocol.StatusPending {
			held++
		}
	}
	if held != 0 {
		t.Errorf("%d leg(s) still pending after the obstruction left — the wait had no releaser, "+
			"which makes it a stall wearing a queue reason", held)
	}
}

// TestStaleDig_DissolveMarkerIsWhatSeparatesItFromAFailure is the discriminator
// under test on its own, because the two outcomes are one string apart and the
// wrong one terminates demand.
func TestStaleDig_DissolveMarkerIsWhatSeparatesItFromAFailure(t *testing.T) {
	t.Parallel()
	cancelled := func(detail string) *orders.Order {
		return &orders.Order{Status: protocol.StatusCancelled, ErrorDetail: detail}
	}

	if !digWasDissolved([]*orders.Order{cancelled(reshuffleDissolveDetail)}) {
		t.Error("a set cancelled by the dissolve must read as dissolved")
	}
	if digWasDissolved([]*orders.Order{cancelled("operator terminated it")}) {
		t.Error("an operator cancel must NOT read as a dissolve — the compound really did stop, and " +
			"returning its parent to the acquiring set would resurrect work a human cancelled")
	}
	if digWasDissolved([]*orders.Order{
		cancelled(reshuffleDissolveDetail),
		{Status: protocol.StatusFailed},
	}) {
		t.Error("a FAILED leg vetoes the dissolve reading — something else went wrong in the same " +
			"moment, and the failure cascade is the honest disposition for that")
	}
	if digWasDissolved([]*orders.Order{{Status: protocol.StatusConfirmed}}) {
		t.Error("a cleanly finished set is not a dissolve")
	}
}
