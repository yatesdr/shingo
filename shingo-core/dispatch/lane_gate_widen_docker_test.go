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

// lane_gate_widen_docker_test.go — the oracle widening to the group.
//
// ── CONVERTED WITH C2c (Lane C, 2026-10-10) ───────────────────────────────
//
// The widen arm these tests were written for (widenDropoffToGroup and
// widenRefusedDropoff, behind the in-lane re-bind) is deleted: C2c ruling 1
// leaves one spelling for choosing a drop in a group, asked across the group at
// every release, and ruling 3 makes a refused mouth the next candidate, not a
// wait, while the group has one (DESIGN-C2c §7.3). The claims are kept, and
// their fixtures are the drop C2c chooses across the group: a coordinated swap
// standing at its group's waiting spot with its interior drop pencilled in lane
// A, stamped with the group, holding paper (c2c_group_choice_pins). A drop with
// no group stamp — the plain stores these fixtures used to stage — is asked of
// its own lane only (D13 Q-NAMED), so it could not carry the claims any more.
//
// Named moves (DESIGN-C2c §5.2): TestWiden_ClaimedBlockerWithAFreeSiblingWidens,
// TestWiden_ConcurrentRebindsDoNotDoubleAppend and
// TestWiden_ATierTwoWallIsAReasonToReAim keep their outcome (the sibling).
// TestWiden_ADigThatWouldHelpIsPreferredToWidening (D13 F3) and
// TestWiden_ATransientRefusalIsNotReAimed (D13 F4) FLIP and are renamed for what
// they now pin; their old names are in their comments.

// c2cWidenWall puts a bin at slot claimed by a live order, so no dig would help
// (acceptanceDigNeeded's rule 3: a hard claim is a robot in motion).
func c2cWidenWall(t *testing.T, db *store.DB, payload, label string, slot, line *nodes.Node) {
	t.Helper()
	blocker := createTestBinAtNode(t, db, payload, slot.ID, label)
	claimant := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.SourceNode = slot.Name
		o.DeliveryNode = line.Name
		o.Status = "in_transit"
	})
	// RESERVE THEN CLAIM: a bare claim is refused by design (store/bins.go).
	testdb.ReserveBin(t, db, claimant.ID, blocker.ID)
	testutil.MustNoErr(t, db.ClaimBin(blocker.ID, claimant.ID), "a live order claims the wall bin")
}

// c2cWaitLaneAt is the lane the plan's lane wait in front of the interior drop
// names after the release.
func c2cWaitLaneAt(steps []resolvedStep) (int64, bool) {
	i, _ := c2cInteriorDrop(steps)
	if i <= 0 || steps[i-1].Action != protocol.ActionWait || steps[i-1].WaitKind != WaitKindLane {
		return 0, false
	}
	return steps[i-1].WaitLane, true
}

// TestWiden_ClaimedBlockerWithAFreeSiblingWidens: a drop pencilled into a lane
// it cannot use, where a dig is NOT the answer (the walling bin is claimed by a
// live order, so it is leaving anyway), with a sibling lane of the same group
// empty. Standing still while the group has room is the state the group gate
// exists to end: the release chooses across the group and the drop lands in
// the sibling, and the wait moves with it (a stale WaitLane takes the wrong
// lane's occupancy at append, F-12's shape).
func TestWiden_ClaimedBlockerWithAFreeSiblingWidens(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	g := c2cTwoLaneGroup(t, db, "WIDEN1", 3, 2)
	press, press2 := lineNode(t, db, "WIDEN1-PRESS"), lineNode(t, db, "WIDEN1-PRESS2")
	elsewhere := lineNode(t, db, "WIDEN1-EMPTYSRC")
	testdb.CreateBinAtNode(t, db, "", elsewhere.ID, "WIDEN1-EMPTY")
	swap := c2cAtSpot(t, db, "widen1-swap", sd.Payload.Code, 0,
		c2cSwapPlan(g, press.Name, g.a[1].Name, elsewhere.Name, press2.Name, true))
	c2cPaper(t, db, swap.ID, g.a[1])
	c2cWidenWall(t, db, sd.Payload.Code, "BIN-WIDEN1-WALL", g.a[0], press)
	if n := binsIn(t, db, g.b...); n != 0 {
		t.Fatalf("fixture: the sibling lane must start empty, has %d bins", n)
	}

	c2cGatePass(d, g)

	after, steps := c2cReload(t, db, swap)
	_, drop := c2cInteriorDrop(steps)
	if lane := c2cLaneOf(t, db, drop.Node); lane != g.laneB.ID || after.WaitIndex < 2 {
		t.Fatalf("the drop is at %q (lane %d) at wait index %d (cause %q), want it released into the free "+
			"sibling %s — a lane walled by a CLAIMED blocker cannot be dug open, and its group has room",
			drop.Node, lane, after.WaitIndex, after.QueueCause, g.laneB.Name)
	}
	if wl, ok := c2cWaitLaneAt(steps); !ok || wl != g.laneB.ID {
		t.Errorf("the wait in front of the drop names lane %d, want the lane it was released into (%d) — "+
			"the wait and the entry it gates must name one corridor", wl, g.laneB.ID)
	}
	if aErr := d.assertEachWaitGatesItsEntry(steps); aErr != nil {
		t.Errorf("the released plan does not satisfy the splice-time guard: %v", aErr)
	}
}

// TestWiden_ADigIsSummonedOnlyWhenNoLaneOfTheGroupCanTakeTheDrop was
// TestWiden_ADigThatWouldHelpIsPreferredToWidening ("heal first", F-11), and it
// FLIPS with C2c (D13 F3): a walled slot is not reachable, so it is not a
// candidate (ruling 1), and a refused or walled lane is the next candidate, not
// a wait, while the group has one (ruling 3). Heal first holds only for a group
// with no other lane that can take the drop (DESIGN-C2c §7.3.4), which is the
// shape here: the pencilled lane's only free slot is behind an UNCLAIMED bin (a
// dig would open it), and the sibling lane is full. The group offers nothing,
// so the release refuses and the pass summons the order's own dig of its lane;
// the order stays at the spot. The flip's other half — the same wall with room
// in the sibling, released there with no dig — is
// TestC2cPin_ADropPlannedOnAFullLaneLandsInASiblingWithNoWait.
func TestWiden_ADigIsSummonedOnlyWhenNoLaneOfTheGroupCanTakeTheDrop(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	g := c2cTwoLaneGroup(t, db, "WIDEN2", 2, 2)
	// Somewhere for the dig to park the blocker, outside both lanes: a flat
	// child of the group. CONVERTED (RULINGS-C2c-run-through §3.2, "the chooser
	// considers every child of the group"): the release used to pass a flat child
	// over, and now offers it, so the child declares another payload — the
	// store's chooser refuses it (payloadAllowedAt) and the dig still parks there
	// (its walk asks no payload fence, D13 Q-SCOPE). The group still has no
	// candidate for this drop, which is the shape under test.
	park := &nodes.Node{Name: "WIDEN2-PARK", Enabled: true, ParentID: &g.grp.ID}
	testutil.MustNoErr(t, db.CreateNode(park), "create a parking child")
	parkOnly := &payloads.Payload{Code: "WIDEN2-PARKONLY"}
	testutil.MustNoErr(t, db.CreatePayload(parkOnly), "create the parking child's payload")
	testutil.MustNoErr(t, db.SetNodePayloads(park.ID, []int64{parkOnly.ID}), "the parking child declares it")
	press, press2 := lineNode(t, db, "WIDEN2-PRESS"), lineNode(t, db, "WIDEN2-PRESS2")
	elsewhere := lineNode(t, db, "WIDEN2-EMPTYSRC")
	testdb.CreateBinAtNode(t, db, "", elsewhere.ID, "WIDEN2-EMPTY")
	swap := c2cAtSpot(t, db, "widen2-swap", sd.Payload.Code, 0,
		c2cSwapPlan(g, press.Name, g.a[1].Name, elsewhere.Name, press2.Name, true))
	c2cPaper(t, db, swap.ID, g.a[1])
	wall := testdb.CreateBinAtNode(t, db, sd.Payload.Code, g.a[0].ID, "BIN-WIDEN2-WALL")
	if wall.ClaimedBy != nil {
		t.Fatal("fixture: the walling bin must be UNCLAIMED, or no dig is wanted")
	}
	fillLaneSlots(t, db, sd.Payload.Code, "WIDEN2-SIB", g.b...) // no other lane can take it

	c2cGatePass(d, g)

	after, steps := c2cReload(t, db, swap)
	if after.WaitIndex != 1 {
		t.Fatalf("the swap left the spot (wait index %d) although no lane of its group could take its drop",
			after.WaitIndex)
	}
	if _, drop := c2cInteriorDrop(steps); drop.Node != g.a[1].Name {
		t.Errorf("the drop was moved to %q with nowhere in the group to go", drop.Node)
	}
	if dig := digParentFor(t, db, g.a[0].Name); dig == nil {
		t.Errorf("no dig was summoned out of %s: the group has no other lane for the drop, the wall in "+
			"front of its slot is unclaimed, and heal first holds for a group with no candidate", g.laneA.Name)
	}
}

// TestWiden_ConcurrentRebindsDoNotDoubleAppend: TWO waiters, ONE free sibling
// slot. Each release is driven directly, one after the other, as two passes
// would; what arbitrates is the release hand's slot claim (DB.ConfirmReleaseDrop:
// the reservation is exclusive per node), so exactly one takes the slot, the
// other does not append, and neither ends up holding a destination the other
// owns. (Two passes over one GROUP cannot interleave in-process since C2c: the
// gate's mutex is the group's, ruling 4. The claim still arbitrates against
// every other chooser.)
func TestWiden_ConcurrentRebindsDoNotDoubleAppend(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, backend := newTestDispatcherWithResolver(t, db)
	g := c2cTwoLaneGroup(t, db, "WIDEN3", 3, 2)
	press, press2 := lineNode(t, db, "WIDEN3-PRESS"), lineNode(t, db, "WIDEN3-PRESS2")
	var swaps []*orders.Order
	for i, pencil := range []*nodes.Node{g.a[1], g.a[2]} {
		empty := lineNode(t, db, fmt.Sprintf("WIDEN3-EMPTYSRC-%d", i))
		testdb.CreateBinAtNode(t, db, "", empty.ID, fmt.Sprintf("WIDEN3-EMPTY-%d", i))
		s := c2cAtSpot(t, db, fmt.Sprintf("widen3-swap-%d", i), sd.Payload.Code, 0,
			c2cSwapPlan(g, press.Name, pencil.Name, empty.Name, press2.Name, true))
		c2cPaper(t, db, s.ID, pencil)
		swaps = append(swaps, s)
	}
	// The pencilled lane is walled and claimed (no dig wanted), and the sibling
	// has exactly ONE free slot for the two of them.
	c2cWidenWall(t, db, sd.Payload.Code, "BIN-WIDEN3-WALL", g.a[0], press)
	fillLaneSlots(t, db, sd.Payload.Code, "WIDEN3-SIB", g.b[1:]...) // leave b[0] free, and only b[0]

	before := len(backend.ReleaseCalls())
	won := 0
	for _, s := range swaps {
		fresh, err := db.GetOrder(s.ID)
		testutil.MustNoErr(t, err, "reload a waiter")
		if !IsGateStaged(fresh) {
			continue
		}
		if _, err := releaseGatedStore(d, db, fresh, g.laneA, candidateFor(t, db, fresh, gateEntryIndexFor(t, fresh))); err == nil {
			won++
		}
	}

	if won != 1 {
		t.Errorf("releases that succeeded = %d, want exactly 1 — two waiters and one free slot, so "+
			"one takes it and the other refuses and retries", won)
	}
	if n := len(backend.ReleaseCalls()) - before; n != 1 {
		t.Errorf("appends = %d, want exactly 1 — a second append into a slot another order booked is "+
			"two bins on one position", n)
	}
	holders, err := reservations.OccupantsOf(db.DB, g.laneB.ID)
	testutil.MustNoErr(t, err, "read the sibling lane's occupancy")
	if len(holders) > 1 {
		t.Errorf("occupancy rows on %s = %v, want at most one — the corridor is single file",
			g.laneB.Name, holders)
	}
}

// newTestDispatcherWithResolver is newTestDispatcher with a REAL resolver.
// The widening's whole question is "does this group have somewhere else", which
// only the resolver can answer; under resolver=nil it returns no-widening and
// every test here would pass by never taking the arm it is about.
func newTestDispatcherWithResolver(t *testing.T, db *store.DB) (*Dispatcher, *testdb.MockTrackingBackend) {
	t.Helper()
	backend := testdb.NewTrackingBackend()
	return NewDispatcher(db, backend, &mockEmitter{}, "core", "shingo.dispatch", &DefaultResolver{DB: db}), backend
}

// fillLaneSlots puts a bin in each named slot, so a lane genuinely has nowhere.
func fillLaneSlots(t *testing.T, db *store.DB, payload, prefix string, slots ...*nodes.Node) {
	t.Helper()
	for i, s := range slots {
		createTestBinAtNode(t, db, payload, s.ID, fmt.Sprintf("BIN-%s-%d", prefix, i))
	}
}

// binsIn counts the bins across the given slots.
func binsIn(t *testing.T, db *store.DB, slots ...*nodes.Node) int {
	t.Helper()
	n := 0
	for _, s := range slots {
		bins, err := db.ListBinsByNode(s.ID)
		testutil.MustNoErr(t, err, "list bins")
		n += len(bins)
	}
	return n
}

// TestWiden_ATierTwoWallIsAReasonToReAim is the shape the whole oracle round is
// for.
//
// ── THE SHAPE, FROM THE PLANT ─────────────────────────────────────────────
//
// demo.yaml 2026-08-30, Lane_06:
//
//	134  complex ASSY  staged  lane-deeper-pending  AMR-03  -> SMN_016 (depth 1)
//	132  move    ASSY  staged  lane-deeper-pending  AMR-02  -> SMN_017 (depth 2)
//	128  complex PANEL-A queued  swap-hold                  -> SMN_018 (depth 3)
//
// 128 is the deepest and cannot move: it waits on a swap partner that waits on
// PANEL-A the plant does not have. stillComingToLane counts it — correctly, it
// is not dispatched and is genuinely still coming — so 132 is walled by Tier 2,
// and 134 is walled behind 132. Two robots stood 39 minutes while their group
// had free lanes.
//
// Since C2c the Tier-2 wall is a lane fact asked AFTER the group's choice, and a
// refusal sends the drop to the group's next lane (rulings 3 and 4; DESIGN-C2c
// §7.4). Outcome kept: the drop lands in the free sibling, and the wait names it.
func TestWiden_ATierTwoWallIsAReasonToReAim(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	g := c2cTwoLaneGroup(t, db, "WIDEN4", 3, 2)
	press, press2 := lineNode(t, db, "WIDEN4-PRESS"), lineNode(t, db, "WIDEN4-PRESS2")
	elsewhere := lineNode(t, db, "WIDEN4-EMPTYSRC")
	testdb.CreateBinAtNode(t, db, "", elsewhere.ID, "WIDEN4-EMPTY")

	// THE DEEPER STORE THAT CANNOT PLACE. Not dispatched — which is exactly what
	// makes it a legitimate blocker (stillComingToLane's first arm) and exactly
	// what makes the wait it creates unbounded.
	stuck := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = g.a[2].Name
		o.Status = "queued"
		o.PayloadCode = sd.Payload.Code
	})
	testutil.MustNoErr(t, db.ReserveSlot(g.a[2].ID, stuck.ID), "the deeper store holds its slot (a plan alone walls nothing)")

	swap := c2cAtSpot(t, db, "widen4-swap", sd.Payload.Code, 0,
		c2cSwapPlan(g, press.Name, g.a[1].Name, elsewhere.Name, press2.Name, true))
	c2cPaper(t, db, swap.ID, g.a[1])

	// The refusal really is Tier 2 and not something else — if it were, this test
	// would be about a different arm and would pass for the wrong reason.
	v, err := d.gateEntryVerdict(g.laneA, swap, g.a[1], false)
	testutil.MustNoErr(t, err, "classify the swap at its pencilled lane")
	if v.Admitted() || v.Cause() != CauseLaneDeeperPending {
		t.Fatalf("fixture: want a %q refusal in lane A, got admitted=%v cause=%q — this test is about the "+
			"ordering wall, not whichever refusal happened instead",
			CauseLaneDeeperPending, v.Admitted(), v.Cause())
	}
	if n := binsIn(t, db, g.b...); n != 0 {
		t.Fatalf("fixture: the sibling lane must be empty, has %d bins", n)
	}

	c2cGatePass(d, g)

	after, steps := c2cReload(t, db, swap)
	_, drop := c2cInteriorDrop(steps)
	if lane := c2cLaneOf(t, db, drop.Node); lane != g.laneB.ID || after.WaitIndex < 2 {
		t.Fatalf("the drop is at %q (lane %d) at wait index %d (cause %q), want it released into the free "+
			"sibling %s. It is walled by a deeper store that cannot place, no dig would help, and its group "+
			"has an empty aisle — standing there is the 39-minute wait this round exists to end",
			drop.Node, lane, after.WaitIndex, after.QueueCause, g.laneB.Name)
	}
	if wl, ok := c2cWaitLaneAt(steps); !ok || wl != g.laneB.ID {
		t.Errorf("the lane wait names lane %d, want %d — the wait and the entry it gates must name one "+
			"corridor, or the append takes the wrong lane's occupancy", wl, g.laneB.ID)
	}
	if aErr := d.assertEachWaitGatesItsEntry(steps); aErr != nil {
		t.Errorf("the released plan does not satisfy the splice-time guard: %v", aErr)
	}
}

// TestWiden_ATransientRefusalIsReAimedWhileTheGroupHasRoom was
// TestWiden_ATransientRefusalIsNotReAimed, and it FLIPS with C2c (D13 F4).
//
// A lane-occupied refusal means a robot is INSIDE the corridor right now, and
// the old rule held the waiter there because occupancy is transient: re-aiming
// spends a drive to skip a wait that was already ending (refusalCanWiden's named
// set). Ruling 3 decides it the other way: "a refused mouth is the next
// candidate, not a wait, while the group has one". The cost the old rule avoided
// is accepted and measured in the pair (re-aims per run). The event-driven twin
// is TestC2cPin_AWaiterIsReleasedByABinLeavingAnotherLaneOfItsGroup.
func TestWiden_ATransientRefusalIsReAimedWhileTheGroupHasRoom(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	g := c2cTwoLaneGroup(t, db, "WIDEN5", 2, 2)
	press, press2 := lineNode(t, db, "WIDEN5-PRESS"), lineNode(t, db, "WIDEN5-PRESS2")
	elsewhere := lineNode(t, db, "WIDEN5-EMPTYSRC")
	testdb.CreateBinAtNode(t, db, "", elsewhere.ID, "WIDEN5-EMPTY")

	// SOMEBODY IS IN LANE A'S CORRIDOR, and stays.
	occupant := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = g.a[0].Name
		o.Status = "in_transit"
	})
	if ok, oErr := reservations.AcquireOccupancy(db.DB, occupant.ID, g.laneA.ID); oErr != nil || !ok {
		t.Fatalf("fixture: put a robot inside lane A: ok=%v err=%v", ok, oErr)
	}
	swap := c2cAtSpot(t, db, "widen5-swap", sd.Payload.Code, 0,
		c2cSwapPlan(g, press.Name, g.a[1].Name, elsewhere.Name, press2.Name, true))
	c2cPaper(t, db, swap.ID, g.a[1])

	c2cGatePass(d, g)

	after, steps := c2cReload(t, db, swap)
	_, drop := c2cInteriorDrop(steps)
	if lane := c2cLaneOf(t, db, drop.Node); lane != g.laneB.ID || after.WaitIndex < 2 {
		t.Errorf("the drop is at %q (lane %d) at wait index %d (cause %q), want it released into %s: lane "+
			"A refused it lane-occupied, and a refused mouth is the next candidate while the group has one",
			drop.Node, lane, after.WaitIndex, after.QueueCause, g.laneB.Name)
	}
}

// ── P7: TWO FLAT BUGS IN THE WIDEN ARM (DESIGN-C2c §0.5, §9 P7) ───────────
//
// Found by C2c's P0 (2026-10-10) reading widenDropoffToGroup; neither was
// exercised by any test. Both die with the arm when C2c deletes it (one chooser,
// ruling 1), so they are pinned here and reported as flat bugs the deletion
// fixes rather than fixed separately. The fixture is C2c's (c2c_group_choice_pins).

// TestWiden_ACarriedEmptyIsAskedAsAnEmpty is P7 (i), §0.5(a). The swap carries
// an EMPTY from the press into G (its drop's payload is "": plan.DropPayload)
// and lane A filled while it drove. Lane B is declared for another payload Q,
// which refuses a full of P but not an empty (payloadAllowedAt: "an empty
// carrier has no payload to check against a declaration"). So the group has
// room for this drop, in B. C2c ruling 1's hard rules ask with the drop's own
// payload and carrier (DESIGN-C2c §7.1 `need`).
//
// RED AT BASE, by reading: the widen asks Resolve(G) with order.PayloadCode (P,
// the full's payload) and UnknownBinType, B refuses P, A is full, and the swap
// waits at G's spot under gate-rebind-unavailable — the C2b Q4 leak, at release.
func TestWiden_ACarriedEmptyIsAskedAsAnEmpty(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	g := c2cTwoLaneGroup(t, db, "C2CP7A", 2, 2)
	other := &payloads.Payload{Code: "C2CP7A-Q"}
	testutil.MustNoErr(t, db.CreatePayload(other), "create the other payload")
	testutil.MustNoErr(t, db.SetNodePayloads(g.laneB.ID, []int64{other.ID}), "declare lane B for Q")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, g.a[0].ID, "C2CP7A-A0")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, g.a[1].ID, "C2CP7A-A1")
	press, press2 := lineNode(t, db, "C2CP7A-PRESS"), lineNode(t, db, "C2CP7A-PRESS2")
	fullSrc := lineNode(t, db, "C2CP7A-FULLSRC")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, fullSrc.ID, "C2CP7A-FULL")
	plan := c2cSwapPlan(g, press.Name, g.a[0].Name, fullSrc.Name, press2.Name, false)
	plan[1].Empty = true // the carried bin is the press's empty
	swap := c2cAtSpot(t, db, "c2cp7a-swap", sd.Payload.Code, 0, plan)

	c2cGatePass(d, g)

	after, steps := c2cReload(t, db, swap)
	_, drop := c2cInteriorDrop(steps)
	if lane := c2cLaneOf(t, db, drop.Node); lane != g.laneB.ID || after.WaitIndex < 2 {
		t.Fatalf("the carried empty's drop is at %q (lane %d) at wait index %d (cause %q), want it in lane B "+
			"(%d): B refuses a full of %s, not an empty, and the release asked as the full",
			drop.Node, lane, after.WaitIndex, after.QueueCause, g.laneB.ID, sd.Payload.Code)
	}
}

// TestWiden_AWidenNeverAnswersTheLaneItWalled is P7 (ii), §0.5(b). Order 266's
// shape with one fact added: lane A also holds a full of the swap's payload, so
// the group's like-kind answer IS lane A. The re-bind sets A aside (its only
// slot walls the swap's own empty at A1) and widens; the widen asks the group
// without setting A aside, the group answers A, and "same lane" reads as "no
// widening". B is empty the whole time. C2c ruling 1: never burying the order's
// own later pickup is a hard rule, applied before the group's algorithm
// (ExcludeLanes from every later pickup, DESIGN-C2c §7.1), so the answer is B.
//
// RED AT BASE, by reading: the swap waits at G's spot under
// gate-rebind-unavailable although a sibling has room.
func TestWiden_AWidenNeverAnswersTheLaneItWalled(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	g := c2cTwoLaneGroup(t, db, "C2CP7B", 3, 2)
	testdb.CreateBinAtNode(t, db, "", g.a[1].ID, "C2CP7B-EMPTY")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, g.a[2].ID, "C2CP7B-SAMEKIND")
	press, press2 := lineNode(t, db, "C2CP7B-PRESS"), lineNode(t, db, "C2CP7B-PRESS2")
	swap := c2cAtSpot(t, db, "c2cp7b-swap", sd.Payload.Code, 0,
		c2cSwapPlan(g, press.Name, g.a[0].Name, g.a[1].Name, press2.Name, true))

	c2cGatePass(d, g)

	after, steps := c2cReload(t, db, swap)
	_, drop := c2cInteriorDrop(steps)
	if lane := c2cLaneOf(t, db, drop.Node); lane != g.laneB.ID || after.WaitIndex < 2 {
		t.Fatalf("the drop is at %q (lane %d) at wait index %d (cause %q), want it in lane B (%d): A walls the "+
			"swap's own pickup at %s, and the group's like-kind preference for A is asked after that rule, "+
			"not instead of it", drop.Node, lane, after.WaitIndex, after.QueueCause, g.laneB.ID, g.a[1].Name)
	}
	if red := selfBurial(t, db, steps); len(red) != 0 {
		t.Errorf("%v", red)
	}
}
