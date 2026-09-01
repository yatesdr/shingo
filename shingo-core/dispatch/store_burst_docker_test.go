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
	"shingocore/store/reservations"
)

// store_burst_docker_test.go — catalog row 4.5: five stores arrive at a group
// while one of its lanes is being dug.
//
// The two halves are each pinned once, in isolation and one bin at a time.
// Selection diverting off a dug lane is
// binresolver/group_resolver_dig_divert_test.go, against a FAKE store with no
// database and no reservations. A single already-resolved store re-aiming is
// TestStoreRedirect_DugLaneWithAFreeSibling_ReSelects. Neither has ever seen more
// than one store, and one store cannot exhibit the thing a burst is about: the
// second store must not be handed the slot the first one just took, and the
// exhaustion boundary — where diverting stops being possible — is only reachable
// with enough of them to fill the sibling.
//
// The sibling here also carries a MARK, which is the composition the audit found
// missing: an order that diverts still has to get INTO the lane it diverted to,
// and on a marked lane that is a second decision made by different code at a
// different moment. Divert-then-gate had no coverage at all.
//
// "NONE PARK BEHIND THE EXCAVATION NEEDLESSLY" is asserted as the negative it is
// stated as, and the word needlessly is load-bearing: the sixth store parks, and
// must, because by then there is genuinely nowhere else. A test that only asserted
// "nobody parks" would be asserting a bug.

// storeBurstGroup builds a group with a lane a dig will take and one MARKED
// sibling of the same depth:
//
//	‹prefix›-GRP
//	├── LANE-DUG:  D1…D5   (where the burst was aimed before the dig)
//	└── LANE-MARK: M1…M5   (the only sibling, with a mark at ‹prefix›-WAIT)
//
// Equal depth is deliberate: the sibling can absorb the whole burst and not one
// bin more, so the divert boundary lands exactly at store six.
func storeBurstGroup(t *testing.T, db *store.DB, prefix string, depth int) (grp, laneDug, laneMark *nodes.Node, dugSlots, markSlots []*nodes.Node, bp *payloads.Payload) {
	t.Helper()
	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp = &payloads.Payload{Code: prefix + "-P"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload")

	grp = &nodes.Node{Name: prefix + "-GRP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")

	mkLane := func(name, mark string) (*nodes.Node, []*nodes.Node) {
		lane := &nodes.Node{Name: name, NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
		testutil.MustNoErr(t, db.CreateNode(lane), "create "+name)
		if mark != "" {
			testutil.MustNoErr(t, db.SetNodeProperty(lane.ID, PropLaneGatePoint, mark), "place the mark")
		}
		var slots []*nodes.Node
		for i := 1; i <= depth; i++ {
			at := i
			s := &nodes.Node{Name: fmt.Sprintf("%s-S%d", name, i), ParentID: &lane.ID, Enabled: true, Depth: &at}
			testutil.MustNoErr(t, db.CreateNode(s), "create slot")
			slots = append(slots, s)
		}
		reloaded, _ := db.GetNode(lane.ID)
		return reloaded, slots
	}
	laneDug, dugSlots = mkLane(prefix+"-LANE-DUG", "")
	laneMark, markSlots = mkLane(prefix+"-LANE-MARK", prefix+"-WAIT")

	grp, _ = db.GetNode(grp.ID)
	return grp, laneDug, laneMark, dugSlots, markSlots, bp
}

// laneOf resolves the lane a delivery node sits in, by name, for the assertions
// below. Returns nil when the node is not a lane slot.
func laneOf(t *testing.T, db *store.DB, nodeName string) *nodes.Node {
	t.Helper()
	n, err := db.GetNodeByDotName(nodeName)
	testutil.MustNoErr(t, err, "resolve node "+nodeName)
	lane, err := db.LaneForNode(n.ID)
	testutil.MustNoErr(t, err, "resolve lane for "+nodeName)
	return lane
}

// TestStoreBurst_FiveAtOneDugLane_DivertToDistinctSlots is the burst, and the
// distinctness is the half a single-order test cannot reach.
//
// Five stores queued behind inventory with concrete destinations in one lane. A
// dig takes that lane. Every one of them re-enters through the door the scanner
// uses on every dispatch attempt, and they must come out aimed at five DIFFERENT
// slots in the sibling — a re-selection that is correct for one order and hands
// the same slot to all five would pass TestStoreRedirect_DugLaneWithAFreeSibling
// exactly as it stands.
//
// MUTATION 1 (verified): delete the redirectStoreOffDugLane call from
// ReserveStorageDropoff (store_slot.go). All five keep destinations inside the dug
// lane and the "still aimed into the excavation" assertion fires.
//
// MUTATION 2 (verified): make resolveStoreLKND call ListChildNodes instead of
// ListChildNodesUnlocked (binresolver/group_resolver.go) — the same mutation the
// binresolver unit tests use, reached here through the whole database path
// instead of a fake store. The re-selection offers the dug lane back and the same
// assertion fires. Two mutations, one assertion, because the divert has two
// mechanisms in series and either one alone parks the burst.
func TestStoreBurst_FiveAtOneDugLane_DivertToDistinctSlots(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	grp, laneDug, laneMark, dugSlots, _, bp := storeBurstGroup(t, db, "SB", 5)
	d := window4Dispatcher(t, db) // a real resolver — the re-selection has to be able to run

	const burst = 5
	var flock []*orders.Order
	for i := range burst {
		bin := createTestBinAtNode(t, db, bp.Code, grp.ID, fmt.Sprintf("SB-BIN-%d", i))
		flock = append(flock, parkedStore(t, db, d, fmt.Sprintf("sb-%d", i), dugSlots[i], bin.ID))
	}

	// The dig arrives after every destination was chosen. That ordering IS the
	// case: a store choosing now would never pick this lane, because the candidate
	// read filters dig-held lanes out in SQL.
	digger := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "sb-digger" })
	if !d.laneLock.TryLock(laneDug.ID, digger.ID) {
		t.Fatal("TryLock on a free lane must succeed")
	}

	// The re-entry every dispatch attempt makes, once per store.
	for _, o := range flock {
		rErr := d.ReserveStorageDropoff(o).Err
		testutil.MustNoErr(t, rErr, "re-entry for "+o.EdgeUUID)
	}

	seen := make(map[string]int64, burst)
	for i, o := range flock {
		after, err := db.GetOrder(o.ID)
		testutil.MustNoErr(t, err, "reload store")

		if lane := laneOf(t, db, after.DeliveryNode); lane != nil && lane.ID == laneDug.ID {
			t.Fatalf("store %d is still aimed at %s, inside dug lane %s — admission refuses it at "+
				"dispatch and it sits out the whole excavation with %s standing empty beside it",
				i, after.DeliveryNode, laneDug.Name, laneMark.Name)
		}
		if prev, dup := seen[after.DeliveryNode]; dup {
			t.Fatalf("stores %d and %d are both aimed at %s. Re-selection that is right for one order "+
				"and hands every other the same slot is worse than not re-selecting: the losers now "+
				"fail their reservation instead of merely waiting", prev, i, after.DeliveryNode)
		}
		seen[after.DeliveryNode] = int64(i)

		// The hold follows the destination. A hold left on the abandoned slot is a
		// slot nobody else in this burst can use — with five in flight that is the
		// difference between the sibling absorbing the burst and jamming halfway.
		dest, err := db.GetNodeByDotName(after.DeliveryNode)
		testutil.MustNoErr(t, err, "resolve the new destination")
		if !holdsSlot(t, db, after.ID, dest.ID) {
			t.Errorf("store %d is aimed at %s and does not hold it", i, after.DeliveryNode)
		}
		if holdsSlot(t, db, after.ID, dugSlots[i].ID) {
			t.Errorf("store %d still holds its abandoned slot %s", i, dugSlots[i].Name)
		}
		// And the BIN hold is untouched — the scope guard. A destination
		// re-selection that dropped it would send the order back to a finder that
		// excludes pending-reserved bins owner-blind.
		if after.BinID == nil || !holdsBin(t, db, after.ID, *after.BinID) {
			t.Errorf("store %d lost the hold on the material it was already carrying", i)
		}
	}
	if len(seen) != burst {
		t.Fatalf("the burst landed on %d distinct slots, want %d", len(seen), burst)
	}

	// ── AND THE SIXTH ONE PARKS, BECAUSE BY NOW IT MUST ───────────────────────
	// The sibling is full. "None park behind the excavation NEEDLESSLY" is the
	// ruling; this is the needful case, and it must park under the SHAPE that
	// already exists rather than half-re-aimed.
	extraBin := createTestBinAtNode(t, db, bp.Code, grp.ID, "SB-BIN-EXTRA")
	extra := parkedStore(t, db, d, "sb-extra", dugSlots[0], extraBin.ID)
	extraErr := d.ReserveStorageDropoff(extra).Err
	testutil.MustNoErr(t, extraErr, "re-entry with nowhere to go")

	stuck, err := db.GetOrder(extra.ID)
	testutil.MustNoErr(t, err, "reload the sixth store")
	if stuck.DeliveryNode != dugSlots[0].Name {
		t.Errorf("the sixth store moved to %s with the sibling full — there was nowhere better, so it "+
			"keeps what it had and waits", stuck.DeliveryNode)
	}
	if !holdsSlot(t, db, extra.ID, dugSlots[0].ID) {
		t.Error("the sixth store gave up its slot hold and got nothing for it — a half-re-aimed order " +
			"is worse off than one that simply waited")
	}
	if !holdsBin(t, db, extra.ID, extraBin.ID) {
		t.Error("the sixth store's bin hold was released")
	}
}

// TestStoreBurst_DivertedOntoAMarkedLane_StagesInsteadOfParking is the
// composition: the burst diverts, and then meets a gate.
//
// The divert answers WHERE. The mark answers WHEN, at a different moment and in
// different code, and the two had never been asked in sequence. What the
// composition has to produce is the gate's whole premise applied to a diverted
// order: every one of the five is SENT — a robot per store, all of them doing
// their pre-lane work and then dwelling at the mark — and exactly one is let into
// the corridor. Neither "all five sent" nor "one inside" is interesting alone; a
// system that parks four of them pre-dispatch also has one inside, and it has put
// the lane's congestion back on the press.
//
// MUTATION (verified): delete the entryWhenGated arm from admitLane — the
// `if d.entryDeferredToGate(s.skip, lane)` early return (admission.go). Store 1 is
// then refused at DISPATCH with lane-occupied — parked whole, its pre-lane work
// never sent — and the "refused at DISPATCH on a MARKED lane" assertion fires on
// the second store of the burst. One store could never show this: the first one is
// admitted either way, so the arm is only reachable with a second in the same
// lane.
func TestStoreBurst_DivertedOntoAMarkedLane_StagesInsteadOfParking(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	backend := testdb.NewSuccessBackend()
	grp, laneDug, laneMark, dugSlots, _, bp := storeBurstGroup(t, db, "SBG", 5)
	d := NewDispatcher(db, backend, &mockEmitter{}, "core", "shingo.dispatch",
		&DefaultResolver{DB: db})
	press := lineNode(t, db, "SBG-PRESS")

	const burst = 5
	var flock []*orders.Order
	for i := range burst {
		bin := createTestBinAtNode(t, db, bp.Code, grp.ID, fmt.Sprintf("SBG-BIN-%d", i))
		flock = append(flock, parkedStore(t, db, d, fmt.Sprintf("sbg-%d", i), dugSlots[i], bin.ID))
	}
	digger := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "sbg-digger" })
	if !d.laneLock.TryLock(laneDug.ID, digger.ID) {
		t.Fatal("TryLock on a free lane must succeed")
	}

	// Divert, then admit, then dispatch — the scanner's own order.
	for i, o := range flock {
		dErr := d.ReserveStorageDropoff(o).Err
		testutil.MustNoErr(t, dErr, "divert")
		after, err := db.GetOrder(o.ID)
		testutil.MustNoErr(t, err, "reload after the divert")
		if lane := laneOf(t, db, after.DeliveryNode); lane == nil || lane.ID != laneMark.ID {
			t.Fatalf("store %d diverted to %s, which is not in the marked sibling %s — the gate half of "+
				"this test needs the burst to actually land there", i, after.DeliveryNode, laneMark.Name)
		}
		dest, err := db.GetNodeByDotName(after.DeliveryNode)
		testutil.MustNoErr(t, err, "resolve destination")

		admitted, cause, _, err := d.AcquireLanesForOrder(after, press, dest, EntryFreshBin)
		testutil.MustNoErr(t, err, "admission")
		if !admitted {
			t.Fatalf("store %d was refused at DISPATCH (%s) on a MARKED lane. Its create stops at the "+
				"wait point outside the corridor, so none of the in-lane questions are about it yet — "+
				"refusing here parks the order whole and puts the lane's congestion back on the press",
				i, cause)
		}
		testutil.MustNoErr(t, d.lifecycle.MoveToSourcing(after, "test", "dispatching the burst"), "to sourcing")
		if _, err := d.DispatchDirect(after, press, dest); err != nil {
			t.Fatalf("store %d could not be dispatched: %v", i, err)
		}
	}

	// EVERY ONE OF THEM WAS SENT.
	if got := len(backend.CreateRequests()); got != burst {
		t.Fatalf("%d fleet creates for %d stores — the ones missing never left the press, which is the "+
			"outcome the mark exists to prevent", got, burst)
	}
	for i, c := range backend.CreateRequests() {
		if c.Complete {
			t.Errorf("store %d was created SEALED — a lane-bound order on a marked lane ships unsealed "+
				"to the wait point, and a sealed one drives straight into the corridor", i)
		}
	}

	// AND EXACTLY ONE IS INSIDE. The valve was open for the first and shut behind
	// it; the other four are dwelling at the mark, still theirs to release.
	if got := len(backend.ReleaseCalls()); got != 1 {
		t.Fatalf("%d tails appended, want exactly 1 — one entry per lane-clear is what single-file "+
			"means, and it is the assertion five concurrent stores exist to make", got)
	}
	staged, err := d.GateStagedCount(laneMark.ID)
	testutil.MustNoErr(t, err, "count gate-staged orders")
	if staged != burst-1 {
		t.Fatalf("%d orders are gate-staged, want %d — the four that did not get in must be DWELLING "+
			"at the mark, not parked", staged, burst-1)
	}
	occ, err := reservations.OccupantsOf(db.DB, laneMark.ID)
	testutil.MustNoErr(t, err, "occupants of the marked lane")
	if len(occ) != 1 {
		t.Fatalf("the marked lane holds %d occupants (%v), want exactly one", len(occ), occ)
	}
	inside := occ[0]

	// ── ONE TAIL PER LANE-CLEAR, NOT ONE PER EVENT ────────────────────────────
	// The evaluator runs on every lane-clearing event, and a burst is precisely
	// where "it fired again" and "somebody left" get confused.
	for range 3 {
		d.EvaluateLaneReleases(laneMark.ID)
	}
	if got := len(backend.ReleaseCalls()); got != 1 {
		t.Fatalf("%d tails after re-evaluating a lane nobody left — the evaluator let a second robot "+
			"into an occupied corridor", got)
	}

	// The one inside PLACES ITS BIN and leaves, which is two facts and not one.
	// Occupancy says "a robot is in the corridor"; the inbound mouth row says "a bin
	// is still owed to this slot", and the depth tiers read the second — a deeper
	// store that has not placed holds its place, or a shallower one would wall it.
	// Both are released on the dropoff block's completion (wiring_block_completed),
	// and releasing only one here would leave the burst correctly stalled.
	placer, err := db.GetOrder(inside)
	testutil.MustNoErr(t, err, "reload the order inside the lane")
	placed, err := db.GetNodeByDotName(placer.DeliveryNode)
	testutil.MustNoErr(t, err, "resolve where it placed")
	testutil.MustNoErr(t, db.MoveBinClearingStaging(*placer.BinID, placed.ID, false), "place the bin")
	d.ReleaseInboundLaneForOrder(inside, placer.DeliveryNode)
	d.ReleaseLaneOccupancy(inside)
	d.EvaluateLaneReleases(laneMark.ID)

	if got := len(backend.ReleaseCalls()); got != 2 {
		t.Fatalf("%d tails after the lane cleared, want 2 — a dweller with no releaser is a stall "+
			"wearing a queue reason", got)
	}
	staged, err = d.GateStagedCount(laneMark.ID)
	testutil.MustNoErr(t, err, "count gate-staged after the clear")
	if staged != burst-2 {
		t.Errorf("%d orders still gate-staged, want %d — exactly one more should have gone in", staged, burst-2)
	}

	// AND NOBODY EVER AIMED INTO THE EXCAVATION. The negative, stated last because
	// it is the one the row asks for and the one an end-state check would miss.
	for i, o := range flock {
		after, err := db.GetOrder(o.ID)
		testutil.MustNoErr(t, err, "reload store")
		if lane := laneOf(t, db, after.DeliveryNode); lane != nil && lane.ID == laneDug.ID {
			t.Errorf("store %d ended up aimed into dug lane %s", i, laneDug.Name)
		}
		if after.Status == protocol.StatusQueued {
			t.Errorf("store %d is still queued — a free marked sibling means nothing waits at the press", i)
		}
	}
}
