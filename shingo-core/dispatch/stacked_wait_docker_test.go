//go:build docker

package dispatch

import (
	"encoding/json"
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
	"shingocore/store/reservations"
)

// stacked_wait_docker_test.go — catalog row 5.5: a buried need with NO substitute,
// in a lane another dig owns, in a group with nowhere to park.
//
// Every one of those is pinned on its own. Window 4's dig arm has
// TestWindow4_NoSubstitute_TriggersTheDig; the lane-locked refusal has its
// classification pinned by TestPlanningError_TransientCodesAreNotSilentlyTerminal;
// the shuffle-slot shortage has TestFindShuffleSlots_TwoDigsMustNotShareASlot. What
// none of them asks is what happens when a demand meets all three IN SEQUENCE,
// which is the only arrangement that can actually kill it: each individual wait is
// correct, and a demand dies when the SECOND one is the arm somebody forgot.
//
// The stack, and the order matters because each stage is only reachable once the
// one before it clears:
//
//	1. the need is buried and irreplaceable   → the dig is asked for
//	2. the lane is locked by another dig      → lane-locked, wait
//	3. the lane frees, the group is full      → no-shuffle-slot, wait
//	4. a slot frees                           → the dig plans, runs, the parent resumes
//
// NEVER TERMINAL is asserted after every stage, not at the end. A demand that dies
// at stage 3 and is re-created by something else would satisfy an end-state check.

// stackedLane builds a group with one 3-deep lane and exactly ONE direct child, so
// the shuffle pool is a single node the test can open and close:
//
//	‹prefix›-GRP
//	├── LANE: S1 (the wall) · S2 (the held bin) · S3 (empty)
//	└── SPARE (the entire shuffle pool)
//
// The lane's own S3 is not a fallback: findShuffleSlots never parks a blocker back
// into the lane it is digging out. So occupying SPARE genuinely leaves the dig
// nowhere to go, which is stage 3.
func stackedLane(t *testing.T, db *store.DB, prefix string) (grp, lane *nodes.Node, slots []*nodes.Node, spare *nodes.Node, bp *payloads.Payload) {
	t.Helper()
	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp = &payloads.Payload{Code: prefix + "-P"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload")

	grp = &nodes.Node{Name: prefix + "-GRP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")

	lane = &nodes.Node{Name: prefix + "-LANE", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(lane), "create lane")
	for d := 1; d <= 3; d++ {
		at := d
		s := &nodes.Node{Name: fmt.Sprintf("%s-LANE-S%d", prefix, d), ParentID: &lane.ID, Enabled: true, Depth: &at}
		testutil.MustNoErr(t, db.CreateNode(s), "create slot")
		slots = append(slots, s)
	}
	spare = &nodes.Node{Name: prefix + "-SPARE", ParentID: &grp.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(spare), "create the shuffle pool")

	grp, _ = db.GetNode(grp.ID)
	lane, _ = db.GetNode(lane.ID)
	return grp, lane, slots, spare, bp
}

// TestStacked_BuriedIrreplaceableNeed_InALockedLane_InAFullGroup is row 5.5.
//
// The demand is a complex order holding the only bin of its payload, so the cheap
// correction window 4 prefers — shop a substitute — is unavailable by construction
// and every stage below is a genuine wait rather than a choice.
//
// ── THIS TEST FOUND A LIVE ONE, and stage 3 is it ─────────────────────────
//
// Both complex reshuffle arms mapped EVERY planning error to failOrderInternal,
// ErrNoShuffleSlot included. planning_service.planBuriedReshuffle has had that
// case split out as congestion since sim order 21 died of it on the 2026-07-10
// houseserver run ("cannot plan reshuffle: need 1 slot, 0 available"); the complex
// twins never got the same arm. So a COMPLEX demand — the changeover swaps, which
// are most of the plant's lane traffic — whose bin was buried at a moment when the
// group had no free parking was terminally failed for a condition that clears the
// second any other order releases a slot. The arm is added on this branch
// (complex_reshuffle.go, both twins) and stage 3 is its regression test.
//
// MUTATION 1 (verified): remove that ErrNoShuffleSlot arm from
// handleComplexBuriedOnReplay so a full group falls through to failOrderInternal
// again. Stage 3's non-terminal assertion fires with the order `failed`.
//
// MUTATION 2 (verified): delete the `if d.laneLock.IsLocked(buried.LaneID)` guard
// from handleComplexBuriedOnReplay. The order still waits — the full group refuses
// it one arm later — so nothing collides and no end-state check would notice. What
// breaks is the DIAGNOSIS: stage 2 parks under `no-shuffle-slot` instead of
// `lane-locked`, and the two clear on different events. Stage 2's cause assertion
// fires. That the two waits are indistinguishable unless somebody asserts the tag
// is the whole reason ReshuffleWaitError carries one.
func TestStacked_BuriedIrreplaceableNeed_InALockedLane_InAFullGroup(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, spare, bp := stackedLane(t, db, "STK")
	d := window4Dispatcher(t, db) // a real resolver: the substitute search must be able to run

	// The ONLY bin of this payload, at depth 2.
	held := testdb.CreateBinAtNode(t, db, bp.Code, slots[1].ID, "STK-HELD")
	// The wall, and the shuffle pool's occupant, are BOTH a different payload — so
	// neither can be mistaken for a substitute and stage 1 is honestly "no
	// substitute exists" rather than "the finder did not look".
	other := &payloads.Payload{Code: "STK-OTHER"}
	testutil.MustNoErr(t, db.CreatePayload(other), "create the other payload")
	wall := testdb.CreateBinAtNode(t, db, other.Code, slots[0].ID, "STK-WALL")
	poolFiller := testdb.CreateBinAtNode(t, db, other.Code, spare.ID, "STK-POOL-FILLER")
	line := lineNode(t, db, "STK-LINE")

	steps := []resolvedStep{{Action: protocol.ActionPickup, Node: slots[1].Name, Group: grp.Name}}
	stepsJSON, err := json.Marshal(steps)
	testutil.MustNoErr(t, err, "marshal the plan")
	// COORDINATED IS SET EXPLICITLY, and it is load-bearing rather than fixture
	// dressing: IsCoordinated reads that column and nothing else, and it is what
	// routes the compound's terminal arm to ResumeCompound instead of
	// CompleteCompound. A parent left unmarked would be CONFIRMED by its own dig —
	// reported as delivered having never picked its bin up.
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "stk-demand"
		o.OrderType = OrderTypeComplex
		o.Coordinated = true
		o.PayloadCode = bp.Code
		o.SourceNode = grp.Name
		o.DeliveryNode = line.Name
		o.StepsJSON = string(stepsJSON)
		o.Status = protocol.StatusQueued
	})
	testdb.ReserveBin(t, db, order.ID, held.ID)

	// stillAlive is the assertion this row exists for, so it is a closure called
	// after every stage rather than a block at the end.
	//
	// IT DOES NOT ASSERT THE BIN HOLD, and that is deliberate rather than an
	// omission: recalcBuriedNeed RELEASES the reservation before it re-resolves,
	// and does not re-take it on the dig arm ("a reservation held across an
	// excavation is a promise about a bin the dig may relocate"). Asserting the
	// hold survives would pin the opposite of the documented ruling. What must
	// survive the stack is the ORDER.
	stillAlive := func(stage string) {
		t.Helper()
		o, err := db.GetOrder(order.ID)
		testutil.MustNoErr(t, err, "reload the demand")
		if protocol.IsTerminal(o.Status) {
			t.Fatalf("%s: the demand is %q. Nothing here is a fault — a lane somebody else is digging "+
				"and a group with no free slot are both congestion, and demand never terminates for "+
				"congestion", stage, o.Status)
		}
	}

	// ── STAGE 1: buried, and nothing else will do ─────────────────────────────
	_, _, hold := d.widenSupplyPickups(order, steps)
	if hold == nil {
		t.Fatal("no hold returned — with no other bin of this payload anywhere, the order must ask for " +
			"a dig; otherwise it waits on a bin behind a wall with nothing coming to move it")
	}
	if MapFinderOutcome(*hold) != OutcomeReshuffle {
		t.Fatalf("outcome = %v, want OutcomeReshuffle", MapFinderOutcome(*hold))
	}
	if hold.Buried == nil || hold.Buried.Bin.ID != held.ID || hold.Buried.LaneID != lane.ID {
		t.Fatalf("the dig names %v, want bin %d in lane %d", hold.Buried, held.ID, lane.ID)
	}
	stillAlive("stage 1 (buried, no substitute)")

	// ── STAGE 2: and another dig owns the lane ────────────────────────────────
	digger := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "stk-foreign-dig" })
	if !d.laneLock.TryLock(lane.ID, digger.ID) {
		t.Fatal("TryLock on a free lane must succeed")
	}

	d.handleComplexBuriedOnReplay(order, hold.Buried)
	stillAlive("stage 2 (the lane is another dig's)")

	parked, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload after the locked-lane pass")
	if parked.QueueCause != string(CauseLaneLocked) {
		t.Errorf("queue_cause = %q, want %q — three reshuffle waits have three different releasers and "+
			"three different answers to 'should I go and look at it', and they used to arrive "+
			"indistinguishable", parked.QueueCause, CauseLaneLocked)
	}
	if kids, _ := db.ListChildOrders(order.ID); len(kids) != 0 {
		t.Fatalf("%d compound child/children were written against a lane this order does not own", len(kids))
	}
	// The CLASSIFICATION behind this park — codeLaneLocked being Transient() — is
	// pinned once for all three reshuffle waits by
	// TestPlanningError_TransientCodesAreNotSilentlyTerminal (wait_not_fail_test.go)
	// and is deliberately not restated here.

	// ── STAGE 3: the lane frees, and there is nowhere to park ─────────────────
	d.laneLock.Unlock(lane.ID, digger.ID)
	d.handleComplexBuriedOnReplay(order, hold.Buried)
	stillAlive("stage 3 (the group has no free shuffle slot)")

	starved, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload after the full-group pass")
	if starved.QueueCause != string(CauseNoShuffleSlot) {
		t.Errorf("queue_cause = %q, want %q — a crowded group and a busy lane clear on different "+
			"events, and only one of them is something an operator can act on",
			starved.QueueCause, CauseNoShuffleSlot)
	}
	if kids, _ := db.ListChildOrders(order.ID); len(kids) != 0 {
		t.Fatalf("%d compound child/children were written with nowhere to put a blocker", len(kids))
	}
	if d.laneLock.IsLocked(lane.ID) {
		t.Fatal("the lane was left locked by a pass that planned nothing — the next attempt would " +
			"refuse itself on a lock nothing alive will release")
	}

	// ── STAGE 4: A SLOT FREES, AND THE STACK UNWINDS ──────────────────────────
	testutil.MustNoErr(t, db.MoveBinClearingStaging(poolFiller.ID, line.ID, false), "somebody clears the spare")

	d.handleComplexBuriedOnReplay(order, hold.Buried)
	stillAlive("stage 4 (the dig is planned)")

	digging, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload once the dig is planned")
	// THE DEMAND TAKES THE DIG. This assertion has now been written both ways
	// round and both texts are worth keeping. Originally `reshuffling`, which was
	// how the old shape said "the dig started". Then, under the two-shape ruling:
	// "It is now a customer of one, so the excavation is proved by the dig
	// existing (below) and the demand is proved unconsumed by it still sitting in
	// the acquiring set", asserting protocol.StatusQueued.
	//
	// §R.91 rules the first way: every demand that creates a dig becomes its
	// parent. So `reshuffling` is once again how this test knows the excavation
	// started, and it is proved to be THIS demand's excavation by the leg count
	// below reading off the demand itself.
	if digging.Status != protocol.StatusReshuffling {
		t.Fatalf("the demand is %q, want %q — every obstacle has cleared, so it must now be digging",
			digging.Status, protocol.StatusReshuffling)
	}
	legs := legsOf(t, db, laneClearFor(t, db, digging).ID)
	// Expose mode: the unbury alone. The complex parent owns its own pickup and
	// runs it against the now-accessible slot after the compound.
	if len(legs) != 1 {
		t.Fatalf("the dig has %d leg(s), want exactly the wall's unbury — a complex parent's reshuffle "+
			"must not carry a retrieve, or the bin is delivered to the parent's LAST step's node", len(legs))
	}
	if legs[0].VendorOrderID == "" {
		t.Fatalf("the dig's leg never went out (queue_cause %q)", legs[0].QueueCause)
	}
	// WHERE THE WALL GOES, read when Core chooses it. The leg is dispatched with no
	// destination and dwells in the locked lane holding the wall bin; releasing it
	// is the moment the freed spare is picked, and it is the same fact one step
	// later. That the spare had to be FREED first — by the earlier eviction in this
	// scenario — is what the assertion is really about, and it is unchanged.
	digLeg := releaseDwell(t, d, db, legs[0])
	if digLeg.DeliveryNode != spare.Name {
		t.Errorf("the wall was released onto %s, want the freed spare %s", digLeg.DeliveryNode, spare.Name)
	}

	landLeg(t, d, db, digLeg)
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(order.ID), "close the dig")

	resumed, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload after the dig")
	if !protocol.IsAcquiring(resumed.Status) {
		t.Fatalf("the demand is %q after its dig finished, want an acquiring status — a coordinated "+
			"parent RESUMES, it does not complete, and one left outside {queued, sourcing} is one the "+
			"scanner never looks at again", resumed.Status)
	}
	stillAlive("after the dig completed")

	// AND THE NEED IS SERVABLE. The same recalculation that asked for the dig now
	// leaves the step exactly where it was — which is the only evidence that the
	// excavation was the RIGHT one rather than merely a successful compound.
	out, changed, after := d.widenSupplyPickups(resumed, steps)
	if after != nil {
		t.Fatalf("the demand is STILL holding (%v) after three waits and a dig. Every stage was "+
			"transient, so the stack unwinding has to end in service", after.QueueCause)
	}
	if changed || out[0].Node != slots[1].Name {
		t.Errorf("the step moved to %s — the held bin is reachable again and must be left alone",
			out[0].Node)
	}

	// The wall really did leave the lane; the order row agreeing is the plan, the
	// bin agreeing is the outcome.
	moved, err := db.GetBin(wall.ID)
	testutil.MustNoErr(t, err, "reload the wall")
	if moved.NodeID == nil || *moved.NodeID != spare.ID {
		t.Errorf("the wall is at node %v, want the spare %d", moved.NodeID, spare.ID)
	}

	// THE LANE STAYS HELD, and that is not a leak — but it is no longer held as a
	// DIG. The assertion used to be `IsLocked(lane.ID)`, under: "Expose mode
	// transfers the lock to the complex parent until the target bin leaves (the
	// re-burial window, catalog row 3.7), so an unlocked lane here would be the
	// bug, not the tidy outcome."
	//
	// The claim is unchanged and the MODE is not. Gate 2 (§R.91) converts the
	// parent's own dig row to its own OUTBOUND row at resume, which is the same
	// protection stated in the vocabulary that already exists: outbound excludes
	// a drop into the lane, which is the only way the uncovered bin can be
	// re-buried, and it shares with other outbound holders, who can only take
	// bins out. Asserting IsLocked here would now demand that a finished
	// excavation keep digging.
	holders, hErr := reservations.ActiveMouthRows(db.DB, lane.ID)
	testutil.MustNoErr(t, hErr, "read the lane's mouth holds after the dig")
	demandHolds := false
	for _, h := range holders {
		if h.OrderID == order.ID && h.Mode == reservations.ModeOutbound {
			demandHolds = true
		}
	}
	if !demandHolds {
		t.Errorf("lane %s holds %+v — the demand has not picked its bin yet, and without its own "+
			"outbound hold anything may store in front of it before it does", lane.Name, holders)
	}
}
