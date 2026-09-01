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
	"shingocore/store/reservations"
)

// lane_gate_widen_docker_test.go — the oracle widening to the group.
//
// Three claims, and the first is the one nothing in the suite exercised before:
// a lane walled by a CLAIMED blocker, where no dig would help, with a free
// sibling in the same group. Until the widening there was no path out of that
// state except waiting for the claim to resolve, so the true arm of "should this
// order go somewhere else" had never once been taken.

// TestWiden_ClaimedBlockerWithAFreeSiblingWidens is the missing RED test.
//
// ── THE SHAPE ─────────────────────────────────────────────────────────────
//
// A dweller is aimed into a lane it cannot use, and a dig is NOT the answer: the
// bin walling it is hard-claimed by a live order, so somebody is already coming
// for it (acceptanceDigNeeded's rule 3 — "a hard claim is a robot in motion, so
// the bin is leaving anyway"). Refusing the dig is right. Standing still is not,
// when a sibling lane in the same group has room.
//
// RED at base: the release refuses with gate-rebind-unavailable and the order
// keeps dwelling. Nothing in the tree took it anywhere else.
func TestWiden_ClaimedBlockerWithAFreeSiblingWidens(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcherWithResolver(t, db)

	wall, park, w, p, bp := clearLaneFixture(t, db, "WIDEN1")
	line := lineNode(t, db, "WIDEN1-LINE")

	// The dweller is aimed at the wall lane's deep slot.
	deep := stageDeeperBlocker(t, db, d, line, w[2], "widen1-deep")
	dweller := stageGatedStore(t, db, d, line, w[1], func(o *orders.Order) { o.PayloadCode = bp.Code })
	if !IsGateStaged(dweller) {
		t.Fatalf("fixture: the dweller must be gate-staged (wait_index=%d)", dweller.WaitIndex)
	}
	markStaged(t, db, dweller.ID)
	placeDeeperBlocker(t, db, d, deep.ID, w[2].Name)

	// FILL the wall lane so there is nowhere in it, and CLAIM the blocker so no
	// dig is wanted. Both halves matter: an unclaimed wall would summon a dig,
	// which is the correct answer and a different test.
	blocker := createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-WIDEN1-WALL")
	claimant := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.SourceNode = w[0].Name
		o.DeliveryNode = line.Name
		o.Status = "in_transit"
	})
	// RESERVE THEN CLAIM. A bare claim is refused by design: claimed_by and the
	// bin reservation are coupled, so a hard claim with no reservation behind it
	// would leave a bin nothing can release (store/bins.go).
	testdb.ReserveBin(t, db, claimant.ID, blocker.ID)
	testutil.MustNoErr(t, db.ClaimBin(blocker.ID, claimant.ID), "a live order claims the wall bin")

	// The sibling lane is EMPTY the whole time.
	if n := binsIn(t, db, p...); n != 0 {
		t.Fatalf("fixture: the sibling lane must start empty, has %d bins", n)
	}

	fresh, err := db.GetOrder(dweller.ID)
	testutil.MustNoErr(t, err, "reload the dweller")
	entryIdx := gateEntryIndexFor(t, fresh)
	if err := d.releaseGatedOrder(fresh, wall, candidateFor(t, db, fresh, entryIdx)); err != nil {
		t.Fatalf("the release should have widened rather than refused: %v", err)
	}

	after, err := db.GetOrder(dweller.ID)
	testutil.MustNoErr(t, err, "reload after the release")
	landed, err := db.GetNodeByDotName(after.DeliveryNode)
	testutil.MustNoErr(t, err, "resolve where it landed")
	newLane, err := db.LaneForNode(landed.ID)
	testutil.MustNoErr(t, err, "lane of the new destination")
	if newLane == nil || newLane.ID != park.ID {
		t.Fatalf("delivery_node = %q (lane %s), want a slot in the free sibling %s — a lane walled "+
			"by a CLAIMED blocker cannot be dug open, and standing still while the group has room "+
			"is the state the widening exists to end",
			after.DeliveryNode, nodeName(newLane), park.Name)
	}

	// AND THE WAIT MOVED WITH IT. A stale WaitLane takes the wrong lane's
	// occupancy at append — the robot enters one corridor while another lane's
	// row says it is inside.
	var steps []resolvedStep
	testutil.MustNoErr(t, json.Unmarshal([]byte(after.StepsJSON), &steps), "parse the patched plan")
	w0, ok := waitAt(steps, 0)
	if !ok || w0.WaitLane != park.ID {
		t.Errorf("the wait still names lane %d, want the lane it was re-aimed into (%d) — the wait "+
			"and the entry it gates must name one corridor", w0.WaitLane, park.ID)
	}
	if aErr := d.assertEachWaitGatesItsEntry(steps); aErr != nil {
		t.Errorf("the patched plan does not satisfy the splice-time guard: %v", aErr)
	}
}

// TestWiden_ADigThatWouldHelpIsPreferredToWidening is the heal-first guard, and
// it is the half that keeps the widening from becoming a way to leave walls
// standing.
//
// Same fixture, one fact changed: the walling bin is UNCLAIMED, so a dig would
// open the lane. The plan named that lane for a reason and the bins in front of
// the target are the actual problem — widening past them serves this order and
// leaves the wall for whoever comes next. So the release must REFUSE, which is
// what lets the pass summon the excavation (F-11).
func TestWiden_ADigThatWouldHelpIsPreferredToWidening(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcherWithResolver(t, db)

	wall, _, w, p, bp := clearLaneFixture(t, db, "WIDEN2")
	line := lineNode(t, db, "WIDEN2-LINE")

	deep := stageDeeperBlocker(t, db, d, line, w[2], "widen2-deep")
	dweller := stageGatedStore(t, db, d, line, w[1], func(o *orders.Order) { o.PayloadCode = bp.Code })
	if !IsGateStaged(dweller) {
		t.Fatalf("fixture: the dweller must be gate-staged (wait_index=%d)", dweller.WaitIndex)
	}
	markStaged(t, db, dweller.ID)
	placeDeeperBlocker(t, db, d, deep.ID, w[2].Name)

	// UNCLAIMED wall — a dig is warranted — and the deeper slot filled so the
	// lane genuinely has nowhere.
	blocker := createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-WIDEN2-WALL")
	if blocker.ClaimedBy != nil {
		t.Fatal("fixture: the walling bin must be UNCLAIMED, or no dig is wanted and this is the " +
			"other test")
	}
	// NOTHING ELSE IS FILLED. The dweller's own slot (w[1]) stays EMPTY, which is
	// acceptanceDigNeeded's rule 2 — a store whose own slot is taken has a
	// different problem and the wall is not it — and the slot is UNREACHABLE
	// behind the wall bin, so the in-lane finder still has nowhere to offer.
	_ = p

	fresh, err := db.GetOrder(dweller.ID)
	testutil.MustNoErr(t, err, "reload the dweller")
	entryIdx := gateEntryIndexFor(t, fresh)
	if err := d.releaseGatedOrder(fresh, wall, candidateFor(t, db, fresh, entryIdx)); err == nil {
		t.Fatal("the release WIDENED past a wall a dig would have cleared. The plan named this lane, " +
			"the bins in front of the target are the problem, and going somewhere else leaves the " +
			"wall standing for the next order")
	}

	after, err := db.GetOrder(dweller.ID)
	testutil.MustNoErr(t, err, "reload after the refusal")
	landed, err := db.GetNodeByDotName(after.DeliveryNode)
	testutil.MustNoErr(t, err, "resolve where it is aimed")
	stillLane, err := db.LaneForNode(landed.ID)
	testutil.MustNoErr(t, err, "lane of the destination")
	if stillLane == nil || stillLane.ID != wall.ID {
		t.Errorf("delivery_node = %q (lane %s), want it still aimed into %s — heal first",
			after.DeliveryNode, nodeName(stillLane), wall.Name)
	}
}

// TestWiden_ConcurrentRebindsDoNotDoubleAppend is the concurrency claim finch's
// no-new-lock reading rests on.
//
// TWO dwellers, ONE free sibling slot. The per-lane serializer does not help
// here — they are released from different passes over the same lane — so what
// arbitrates is the transactional slot claim: claimStoreSlot goes through the
// reservation layer, which is exclusive per node, and the loser gets an error,
// refuses to release, and stays a candidate for the next pass.
//
// The property is NOT "both succeed". It is that exactly one takes the slot, the
// other does not append, and neither ends up holding a destination the other
// owns.
func TestWiden_ConcurrentRebindsDoNotDoubleAppend(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, backend := newTestDispatcherWithResolver(t, db)

	wall, park, w, p, bp := clearLaneFixture(t, db, "WIDEN3")
	line := lineNode(t, db, "WIDEN3-LINE")

	deep := stageDeeperBlocker(t, db, d, line, w[2], "widen3-deep")
	dwellerA := stageGatedStore(t, db, d, line, w[1], func(o *orders.Order) { o.PayloadCode = bp.Code })
	markStaged(t, db, dwellerA.ID)
	placeDeeperBlocker(t, db, d, deep.ID, w[2].Name)
	dwellerB := stageGatedStore(t, db, d, line, w[0], func(o *orders.Order) { o.PayloadCode = bp.Code })
	markStaged(t, db, dwellerB.ID)

	// The wall lane is full and claimed (no dig wanted), and the sibling has
	// exactly ONE free slot for the two of them.
	blocker := createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-WIDEN3-WALL")
	claimant := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.SourceNode = w[0].Name
		o.DeliveryNode = line.Name
		o.Status = "in_transit"
	})
	testdb.ReserveBin(t, db, claimant.ID, blocker.ID)
	testutil.MustNoErr(t, db.ClaimBin(blocker.ID, claimant.ID), "a live order claims the wall bin")
	fillLaneSlots(t, db, bp.Code, "WIDEN3-SIB", p[1:]...) // leave p[0] free, and only p[0]

	before := len(backend.ReleaseCalls())
	won := 0
	for _, id := range []int64{dwellerA.ID, dwellerB.ID} {
		fresh, err := db.GetOrder(id)
		testutil.MustNoErr(t, err, "reload a dweller")
		if !IsGateStaged(fresh) {
			continue
		}
		if err := d.releaseGatedOrder(fresh, wall, candidateFor(t, db, fresh, gateEntryIndexFor(t, fresh))); err == nil {
			won++
		}
	}

	if won != 1 {
		t.Errorf("releases that succeeded = %d, want exactly 1 — two dwellers and one free slot, so "+
			"one takes it and the other refuses and retries", won)
	}
	if n := len(backend.ReleaseCalls()) - before; n != 1 {
		t.Errorf("appends = %d, want exactly 1 — a second append into a slot another order booked is "+
			"two bins on one position", n)
	}
	// And nobody is holding a slot they did not win.
	holders, err := reservations.OccupantsOf(db.DB, park.ID)
	testutil.MustNoErr(t, err, "read the sibling lane's occupancy")
	if len(holders) > 1 {
		t.Errorf("occupancy rows on %s = %v, want at most one — the corridor is single file",
			park.Name, holders)
	}
	_ = protocol.ActionWait
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
