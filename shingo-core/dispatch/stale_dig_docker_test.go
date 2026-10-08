//go:build docker

package dispatch

import (
	"encoding/json"
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
	"shingocore/store/reservations"
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
	grpType, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "db.GetNodeTypeByCode(\"NGRP\")")
	lanType, err := db.GetNodeTypeByCode("LANE")
	testutil.MustNoErr(t, err, "db.GetNodeTypeByCode(\"LANE\")")

	bp = &payloads.Payload{Code: prefix + "-P"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload")

	grp = &nodes.Node{Name: prefix + "-GRP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")

	testdb.MarkGroup(t, db, grp.ID)
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
	grpRaw, err := db.GetNode(grp.ID)
	grp = testutil.Must(t, grpRaw, err, "db.GetNode(grp.ID)")
	laneRaw, err := db.GetNode(lane.ID)
	lane = testutil.Must(t, laneRaw, err, "db.GetNode(lane.ID)")
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
	db := testDBShared(t)
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
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent, lane, slots, bp := planStaleDigFixture(t, db, d, "SD-WAIT")

	// The wall lands — but a robot is already coming for it.
	wall := testdb.CreateBinAtNode(t, db, bp.Code, slots[0].ID, "SD-WAIT-WALL")
	hauler := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "sd-wait-hauler"
		o.Status = protocol.StatusDispatched
	})
	// The dug-lane collision IS this test's subject, and reservations.Acquire now
	// refuses to create it — see ClaimBinInDugLaneForTest for why the state stays
	// reachable and therefore still worth an arm.
	testdb.ClaimBinInDugLaneForTest(t, db, wall.ID, hauler.ID)

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
	// GATE 1 (§R.91) INVERTED THIS ONE. It used to read: "a FAILED leg vetoes the
	// dissolve reading — something else went wrong in the same moment, and the
	// failure cascade is the honest disposition for that", asserting
	// digWasDissolved was FALSE for this set.
	//
	// There is no failure cascade for a dig leg any more. A leg that died and a
	// plan that went stale are the same fact about the demand — the dig did not
	// get through — so both close the chapter and both re-plan. The words differ
	// (chapterEndedInFailure picks them); the disposition does not.
	if !digWasDissolved([]*orders.Order{
		cancelled(reshuffleDissolveDetail),
		{Status: protocol.StatusFailed},
	}) {
		t.Error("a chapter with a FAILED leg in it has still ENDED. Vetoing the reading here is what " +
			"used to route it to the failure cascade, which took the demand out of the plant because " +
			"a robot broke down")
	}

	// AND A CONFIG-FAILED LEG IS THE ONE THAT STILL VETOES (§R.45). Nothing a
	// re-plan can do invents a node nobody configured, so there is no next
	// chapter to open and the demand has to say so and stop.
	if digWasDissolved([]*orders.Order{
		{Status: protocol.StatusFailed, ErrorDetail: configFailure("source node", "NOT-A-NODE")},
	}) {
		t.Error("a leg that could not be BUILT must not read as a chapter that can be re-planned — " +
			"the demand would loop forever against configuration nobody has fixed")
	}
	if digWasDissolved([]*orders.Order{{Status: protocol.StatusConfirmed}}) {
		t.Error("a cleanly finished set is not a dissolve")
	}
}

// TestStaleDig_DeadClaimantObstruction_Dissolves is the THIRD arm, and the one
// §R.98 stage C added: a hard claim is only a releaser while its claimant is
// still moving.
//
// `claimed_by != nil` was the whole test above, and it reads identically for a
// robot driving the bin out and for an order that stopped an hour ago. Measured
// on the stage-1 window: 63 refusals, 0 dissolves. `queue_releasers` declares
// this cause's releaser as "the bin in front is moved — by a dig, or by whoever
// claimed it carrying it out", so a dead claimant is a named wait whose releaser
// cannot exist, which §R.93.2's bar forbids in as many words.
//
// Two ways to be dead, one arm each, because they are different rows: a claimant
// that reached a terminal status, and a claimant that is nominally live and has
// not advanced inside the stall window.
//
// MUTATION (verified): drop the claimantIsMoving call and go back to
// `ClaimedBy != nil`. Both sub-tests park the dig on a wait forever and the
// "dissolved" assertion fires — the measured stage-1 wedge, restated.
func TestStaleDig_DeadClaimantObstruction_Dissolves(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		tag  string
		kill func(t *testing.T, db *store.DB, claimantID int64)
	}{
		{
			// The status is written directly, and that is the honest fixture rather
			// than a shortcut. TerminalizeOrder RELEASES the claim in the same
			// transaction, so the ordinary terminal path can never produce this row
			// — which is why the old `ClaimedBy != nil` test survived contact with
			// every well-behaved order. The state is nonetheless real: the engine
			// runs ReleaseOrphanedClaims on its sweep precisely because claims do
			// outlive terminal owners, and between the two the dig is looking at a
			// claim with nobody behind it.
			name: "claimant is terminal and its claim outlived it",
			tag:  "SD-DEAD1",
			kill: func(t *testing.T, db *store.DB, claimantID int64) {
				_, err := db.DB.Exec(`UPDATE orders SET status='failed' WHERE id=$1`, claimantID)
				testutil.MustNoErr(t, err, "kill the claimant")
			},
		},
		{
			name: "claimant is non-terminal and has stopped advancing",
			tag:  "SD-DEAD2",
			kill: func(t *testing.T, db *store.DB, claimantID int64) {
				stale := clock.Now().UTC().Add(-2 * claimantStallWindow)
				_, err := db.DB.Exec(`UPDATE orders SET updated_at=$2 WHERE id=$1`, claimantID, stale)
				testutil.MustNoErr(t, err, "stall the claimant")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testDBShared(t)
			d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
			parent, lane, slots, bp := planStaleDigFixture(t, db, d, tc.tag)

			// The wall lands and something claims it — so the lane looks, to the old
			// test, exactly like the self-clearing case above.
			wall := testdb.CreateBinAtNode(t, db, bp.Code, slots[0].ID, tc.tag+"-WALL")
			hauler := testdb.CreateOrder(t, db, func(o *orders.Order) {
				o.EdgeUUID = tc.tag + "-hauler"
				o.Status = protocol.StatusDispatched
			})
			// The dug-lane collision IS this test's subject, and reservations.Acquire now
			// refuses to create it — see ClaimBinInDugLaneForTest for why the state stays
			// reachable and therefore still worth an arm.
			testdb.ClaimBinInDugLaneForTest(t, db, wall.ID, hauler.ID)

			// PRECONDITION, or the assertion below is vacuous: while the claimant is
			// alive this must WAIT, so the dissolve that follows is attributable to
			// the claimant dying and not to the fixture never having had a claim.
			testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "redrive while the claimant is alive")
			alive, err := db.ListChildOrders(parent.ID)
			testutil.MustNoErr(t, err, "list legs while the claimant is alive")
			for _, l := range alive {
				if l.Status == protocol.StatusCancelled {
					t.Fatalf("leg %d was cancelled while a live robot was carrying the wall out", l.ID)
				}
			}

			tc.kill(t, db, hauler.ID)

			// The claim row is UNTOUCHED — this is the whole point. Nothing swept it,
			// nothing released it; the bin still reads `claimed_by = hauler`.
			held, err := db.GetBin(wall.ID)
			testutil.MustNoErr(t, err, "reload the wall")
			if held.ClaimedBy == nil || *held.ClaimedBy != hauler.ID {
				t.Fatalf("the wall's claim went away on its own (claimed_by=%v) — then this test is "+
					"about a released claim, not a dead one", held.ClaimedBy)
			}

			testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "redrive against the dead claimant")

			legs, err := db.ListChildOrders(parent.ID)
			testutil.MustNoErr(t, err, "list legs after")
			dissolved := false
			for _, l := range legs {
				if l.Status == protocol.StatusCancelled && l.ErrorDetail == reshuffleDissolveDetail {
					dissolved = true
				}
			}
			if !dissolved {
				t.Fatal("the dig held its wait against a claim nobody is carrying out — a named wait " +
					"whose releaser cannot exist is the bar's own violation, and it is what 63 " +
					"refusals and 0 dissolves looked like")
			}
			if d.laneLock.IsLocked(lane.ID) {
				t.Error("the lane is still locked after the dissolve — the re-plan refuses a locked lane")
			}

			testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "the cancel wiring's re-drive")
			after, err := db.GetOrder(parent.ID)
			testutil.MustNoErr(t, err, "reload parent")
			if protocol.IsTerminal(after.Status) {
				t.Fatalf("parent is %q — nothing here terminates a demand. The dig re-plans; the "+
					"demand keeps its need", after.Status)
			}
			if !protocol.IsAcquiring(after.Status) {
				t.Fatalf("parent is %q, want an acquiring status — the oracle only adapts if the "+
					"scanner looks at it again", after.Status)
			}
		})
	}
}

// TestStaleDig_GateOwnDig_DissolvesResummonsAndCompletes is window 2's
// composition on the gate path: one self-heal window composed with another, and
// followed to the demand closing.
//
// It replaces TestStaleDig_Window2Dig_DissolvesReplansAndCompletes, whose door
// (the held-bin re-check at pre-dispatch admission) no lane reaches now: every
// lane is marked, so a held bin buried after its hold was taken is found by the
// gate at the mark, and the order summons its OWN dig there, keeping `staged`
// and its lane lock. What this pins is the same promise through that door:
//
//   - the dig goes stale (a wall lands in front of its pending leg) and
//     DISSOLVES rather than waiting on a bin nothing will move;
//   - the order stays at its mark holding its bin, and the next pass summons a
//     fresh dig that includes the wall;
//   - that dig completes, the chapter closes, the tail is appended where the
//     robot stands, and the demand CONFIRMS with its bin at the line (F-07's
//     assertion: "not failed" was true of a demand that never closed);
//   - the ledger is clean: no lane lock, no occupants.
//
// MUTATION (run 2026-10-08): make handleStaleDigLeg hold the walled leg instead
// of dissolving (skip its dissolveCompound tail). The dissolve-marker assertion
// fires. The retired pin's closed-chapter mutation (compoundGenerations) does NOT
// fire here, and should not: on this path the demand closes by its own tail, not
// by the compound's completion arithmetic.
func TestStaleDig_GateOwnDig_DissolvesResummonsAndCompletes(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, lane, slots, bp := staleDigLane(t, db, "SD-GW")

	// [empty, blocker, blocker, HELD]: two blockers, so the dig's second leg is
	// still pending when the wall lands.
	testdb.CreateBinAtNode(t, db, bp.Code, slots[1].ID, "SD-GW-BLK1")
	testdb.CreateBinAtNode(t, db, bp.Code, slots[2].ID, "SD-GW-BLK2")
	held := testdb.CreateBinAtNode(t, db, bp.Code, slots[3].ID, "SD-GW-HELD")
	line := lineNode(t, db, "SD-GW-LINE")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "sd-gw-demand"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.SourceNode = slots[3].Name
		o.DeliveryNode = line.Name
		o.Status = protocol.StatusPending
	})
	testdb.ReserveBin(t, db, order.ID, held.ID)
	testutil.MustNoErr(t, db.UpdateOrderBinID(order.ID, held.ID), "stamp the held bin")
	order, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload the held-bin order")
	reload := func(what string) *orders.Order {
		o, err := db.GetOrder(order.ID)
		testutil.MustNoErr(t, err, "reload the demand: "+what)
		return o
	}

	// ── TO THE MARK, AND THE GATE FINDS IT BURIED ─────────────────────────
	_, err = d.DispatchHand(order, held.ID, slots[3], line)
	testutil.MustNoErr(t, err, "dispatch the held-bin order")
	if o := reload("at the mark"); !IsGateStaged(o) || o.QueueCause != string(CauseLaneTargetBuried) {
		t.Fatalf("the order is %s (staged=%v, cause %q), want it held at the mark as buried — the rest of "+
			"this test is about the dig the gate summons for it", o.Status, IsGateStaged(o), o.QueueCause)
	}
	d.EvaluateLaneReleases(lane.ID)
	legs := legsOf(t, db, order.ID)
	if len(legs) != 2 || legs[0].VendorOrderID == "" || legs[1].Status != protocol.StatusPending {
		t.Fatalf("the own dig is %+v, want two legs with the first sent and the second pending", legs)
	}
	if owner := d.laneLock.LockedBy(lane.ID); owner != order.ID {
		t.Fatalf("lane lock is held by %d, want the order digging its own lane (%d)", owner, order.ID)
	}

	// ── AND THEN IT GOES STALE ────────────────────────────────────────────
	// A wall lands at the mouth, in front of the pending leg's blocker. Nothing in
	// this dig will ever move it: its legs were written before it existed.
	testdb.CreateBinAtNode(t, db, bp.Code, slots[0].ID, "SD-GW-WALL")
	landLeg(t, d, db, legs[0])
	sawDissolveMarker := false
	for _, l := range legsOf(t, db, order.ID) {
		if l.Status == protocol.StatusCancelled && l.ErrorDetail == reshuffleDissolveDetail {
			sawDissolveMarker = true
		}
	}
	if !sawDissolveMarker {
		t.Fatal("no leg carries the dissolve marker — the walled leg is waiting on a bin nothing will move")
	}
	if o := reload("after the dissolve"); protocol.IsTerminal(o.Status) || !IsGateStaged(o) ||
		o.BinID == nil || *o.BinID != held.ID {
		t.Fatalf("after the dissolve the demand is %s (staged=%v, bin %v), want it still at its mark holding "+
			"bin %d: nothing is wrong with the demand, the lane changed", o.Status, IsGateStaged(o), o.BinID, held.ID)
	}

	// ── THE NEXT PASS SUMMONS A FRESH DIG, AND IT COMPLETES ───────────────
	d.EvaluateLaneReleases(lane.ID)
	for pass := 0; pass < 4 && IsGateStaged(reload("digging")); pass++ {
		for _, l := range legsOf(t, db, order.ID) {
			if !protocol.IsTerminal(l.Status) && l.VendorOrderID != "" {
				landLeg(t, d, db, l)
			}
		}
		testutil.MustNoErr(t, d.AdvanceCompoundOrder(order.ID), "close the dig's chapter")
		d.EvaluateLaneReleases(lane.ID)
	}
	released := reload("released")
	if IsGateStaged(released) || released.Status != protocol.StatusInTransit {
		t.Fatalf("the demand is %s (staged=%v, cause %q) after the second dig, want its tail appended and "+
			"the robot sent in for its bin", released.Status, IsGateStaged(released), released.QueueCause)
	}

	// ── AND THE DEMAND CLOSES ─────────────────────────────────────────────
	testutil.MustNoErr(t, db.MoveBinClearingStaging(held.ID, line.ID, false), "the bin reaches the line")
	_, err = db.TerminalizeOrder(order.ID, protocol.StatusConfirmed, "delivered")
	testutil.MustNoErr(t, err, "the demand completes")
	if done := reload("closed"); done.Status != protocol.StatusConfirmed {
		t.Fatalf("the demand is %q at the end, want %q", done.Status, protocol.StatusConfirmed)
	}
	if d.laneLock.IsLocked(lane.ID) {
		t.Error("the lane is still locked after the demand completed")
	}
	occRaw, err := reservations.OccupantsOf(db.DB, lane.ID)
	if occ := testutil.Must(t, occRaw, err, "reservations.OccupantsOf(db.DB, lane.ID)"); len(occ) != 0 {
		t.Errorf("lane %s still has occupants %v", lane.Name, occ)
	}
}

// TestStaleDig_TheOwnDigResumeReAsksTheDropBeforeItAppends is demo r2's hard
// burial (62666564; the §R.101 case): a store waiting at its mark named its slot
// before another order claimed the bin behind it; the gate found the slot walled
// and the store summoned its own dig; when the dig's chapter closed, the resume
// appended the tail it had been holding — the slot chosen before the claim — and
// put the bin in front of the claimed one. The resume now goes through the gate's
// own release (releaseAdmittedCandidate): the drop is re-asked against the lane as
// it stands, with the burial guard, before the tail is sent.
func TestStaleDig_TheOwnDigResumeReAsksTheDropBeforeItAppends(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcherWithResolver(t, db)
	_, lane, slots, bp := staleDigLane(t, db, "SD-RS")
	testdb.CreateBinAtNode(t, db, bp.Code, slots[0].ID, "SD-RS-BLK")
	claimed := testdb.CreateBinAtNode(t, db, bp.Code, slots[2].ID, "SD-RS-CLAIMED")
	press := lineNode(t, db, "SD-RS-PRESS")

	plan := []resolvedStep{
		{Action: protocol.ActionPickup, Node: press.Name},
		{Action: protocol.ActionWait, Node: "SD-RS-GRP-WAIT", WaitKind: WaitKindLane, WaitLane: lane.ID},
		{Action: protocol.ActionDropoff, Node: slots[1].Name, Group: "SD-RS-GRP"},
	}
	planJSON, err := json.Marshal(plan)
	testutil.MustNoErr(t, err, "marshal the store's plan")
	store := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "sd-rs-store"
		o.OrderType = OrderTypeComplex
		o.Coordinated = true
		o.PayloadCode = bp.Code
		o.SourceNode = press.Name
		o.DeliveryNode = slots[1].Name
		o.Status = protocol.StatusStaged
	})
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(store.ID, string(planJSON)), "persist the plan")
	testutil.MustNoErr(t, db.UpdateOrderVendor(store.ID, "SD-RS-V1", "WAITING", "AMR-1"), "set the vendor order")

	// Another order claims the bin behind the store's slot while it waits.
	collector := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "sd-rs-collector"
		o.Status = protocol.StatusDispatched
		o.SourceNode = slots[2].Name
	})
	testdb.ClaimBinForTest(t, db, claimed.ID, collector.ID)

	// The gate finds the slot walled and the store digs its own lane open.
	d.EvaluateLaneReleases(lane.ID)
	legs := legsOf(t, db, store.ID)
	if len(legs) == 0 {
		t.Fatal("fixture: the store summoned no dig — the resume under test never runs")
	}
	for _, l := range legs {
		if l.VendorOrderID != "" && !protocol.IsTerminal(l.Status) {
			landLeg(t, d, db, l)
		}
	}
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(store.ID), "close the dig's chapter")

	after, err := db.GetOrder(store.ID)
	testutil.MustNoErr(t, err, "reload the store")
	var steps []resolvedStep
	testutil.MustNoErr(t, json.Unmarshal([]byte(after.StepsJSON), &steps), "read the plan")
	drop := steps[len(steps)-1]
	if !IsGateStaged(after) && (drop.Node == slots[0].Name || drop.Node == slots[1].Name) {
		t.Fatalf("the resume sent the store to %s, in front of bin %d at %s, which order %d claimed while "+
			"the store waited: the tail it appended was chosen before the claim and never re-asked",
			drop.Node, claimed.ID, slots[2].Name, collector.ID)
	}
}
