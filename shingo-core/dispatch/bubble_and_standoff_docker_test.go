//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// bubble_and_standoff_docker_test.go — two ways a healthy-looking plant loses
// capacity it never gets back, both measured on the lane-stress rig 2026-08-13.

// TestFindShuffleSlots_WillNotSealAnEmptySlotSomebodyIsDrivingTo is the bubble.
//
// ── WHY A SEALED EMPTY SLOT IS WORSE THAN A SEALED BIN ────────────────────
//
// A sealed BIN is recoverable: it is in somebody's way, so a dig gets raised and
// the corridor opens. A sealed EMPTY SLOT is in nobody's way. No demand names
// it, no claim protects it, and no excavation will ever be planned against it —
// it is simply gone for the life of the plant, and the only trace is a census
// counting one more air bubble than yesterday.
//
// ── THE SPECIMEN ──────────────────────────────────────────────────────────
//
// LSD_003 (depth 3) was order 57's delivery node, and order 57 was driving to
// it. A dig leg parked its blocker at LSD_002 (depth 2), one slot in front —
// lawful by every rule that existed, because all three "somebody holds this"
// guards remove the DEEPER slot from the candidate pool and say nothing about
// the shallower ones. LSD_003 has been unreachable ever since.
//
// The rig's other bubble, LSD_010, is the same shape from the store side: it was
// order 7's delivery node, stores filled d2 and d3 while it waited, order 7 was
// cancelled, and d5 was walled behind them.
//
// MUTATION: drop the `entombing[slot.ID]` skip from shuffleSlotsFrom. The dig is
// offered the slot in front again and the assertion fires.
func TestFindShuffleSlots_WillNotSealAnEmptySlotSomebodyIsDrivingTo(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	newTestDispatcher(t, db, testdb.NewSuccessBackend())
	grp, dug, park, _, dugSlots, parkSlots, bp := setupDwellGroup(t, db, "BUBBLE", 4, false)

	// Something to dig.
	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "BUBBLE-BLK")
	createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "BUBBLE-TGT")

	// THE SPOKEN-FOR SLOT: a MIDDLE slot in the parking lane, EMPTY, with a live
	// order driving to fill it. Its delivery_node is the plainest of the three
	// ownership spellings and the one the rig's specimen used.
	//
	// Middle rather than deepest ON PURPOSE. The guard has to be surgical: the
	// slots BEHIND a spoken-for one cannot seal it and stay legal parking. A
	// whole-lane refusal would pass a deepest-slot fixture and starve the plant.
	target := parkSlots[len(parkSlots)-2]
	inbound := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "bubble-inbound"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.Status = protocol.StatusInTransit
		o.DeliveryNode = target.Name
	})

	slots, err := findShuffleSlots(db, dug.ID, grp.ID, 1, digAskerFor(inbound), nil)
	testutil.MustNoErr(t, err, "ask for parking with a deeper slot spoken for")
	if len(slots) == 0 {
		t.Fatal("no parking offered at all — the lane has slots deeper than the spoken-for one and " +
			"those are still legal; refusing the whole lane starves the dig instead of protecting a slot")
	}
	for _, s := range slots {
		if s.ParentID == nil || *s.ParentID != park.ID {
			continue
		}
		if s.Depth == nil || target.Depth == nil {
			t.Fatalf("slot %s or %s has no depth — the fixture is not a lane", s.Name, target.Name)
		}
		if *s.Depth < *target.Depth {
			t.Fatalf("the pool offered %s (depth %d), which is in front of %s (depth %d) — an EMPTY "+
				"slot order %d is driving to fill. Parking here seals it, and a sealed empty slot is "+
				"in nobody's way: no demand names it, no claim protects it, and no dig will ever be "+
				"raised against it. That is capacity gone for the life of the plant",
				s.Name, *s.Depth, target.Name, *target.Depth, inbound.ID)
		}
	}

	// ── AND IT IS THE ORDER'S PROTECTION, NOT THE SLOT'S ────────────────────
	//
	// Asked with everything BEHIND the target excluded, so the only candidates
	// left in this lane are the ones in front of it. That turns "is a shallower
	// slot offered" into the whole answer instead of something the deepest-first
	// walk can satisfy without ever considering it.
	behind := map[int64]bool{}
	for _, s := range parkSlots {
		if s.Depth != nil && target.Depth != nil && *s.Depth > *target.Depth {
			behind[s.ID] = true
		}
	}
	if _, err := findShuffleSlots(db, dug.ID, grp.ID, 1, digAskerFor(inbound), behind); err == nil {
		t.Fatal("with everything behind the target excluded, the only slots left in that lane are in " +
			"front of it — and they are refused while order is driving there. Offering one is the bubble")
	}

	testutil.MustNoErr(t, db.FailOrderAtomic(inbound.ID, "the inbound order went away"),
		"terminalize the inbound order")

	after, err := findShuffleSlots(db, dug.ID, grp.ID, 1, digAskerFor(inbound), behind)
	testutil.MustNoErr(t, err, "ask again once nothing is coming for that slot")
	if len(after) == 0 {
		t.Fatal("still nothing offered after the only order driving to that slot reached a terminal " +
			"status. The protection belongs to the order, not to the slot: nothing is coming for it " +
			"now, so the slots in front of it are ordinary parking again")
	}
}

// TestServiceDig_UngatedProposalIsCounted is the sibling of the standoff test
// below, run against the population that standoff protection does not reach.
//
// The one-dig-per-episode gate is keyed on the ORIGIN, because a dig serves a
// LANE and one dig serves every demand behind it (§R.40) — there is no 1:1
// identity to key on, and the episode is the only tie a dig has. Which means a
// requester carrying NO episode cannot be gated by it at all: the query has no
// key, the store declines to guess, and the proposal goes through.
//
// That was invisible. LiveServiceDigInEpisode returned a bare 0 for "I did not
// ask", which is the same value it returns for "I asked and nothing is running",
// so the caller could not tell an absent answer from a clean one and the gate's
// silence looked like the gate passing.
//
// THIS TEST DOES NOT ASSERT THAT THE SECOND DIG IS REFUSED, and that is the
// point. The behaviour is deliberately unchanged — an originless order still
// gets no limit, because serialising every unattributed dig in the plant against
// every other is the worse failure. What is asserted is that the skip is now
// COUNTED, so the size of the ungated population is knowable before closing the
// origin leak switches this gate on for all of it.
//
// NOT PARALLEL, DELIBERATELY. The tally is a process-global instrument; a
// parallel test that reads it is asserting on the rest of the suite, which is
// how multibin_settle_docker_test.go first went red. Go pauses parallel tests
// while a sequential one runs, so the delta below is this test's own.
//
// MUTATION (verified): drop the `case !asked:` arm from proposeLaneClearDig and
// the delta reads 0 — the gate goes back to being silently off.
func TestServiceDig_UngatedProposalIsCounted(t *testing.T) {
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, first, second, _, firstSlots, secondSlots, bp := setupDwellGroup(t, db, "UNGATED", 2, true)

	createTestBinAtNode(t, db, bp.Code, firstSlots[0].ID, "UNGATED-BLK-1")
	createTestBinAtNode(t, db, bp.Code, firstSlots[1].ID, "UNGATED-TGT-1")
	createTestBinAtNode(t, db, bp.Code, secondSlots[0].ID, "UNGATED-BLK-2")
	createTestBinAtNode(t, db, bp.Code, secondSlots[1].ID, "UNGATED-TGT-2")

	// The same demand as the standoff test, minus the one thing the gate needs.
	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "ungated-demand"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.Status = protocol.StatusQueued
		o.OriginID = ""
		o.OriginClass = "orphan"
	})

	ResetUngatedDigTally()
	if res := d.proposeLaneClearDig(first, firstSlots[1], demand, digOwnedByRequester); res.outcome != serviceDigStarted {
		t.Fatalf("the first dig did not start (outcome %v, err %v)", res.outcome, res.err)
	}
	res := d.proposeLaneClearDig(second, secondSlots[1], demand, digOwnedByRequester)

	// THE GATE IS OFF. Whatever else the second proposal runs into — parking,
	// geometry — it must not be the episode limit, because there is no episode.
	if res.outcome == serviceDigEpisodeAlreadyDigging {
		t.Fatalf("the second proposal was refused as serviceDigEpisodeAlreadyDigging for a demand with "+
			"no origin. The gate cannot be keyed without one; a refusal here means it is guessing, "+
			"which serialises every unattributed dig in the plant against every other (err %v)", res.err)
	}

	// AND BOTH SKIPS WERE RECORDED. Two proposals, two ungated decisions.
	if n := UngatedDigTally(); n != 2 {
		t.Errorf("UngatedDigTally() = %d, want 2 — both proposals reached the planner without the "+
			"one-dig-per-episode gate being able to run, and an admission gate that is switched off "+
			"and says nothing is how a dispatch-shaping change later gets attributed to the wrong "+
			"cause. This count is the population that closing the origin leak will switch it on for.", n)
	}
}

// TestServiceDig_OneEpisodeGetsOneExcavationAtATime is the standoff.
//
// ── HOW ONE DEMAND DEADLOCKS ITSELF ───────────────────────────────────────
//
// A buried demand does not wait for its dig. It re-resolves onto whatever it can
// reach, and a lane under excavation is the one place it cannot — so it picks a
// second bin, finds THAT buried, and raises a second dig for the same one bin it
// needs. The two then compete for the same scarce parking.
//
// Digs 2 and 8 on the lane-stress rig 2026-08-13, both raised for order 1, ended
// in a closed mutual hold: dig 2 holding LS_D1 and needing parking only LS_D3
// could give, dig 8 holding LS_D3 and needing parking only LS_D1 could give.
// Every wait individually lawful, the walk closed, and the tripwire filed it.
//
// Right of way (§R.61) is supposed to make that unreachable, and it is a
// PLAN-TIME rule: both digs planned before either had taken its lane, so both
// passed. Rather than re-check after the acquire — which costs an order row per
// attempt, the shape this file has already paid for twice — the second dig is
// simply not raised.
//
// MUTATION: delete the LiveServiceDigInEpisode guard from proposeLaneClearDig.
// The second dig starts, and the fixture's two digs hold each other's parking.
func TestServiceDig_OneEpisodeGetsOneExcavationAtATime(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, first, second, _, firstSlots, secondSlots, bp := setupDwellGroup(t, db, "ONEDIG", 2, true)

	// Two lanes, each with a bin buried behind a blocker — two legitimate
	// excavations, and one demand that could ask for both.
	createTestBinAtNode(t, db, bp.Code, firstSlots[0].ID, "ONEDIG-BLK-1")
	createTestBinAtNode(t, db, bp.Code, firstSlots[1].ID, "ONEDIG-TGT-1")
	createTestBinAtNode(t, db, bp.Code, secondSlots[0].ID, "ONEDIG-BLK-2")
	createTestBinAtNode(t, db, bp.Code, secondSlots[1].ID, "ONEDIG-TGT-2")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "onedig-demand"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.Status = protocol.StatusQueued
		o.OriginID = "77777777-7777-7777-7777-777777777777"
		o.OriginClass = "demand"
	})

	if res := d.proposeLaneClearDig(first, firstSlots[1], demand, digOwnedByRequester); res.outcome != serviceDigStarted {
		t.Fatalf("the FIRST dig did not start (outcome %v, err %v) — this test is about the second one",
			res.outcome, res.err)
	}

	// THE SECOND ASK, for a different lane and a different bin, on behalf of the
	// same demand. It is refused, and the refusal names itself.
	res := d.proposeLaneClearDig(second, secondSlots[1], demand, digOwnedByRequester)
	if res.outcome != serviceDigEpisodeAlreadyDigging {
		t.Fatalf("the second dig's outcome is %v, want serviceDigEpisodeAlreadyDigging. This demand "+
			"needs ONE bin and already has an excavation running for it; a second one competes with "+
			"the first for the same parking, which is how digs 2 and 8 ended up holding each other's "+
			"lanes with neither able to finish", res.outcome)
	}

	// AND NOTHING WAS COMMITTED. The guard is asked before the plan and before the
	// parent, so a refusal costs a read — not an order row created and cancelled,
	// which is the churn this file has measured twice (16,947 and 38,203).
	if d.laneLock.IsLocked(second.ID) {
		t.Errorf("lane %s was taken by a dig that was refused — the guard must run before the acquire",
			second.Name)
	}
	live, err := db.ListOrders(string(protocol.StatusCancelled), 50)
	testutil.MustNoErr(t, err, "list cancelled orders")
	for _, o := range live {
		if o.DigTargetNode == secondSlots[1].Name {
			t.Errorf("order %d was minted for the refused dig and then cancelled. The guard is asked "+
				"before createServiceDigParent precisely so a refusal costs no row", o.ID)
		}
	}
}
