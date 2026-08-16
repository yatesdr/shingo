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

// ── WINDOW 3, THE F-11 SCENARIO, END TO END ───────────────────────────────
//
// F-11, from the lane-stress rig on 2026-08-09: three robots dwelling at LS_D2's
// mark for 77 minutes, aimed at depths 2, 3 and 4 of a lane whose depth-1 slot
// held BIN-ACT-P2B with `claimed_by = NULL`. No mouth hold, no occupancy row, no
// dig on the lane, and the walling bin unclaimed — so, in the finding's words,
// "no releaser exists in principle". Fourteen free slots behind a wall nobody was
// going to move, and three robots parked in front of it indefinitely.
//
// These tests build that lane and assert that the plant fixes itself.

// healLaneFixture builds the F-11 geometry.
//
//	GRP-HEAL
//	├── HEAL-WALL (MARKED)   W0 depth 0  <- the unclaimed bin nobody wants
//	│                        W1 depth 1  <- the dweller's slot, EMPTY and unreachable
//	│                        W2 depth 2  <- the deeper store that puts it at the mark
//	└── HEAL-PARK (unmarked) P0, P1      <- somewhere for the blocker to go
//
// HEAL-PARK is deliberately UNMARKED: a dig out of a marked lane may not park a
// blocker in a different marked lane (findShuffleSlots, the F-03 fix), so a marked
// park lane would make this test about that exclusion instead of about the heal.
func healLaneFixture(t *testing.T, db *store.DB, name string) (wall, park *nodes.Node, w, p []*nodes.Node, bp *payloads.Payload) {
	t.Helper()
	grpType, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "NGRP type")
	lanType, err := db.GetNodeTypeByCode("LANE")
	testutil.MustNoErr(t, err, "LANE type")

	bp = &payloads.Payload{Code: name + "P"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload")

	grp := &nodes.Node{Name: name + "-GRP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")

	mkLane := func(lname, mark string, depth int) (*nodes.Node, []*nodes.Node) {
		lane := &nodes.Node{Name: lname, NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
		testutil.MustNoErr(t, db.CreateNode(lane), "create "+lname)
		if mark != "" {
			testutil.MustNoErr(t, db.SetNodeProperty(lane.ID, PropLaneGatePoint, mark), "mark "+lname)
		}
		var slots []*nodes.Node
		for i := 0; i < depth; i++ {
			d := i
			s := &nodes.Node{Name: fmt.Sprintf("%s-S%d", lname, i), ParentID: &lane.ID, Enabled: true, Depth: &d}
			testutil.MustNoErr(t, db.CreateNode(s), "create slot")
			slots = append(slots, s)
		}
		reloaded, _ := db.GetNode(lane.ID)
		return reloaded, slots
	}
	wall, w = mkLane(name+"-WALL", name+"-WALL-WAIT", 3)
	park, p = mkLane(name+"-PARK", "", 2)
	return wall, park, w, p, bp
}

// digParentFor returns the compound parent whose children move bins out of this
// lane, or nil. It looks for the thing the heal creates rather than for a
// particular id, because "a dig now exists that did not before" is the claim.
func digParentFor(t *testing.T, db *store.DB, sourceSlot string) *orders.Order {
	t.Helper()
	var parentID int64
	err := db.DB.QueryRow(
		`SELECT parent_order_id FROM orders
		  WHERE source_node = $1 AND parent_order_id IS NOT NULL
		  ORDER BY id DESC LIMIT 1`, sourceSlot).Scan(&parentID)
	if err != nil {
		return nil
	}
	parent, err := db.GetOrder(parentID)
	testutil.MustNoErr(t, err, "load dig parent")
	return parent
}

// TestWindow3_UnclaimedMouthBinIsDugOutWithNobodyAsking is F-11's fix.
//
// THE SETUP IS THE FINDING. A store is put at the mark the way F-11's three were
// — by a DEEPER store holding the lane's mouth, which is Tier 2 doing exactly its
// job. Then the deeper store places and drops its mouth row, so every ORDERING
// reason to hold the dweller is gone and the only thing left between it and its
// empty slot is a bin sitting in the corridor that nothing in the plant has any
// plan for. That is the state F-11 photographed, and before this change it was
// terminal in every sense but the status column: the evaluator re-derived the same
// refusal on every firing, forever, and the lane's free slots stayed unreachable.
//
// WHAT THE TEST DOES, AND THE LIST IS SHORT ON PURPOSE. It calls
// EvaluateLaneReleases once — the same call the event wiring makes on any lane
// event — and then, later, it plays the ROBOT (moves the bin, confirms the legs).
// It never asks for a dig, never re-drives the gate after the dig, and never
// touches the dweller. Everything else in the assertion list has to arrive on its
// own. "Zero human intervention" is the requirement, so the absence of those calls
// is as much the test as the assertions are.
//
// MUTATION (run 2026-08-10): comment out the two `propose(c)` calls in
// evaluateLaneReleasesPass. The dig is never created, the dweller is still
// gate-staged at the end of the run with wait_index 0, and the lane still holds
// its wall — the 77-minute dwell, reproduced in a second.
//
// SECOND MUTATION (run 2026-08-10): keep the trigger, remove the
// EvaluateLaneReleases call from unlockLaneForCompound. The dig fires, claims,
// runs and clears the lane — and the dweller is STILL dwelling at the end, because
// every lane event the dig emitted was consumed while the dig's own lock was still
// held. That arm is why the unlock had to become a trigger.
func TestWindow3_UnclaimedMouthBinIsDugOutWithNobodyAsking(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	wall, park, w, _, bp := healLaneFixture(t, db, "HL1")
	line := lineNode(t, db, "HL1-LINE")

	// The deeper store: dispatched, holding its inbound mouth row, not yet placed.
	// This is what puts the dweller at the mark in the first place (Tier 2).
	deep := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = w[2].Name
		o.Status = "in_transit"
	})
	if adm, _, _, err := d.AcquireLanesForOrder(deep, line, w[2], EntryFreshBin); err != nil || !adm {
		t.Fatalf("the deeper store must take its mouth row: adm=%v err=%v", adm, err)
	}
	testutil.MustNoErr(t, db.UpdateOrderVendor(deep.ID, "hl1-deep", "RUNNING", ""), "deep vendor")

	dweller := stageGatedStore(t, db, d, line, w[1], nil)
	if !IsGateStaged(dweller) {
		t.Fatalf("the dweller must be gate-staged behind the deeper store "+
			"(wait_index=%d vendor=%q) — the fixture is not reproducing F-11",
			dweller.WaitIndex, dweller.VendorOrderID)
	}
	markStaged(t, db, dweller.ID)

	// THE WALL ARRIVES. Unclaimed, in the mouth, wanted by nobody.
	blocker := createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-HL1-WALL")
	if blocker.ClaimedBy != nil {
		t.Fatalf("fixture bug: the walling bin must be UNCLAIMED, or this is a different finding")
	}

	// The deeper store places. Every ORDERING reason to hold the dweller is now
	// gone; only the physical wall is left.
	d.ReleaseInboundLaneForOrder(deep.ID, w[2].Name)

	if d.laneLock.IsLocked(wall.ID) {
		t.Fatal("precondition: no dig may hold the lane before the heal")
	}
	if n := appendsTo(backend, dweller.VendorOrderID); n != 0 {
		t.Fatalf("the dweller already has %d tail append(s) — it is not actually dwelling", n)
	}

	// ── THE ONE CALL. Any lane event would make it; the wiring makes it. ──
	d.EvaluateLaneReleases(wall.ID)

	// 1. A DIG NOW EXISTS, and nobody asked for it.
	parent := digParentFor(t, db, w[0].Name)
	if parent == nil {
		t.Fatalf("no dig was created for %s. A robot is dwelling at the mark aimed at %s, which is "+
			"EMPTY; %s holds unclaimed bin %d walling it; no order names that bin and no dig is "+
			"planned. Nothing else in the plant will ever move it — this is F-11, and the dweller "+
			"waits forever.", wall.Name, w[1].Name, w[0].Name, blocker.ID)
	}
	if parent.Status != StatusReshuffling {
		t.Errorf("dig parent %d is %s, want %s — it must go through the ordinary compound door",
			parent.ID, parent.Status, StatusReshuffling)
	}
	if parent.Coordinated {
		t.Error("the heal parent must be PLAIN: a coordinated parent routes to ResumeCompound and " +
			"would come back wanting work of its own, and clearing the corridor was the whole job")
	}

	// 2. THE DIG CLAIMED THE BLOCKER AT CREATION — the owner's requirement.
	claimed, err := db.GetBin(blocker.ID)
	testutil.MustNoErr(t, err, "reload blocker")
	if claimed.ClaimedBy == nil {
		t.Fatal("the dig did not claim the blocker. 'The dig should claim it on the heal — isn't " +
			"that the whole point': an unclaimed blocker can be stolen by any other planner " +
			"between now and the leg running, and the dig would arrive at an empty slot")
	}
	children, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list dig legs")
	if len(children) != 1 {
		t.Fatalf("dig has %d legs, want 1 (one blocker in the way)", len(children))
	}
	leg := children[0]
	if *claimed.ClaimedBy != leg.ID {
		t.Errorf("blocker is claimed by order %d, want the dig's own leg %d", *claimed.ClaimedBy, leg.ID)
	}
	if leg.SourceNode != w[0].Name {
		t.Errorf("the dig leg picks from %q, want the walling slot %q", leg.SourceNode, w[0].Name)
	}
	// WHERE IT PARKS THE BLOCKER, read after the release rather than after the
	// plan: the leg dwells in the walled lane holding the blocker until Core
	// chooses a slot (the outbound dwell), so a plan-time read finds nothing bound.
	// The property is the same one — the blocker goes to the ungated parking — and
	// so is the exclusion enforcing it.
	leg = releaseDwell(t, d, db, leg)
	destNode, err := db.GetNodeByDotName(leg.DeliveryNode)
	testutil.MustNoErr(t, err, "resolve the leg's dropoff")
	if destNode == nil || destNode.ParentID == nil || *destNode.ParentID != park.ID {
		t.Errorf("the dig parks the blocker at %q, want a slot in the ungated %s",
			leg.DeliveryNode, park.Name)
	}

	// 3. THE DWELLER WAS NOT TOUCHED. The heal is about the lane; the robot at the
	// mark keeps dwelling, which is what a gate wait is for, and it must not have
	// been sealed into a corridor that is still walled.
	stillWaiting, err := db.GetOrder(dweller.ID)
	testutil.MustNoErr(t, err, "reload dweller")
	if !IsGateStaged(stillWaiting) {
		t.Error("the dweller must still be gate-staged while the dig runs")
	}
	if n := appendsTo(backend, dweller.VendorOrderID); n != 0 {
		t.Errorf("the dweller got %d tail append(s) while the lane was still walled — "+
			"nothing may be sealed into a corridor it cannot drive down", n)
	}

	// ── THE ROBOT DOES THE DIG. The fleet's half, and the only half a test has to
	// play: the bin arrives, the leg ends. Everything after this line has to happen
	// by itself. ──
	robotRunsDigLeg(t, db, leg, destNode.ID)
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "advance the finished dig")

	// 4. THE DWELLER IS RELEASED, WITH NO FURTHER PROMPTING. No second
	// EvaluateLaneReleases here on purpose: the dig's completion has to be its own
	// releaser, or the heal is a dig that fixes a lane nobody is told about.
	freed, err := db.GetOrder(dweller.ID)
	testutil.MustNoErr(t, err, "reload dweller after the dig")
	if IsGateStaged(freed) {
		t.Fatalf("the lane is clear and the dweller is STILL at the mark (wait_index=%d, status=%s). "+
			"The dig ran, so the wall is gone — but every lane event it emitted was consumed while "+
			"its own lane lock was still held, and nothing re-asked after the lock dropped.",
			freed.WaitIndex, freed.Status)
	}
	if n := appendsTo(backend, dweller.VendorOrderID); n != 1 {
		t.Fatalf("the dweller got %d tail append(s), want exactly 1", n)
	}
	for _, c := range backend.ReleaseCalls() {
		if c.VendorOrderID == dweller.VendorOrderID && !c.Complete {
			t.Error("the dweller's tail must SEAL it")
		}
	}
	if d.laneLock.IsLocked(wall.ID) {
		t.Error("the heal dig must not leave its lane lock behind")
	}
	// The parent has no demand of its own to resume — clearing the corridor WAS the
	// work — so it confirms rather than requeueing into a scanner that has nothing
	// to plan for it.
	done, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload dig parent")
	if done.Status != StatusConfirmed {
		t.Errorf("heal parent finished as %s, want %s", done.Status, StatusConfirmed)
	}
}

// TestWindow3_ClaimedBlockerWaitsInsteadOfDigging is the law that keeps the heal
// from being a bulldozer, and it is the arm that makes the trigger safe to fire on
// every refusal.
//
// A HARD CLAIM IS A ROBOT IN MOTION. `claimed_by` is written immediately before
// the fleet call and cleared on arrival, so a claimed blocker is a bin that is
// already leaving — somebody IS coming for it. Digging it out would send a second
// robot to a bin a first robot is on its way to, which is the collision the claim
// exists to prevent, and it would do so in the name of self-healing.
//
// The distinction is the same one the burial guard draws (hard claims only, soft
// holds recalculate), and it is deliberately not re-derived here: the compound
// transaction refuses a foreign-claimed bin on its own (store.ErrBlockerClaimed).
// This test pins the earlier read, which exists so the common case does not mint
// an order it would immediately cancel.
//
// MUTATION (run 2026-08-10, and the FIRST FORM OF THIS TEST DID NOT CATCH IT —
// worth recording, because the reason is the interesting part). Dropping the
// `binIsUnclaimed` arm from mouthHealNeeded leaves the test green if it only
// asserts "no dig ran": the compound transaction refuses the foreign-claimed bin
// on its own and rolls the whole thing back, so no child, no lane lock, no dig.
// The deeper law holds without the pre-check — which is exactly what makes the
// pre-check a pre-check rather than the guard.
//
// What the mutation DOES produce is a heal PARENT minted and then cancelled on
// every firing, at lane-event rate, for a lane that was never stuck. So the
// assertion is on the parent row, not on the dig, and with it the mutation fires.
func TestWindow3_ClaimedBlockerWaitsInsteadOfDigging(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	wall, _, w, _, bp := healLaneFixture(t, db, "HL2")
	line := lineNode(t, db, "HL2-LINE")

	deep := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = w[2].Name
		o.Status = "in_transit"
	})
	if adm, _, _, err := d.AcquireLanesForOrder(deep, line, w[2], EntryFreshBin); err != nil || !adm {
		t.Fatalf("the deeper store must take its mouth row: adm=%v err=%v", adm, err)
	}
	testutil.MustNoErr(t, db.UpdateOrderVendor(deep.ID, "hl2-deep", "RUNNING", ""), "deep vendor")

	dweller := stageGatedStore(t, db, d, line, w[1], nil)
	if !IsGateStaged(dweller) {
		t.Fatalf("the dweller must be gate-staged (wait_index=%d)", dweller.WaitIndex)
	}
	markStaged(t, db, dweller.ID)

	// Same wall — but a retrieve is already on its way to this bin.
	blocker := createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-HL2-WALL")
	carrier := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.SourceNode = w[0].Name
		o.Status = "in_transit"
	})
	testutil.MustNoErr(t, execAt(db, `UPDATE bins SET claimed_by=$1 WHERE id=$2`, carrier.ID, blocker.ID),
		"claim the blocker for the carrier")

	d.ReleaseInboundLaneForOrder(deep.ID, w[2].Name)
	d.EvaluateLaneReleases(wall.ID)

	if parent := digParentFor(t, db, w[0].Name); parent != nil {
		t.Fatalf("a heal dig (%d) was created against bin %d, which order %d is already driving to. "+
			"A hard claim means the bin is leaving on its own; the correct disposition is the wait "+
			"the floor already has.", parent.ID, blocker.ID, carrier.ID)
	}
	// AND NOT EVEN A PARENT. The compound transaction would have refused this dig
	// anyway (store.ErrBlockerClaimed), so the assertion above passes with or
	// without the pre-check — it is the ORDER ROW that tells them apart. Minting a
	// parent per lane event and cancelling it again is churn with a paper trail,
	// on a lane whose wait was correct all along.
	if n := healParentsMinted(t, db, wall.Name); n != 0 {
		t.Errorf("%d heal parent(s) were minted and abandoned for %s. The blocker is hard-claimed, "+
			"so the compound was always going to be refused; asking one read earlier is what keeps "+
			"the evaluator from writing an order per firing to find that out.", n, wall.Name)
	}
	if d.laneLock.IsLocked(wall.ID) {
		t.Error("no dig was warranted, so no lane lock may have been taken")
	}
	still, err := db.GetOrder(dweller.ID)
	testutil.MustNoErr(t, err, "reload dweller")
	if !IsGateStaged(still) {
		t.Error("the dweller keeps dwelling — the carrier's pickup is what releases it")
	}
}

// TestWindow3_OccupiedSlotIsNotAWallToDigOut guards the arm that separates "I am
// walled" from "my slot is taken", which look identical from the refusal and want
// opposite answers.
//
// If a bin already sits in the slot a store is aimed at, excavating the corridor
// in front of it delivers the robot to an occupied slot — the dig would run,
// consume a lane lock and two shuffle slots, and change nothing about why the
// order cannot place. The answer to a taken slot is a re-bind (which
// rebindGatedDropoff already does whenever one is reachable) or another lane.
//
// MUTATION (run 2026-08-10): drop the store-slot-emptiness arm from
// mouthHealNeeded. A dig is created for a lane that is simply FULL, and it repeats
// every time the lane is re-evaluated as long as the fill lasts.
func TestWindow3_OccupiedSlotIsNotAWallToDigOut(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	wall, _, w, _, bp := healLaneFixture(t, db, "HL3")
	line := lineNode(t, db, "HL3-LINE")

	deep := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = w[2].Name
		o.Status = "in_transit"
	})
	if adm, _, _, err := d.AcquireLanesForOrder(deep, line, w[2], EntryFreshBin); err != nil || !adm {
		t.Fatalf("the deeper store must take its mouth row: adm=%v err=%v", adm, err)
	}
	testutil.MustNoErr(t, db.UpdateOrderVendor(deep.ID, "hl3-deep", "RUNNING", ""), "deep vendor")

	dweller := stageGatedStore(t, db, d, line, w[1], nil)
	if !IsGateStaged(dweller) {
		t.Fatalf("the dweller must be gate-staged (wait_index=%d)", dweller.WaitIndex)
	}
	markStaged(t, db, dweller.ID)

	// A wall in front AND a bin in the dweller's own slot. The lane is just full.
	createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-HL3-WALL")
	createTestBinAtNode(t, db, bp.Code, w[1].ID, "BIN-HL3-SITTING")

	d.ReleaseInboundLaneForOrder(deep.ID, w[2].Name)
	d.EvaluateLaneReleases(wall.ID)

	if parent := digParentFor(t, db, w[0].Name); parent != nil {
		t.Fatalf("heal dig %d was created for a lane whose problem is that it is FULL. Clearing the "+
			"corridor would deliver the robot to a slot that already has a bin in it; the answer to "+
			"a taken slot is a re-bind, not an excavation.", parent.ID)
	}
	if n := healParentsMinted(t, db, wall.Name); n != 0 {
		t.Errorf("%d heal parent(s) minted for a lane that is simply full", n)
	}
	if d.laneLock.IsLocked(wall.ID) {
		t.Error("no dig was warranted, so no lane lock may have been taken")
	}
}

// healParentsMinted counts every heal parent this lane has ever had, cancelled
// ones included.
//
// digParentFor cannot answer this: it looks for a child, and a heal that was
// refused by the compound transaction has no children — the whole write rolled
// back. The parent row is the only trace such an attempt leaves, which is what
// makes it the right thing to count when the claim is "we did not even try".
// TestWindow3_OrdinaryMouthHoldRefusesTheHealBeforeMintingAParent is the
// 16,947-order runaway, reproduced and then prevented.
//
// ── TWO READERS, TWO QUESTIONS, A DURABLE WRITE BETWEEN THEM ─────────────
//
// The heal path pre-checked with LaneLock.IsLocked — "does a DIG hold this
// lane" — then created the dig's PARENT ORDER, then called TryLock, which is
// AcquireLanes(ModeDig) and refuses on ANY other owner's mouth row. TryLock's
// own doc says so: "already held — by another dig, OR BY AN ORDINARY ORDER'S
// MOUTH HOLD."
//
// So on a lane an ordinary order holds, the guard said GO and the claim said NO
// — forever, because that answer does not change while the row exists, and a
// gate-staged order keeps its row until it places. Between the two sat an
// INSERT, so every lane event minted an order and cancelled it.
//
// MEASURED, lane-stress rig 2026-08-10, on a freshly seeded plant: LS_C5 held
// exactly one row — `mouth`/`outbound`, owned by a staged order. 16,947 heal
// parents were created and cancelled in minutes, ZERO digs ever started, ~64
// cancellations per second, and every other status drained to nothing. The
// cancellation reason read "another dig took lane LS_C5 first", naming a dig
// that did not exist — which is what sent the first look in the wrong direction.
//
// THE OUTCOME IS UNCHANGED AND THAT IS THE POINT. AcquireLanes was always going
// to refuse this dig; asking one read earlier does not change what happens on
// the floor, only whether Core writes an order to find out. That is what makes
// this a churn fix rather than a policy change.
//
// MUTATION (fires): restore `if d.laneLock.IsLocked(lane.ID) { return }` →
// assertion (a) reports a minted, cancelled heal parent.
func TestWindow3_OrdinaryMouthHoldRefusesTheHealBeforeMintingAParent(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	wall, _, w, _, bp := healLaneFixture(t, db, "HL4")
	line := lineNode(t, db, "HL4-LINE")

	// The Tier-2 blocker that parks the dweller, exactly as the positive test
	// builds it.
	deep := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = w[2].Name
		o.Status = "in_transit"
	})
	if adm, _, _, err := d.AcquireLanesForOrder(deep, line, w[2], EntryFreshBin); err != nil || !adm {
		t.Fatalf("the deeper store must take its mouth row: adm=%v err=%v", adm, err)
	}
	testutil.MustNoErr(t, db.UpdateOrderVendor(deep.ID, "hl4-deep", "RUNNING", ""), "deep vendor")

	dweller := stageGatedStore(t, db, d, line, w[1], nil)
	if !IsGateStaged(dweller) {
		t.Fatalf("the dweller must be gate-staged (wait_index=%d)", dweller.WaitIndex)
	}
	markStaged(t, db, dweller.ID)

	// An unclaimed wall — so a heal is genuinely WARRANTED. Without this the test
	// would pass for the wrong reason (nothing to dig).
	blocker := createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-HL4-WALL")
	if blocker.ClaimedBy != nil {
		t.Fatalf("fixture bug: the walling bin must be UNCLAIMED")
	}

	// The ordering reason goes, leaving only the wall — the exact state the
	// positive test digs from.
	d.ReleaseInboundLaneForOrder(deep.ID, w[2].Name)

	// ── AND NOW THE RIG'S ACTUAL SHAPE: an ORDINARY, non-dig mouth hold on the
	// same lane. A retrieve leaving this lane takes an `outbound` row, which is
	// what LS_C5 was holding.
	leaver := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.SourceNode = w[0].Name
		o.DeliveryNode = line.Name
		o.Status = "in_transit"
	})
	if adm, _, _, err := d.AcquireLanesForOrder(leaver, w[0], line, EntryHeldBin); err != nil || !adm {
		t.Fatalf("the leaver must take an outbound mouth row on the wall lane: adm=%v err=%v", adm, err)
	}
	if d.laneLock.IsLocked(wall.ID) {
		t.Fatal("precondition: no DIG holds the lane — that is the whole point, the holder is ordinary")
	}
	if d.laneLock.CanTake(wall.ID) {
		t.Fatal("precondition: a dig must NOT be admissible while an ordinary mouth row is held; " +
			"if it is, this fixture is not reproducing the runaway")
	}

	d.EvaluateLaneReleases(wall.ID)

	// (a) NO ORDER WAS WRITTEN TO DISCOVER A REFUSAL THAT WAS KNOWABLE.
	if n := healParentsMinted(t, db, wall.Name); n != 0 {
		t.Errorf("%d heal parent(s) were minted for %s and cancelled. An ordinary order holds the "+
			"lane's mouth, so AcquireLanes was always going to refuse the dig — and the pre-check "+
			"asked about DIGS instead, so it said go. On the rig this repeated on every lane event: "+
			"16,947 orders, no dig, the plant doing nothing else", n, wall.Name)
	}

	// (b) AND NO LOCK WAS TAKEN. The outcome is identical to before the fix; only
	// the paper trail differs.
	if d.laneLock.IsLocked(wall.ID) {
		t.Error("a dig lock was taken on a lane an ordinary order holds")
	}
}

// TestWindow3_ClaimedBlockerRefusesTheHealBeforeMintingAParent is the test above
// one layer down, and it is the 2026-08-13 rig's other runaway.
//
// THE MEASUREMENT: 38,203 heal parents created and cancelled over 2h15m on the
// lane-stress rig, a steady 200-290 a minute, every one of them ending "heal dig
// not started: a blocker was claimed while the dig was being written". The blocker
// was claimed by a complex order that was itself frozen for the whole window, so
// the answer never changed — and each cancellation was a terminal event that
// re-drove the proposer that had just been refused, so the loop fed itself.
//
// WHY THE EXISTING FACT-3 CHECK DID NOT CATCH IT: mouthHealNeeded asks about
// claimed blockers, but mouthHealNeeded is the LANE GATE's route into the
// proposer. The rig's loop came through a complex demand's walled pickup
// (proposeDigForBuriedPickup), which asked nothing. That is why the check now
// lives in proposeLaneClearDig — the one writer of a service dig — instead of in
// a third caller.
//
// SO THIS TEST GOES THROUGH THE WRITER, not through the gate: a gate-routed
// fixture would be answered by fact 3 and pass with the fix reverted.
//
// MUTATION (verified): delete the blocker-claim loop in proposeLaneClearDig.
// Assertion (a) fires with one minted-and-cancelled parent — one per firing, which
// on the rig was two hours of them.
func TestWindow3_ClaimedBlockerRefusesTheHealBeforeMintingAParent(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	wall, _, w, _, bp := healLaneFixture(t, db, "HL5")
	line := lineNode(t, db, "HL5-LINE")

	// The wall, and a live order carrying it out — the commonest holder by a wide
	// margin, per BlockerClaimedError's own header.
	blocker := createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-HL5-WALL")
	holder := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.SourceNode = w[0].Name
		o.DeliveryNode = line.Name
		o.Status = "in_transit"
	})
	testutil.MustNoErr(t, execAt(db, `UPDATE bins SET claimed_by=$1 WHERE id=$2`, holder.ID, blocker.ID),
		"claim the blocker")

	// A requester walled behind it. It is carried for its origin only — the dig
	// serves the lane, not this order.
	requester := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = w[1].Name
		o.Status = protocol.StatusPending
	})

	if !d.laneLock.CanTake(wall.ID) {
		t.Fatal("precondition: the LANE is takeable — this test is about the BIN, and a lane-level " +
			"refusal would let it pass with the fix reverted")
	}

	res := d.proposeLaneClearDig(wall, w[1], requester)

	// (a) NO ORDER WAS WRITTEN TO DISCOVER A REFUSAL THAT WAS KNOWABLE.
	if n := healParentsMinted(t, db, wall.Name); n != 0 {
		t.Errorf("%d heal parent(s) were minted for %s and cancelled. The blocker was already claimed "+
			"when the plan was built, so the claim CAS was always going to refuse — and on the rig this "+
			"repeated on every event for 2h15m: 38,203 orders, no dig, and each cancellation re-driving "+
			"the next attempt", n, wall.Name)
	}

	// (b) AND IT IS STILL A WAIT, NOT A FAULT. Law 1: a claimed blocker is
	// congestion. The outcome has to be the one whose releaser is the holder
	// carrying the bin out, or the requester parks under something that never clears.
	if res.outcome != serviceDigBlockerClaimed {
		t.Errorf("outcome is %v, want serviceDigBlockerClaimed — the holder is a live order driving "+
			"that bin out of the lane, which is the releaser the requester's wait names", res.outcome)
	}

	// (c) AND NO LOCK WAS TAKEN.
	if d.laneLock.IsLocked(wall.ID) {
		t.Error("a dig lock was taken for a dig that was never going to be written")
	}
}

func healParentsMinted(t *testing.T, db *store.DB, laneName string) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT count(*) FROM orders WHERE payload_desc LIKE $1`, "clear "+laneName+":%").Scan(&n),
		"count heal parents")
	return n
}

// execAt is a one-line raw write for the two places these tests have to play the
// ROBOT — arriving a bin, and putting a claim on one. Both are fleet-side facts
// with no Core-side writer to borrow, and spelling them out is honest about which
// half of the loop the test is standing in for.
func execAt(db *store.DB, q string, args ...any) error {
	_, err := db.DB.Exec(q, args...)
	return err
}

// appendsTo counts the tail appends aimed at one vendor order.
//
// Counting ALL appends does not work here and the reason is worth stating: the
// dig's own leg picks OUT of the marked lane, so it goes through the same gated
// valve as everyone else and gets its own tail appended the moment it is
// dispatched. A total is therefore a mix of the dweller's release and the dig's
// ordinary traffic, and an assertion on it would be measuring the wrong robot.
func appendsTo(backend *testdb.MockBackend, vendorOrderID string) int {
	n := 0
	for _, c := range backend.ReleaseCalls() {
		if c.VendorOrderID == vendorOrderID {
			n++
		}
	}
	return n
}

// robotRunsDigLeg plays the fleet for one dig leg: the bin arrives at the slot the
// leg was sent to, and the leg goes terminal.
//
// IT TERMINALIZES THROUGH THE REAL PATH rather than fast-forwarding the status
// column, and that is the whole reason this exists next to the shared
// driveCompoundChildrenToConfirmed helper instead of calling it. That helper
// writes `confirmed` with a raw UPDATE, deliberately — most compound tests only
// care about the parent's terminal effect. But a raw status write skips
// TerminalizeOrder, which is where a finished order's claims and RESERVATIONS are
// released in the same transaction as the status. So a fast-forwarded dig leg
// keeps its Hold B occupancy row on the dug lane forever, and the next entrant is
// correctly refused with lane-occupied by a robot that finished minutes ago.
//
// That is a property of the harness, not of the floor — but it is exactly the
// fact this test is about, so borrowing the shortcut would have the test assert
// the shortcut's behaviour instead of the product's.
func robotRunsDigLeg(t *testing.T, db *store.DB, leg *orders.Order, destNodeID int64) {
	t.Helper()
	testutil.MustNoErr(t, execAt(db, `UPDATE bins SET node_id=$1, claimed_by=NULL WHERE id=$2`,
		destNodeID, *leg.BinID), "the robot places the blocker")
	if _, err := db.TerminalizeOrder(leg.ID, StatusConfirmed, "test harness: the leg placed its bin"); err != nil {
		t.Fatalf("terminalize dig leg %d: %v", leg.ID, err)
	}
}
