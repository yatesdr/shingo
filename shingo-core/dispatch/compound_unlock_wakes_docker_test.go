//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestCompound_UnlockReleasesEveryLaneItHeldAndWakesEachOne — a dig that held two
// lanes must free BOTH, and every lane it frees is a lane-clearing event.
//
// ── WHAT WAS WRONG ────────────────────────────────────────────────────────
//
// unlockLaneForCompound resolved "which lane does this dig hold" by walking the
// parent's children and taking the FIRST one's source lane. Three faults in one
// derivation:
//
//   - it `return`ed inside the loop, so a parent holding locks on more than one
//     lane released one and leaked the rest (F-19 measured order 1 holding three
//     at once);
//   - after a re-plan the first child belongs to a superseded generation and
//     names a lane the dig no longer holds;
//   - and the release-by-owner FALLBACK underneath it — the path taken when the
//     walk failed — dropped the locks and evaluated nothing at all, so every
//     dweller behind them lost its only releaser.
//
// The lane a dig holds is the reservation row. Asking the owner answers all three.
//
// ── WHY THE DWELLER IS THE ASSERTION ──────────────────────────────────────
//
// "Both locks are gone" alone is green while the robots behind them stand there
// forever: a dropped dig lock is the one lane-clearing event the gate's trigger
// set cannot produce for itself, because every other trigger fires from a bin or
// an order changing and all of those have already fired by the time a dig
// releases. So the test asserts a tail append on the SECOND lane — the one the
// old walk never reached.
//
// ── MUTATIONS RUN (all three fire) ────────────────────────────────────────
//
//  1. restore the children-walk + `return` (release only the first child's lane)
//     → (a) lane B still locked and (c) the dweller never gets its tail.
//  2. keep the owner-scoped release but drop the EvaluateLaneReleases loop
//     → (c) alone: both lanes free, dweller still dwelling. This is the fallback
//     arm's old behaviour exactly, and it is the half that is invisible from the
//     lock table. It also fires TestWindow3_UnclaimedMouthBinIsDugOutWithNobodyAsking.
//  3. ignore the snapshot and read the lanes inside unlockLaneForCompound instead
//     → this test still passes (nothing terminalizes the parent here), and
//     TestWindow3_… plus TestMultiGate_SecondGateGetsWindow3sRescue both fail.
//     That asymmetry is the point and is why the snapshot is a separate call: on
//     the real completion path the parent is terminalized FIRST, which deletes
//     its reservations, so a lane read afterwards finds nothing and wakes nobody.
//     This test cannot catch that alone; the two that drive the full teardown can.
func TestCompound_UnlockReleasesEveryLaneItHeldAndWakesEachOne(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	// Two MARKED lanes in one group. laneA is where the dig's children work;
	// laneB is the second lane the same dig locked — invisible to a children walk.
	laneA, laneB, a, b, _ := healLaneFixture(t, db, "TWOLANE")
	testutil.MustNoErr(t,
		db.SetNodeProperty(laneB.ID, PropLaneGatePoint, "TWOLANE-PARK-WAIT"), "mark laneB")
	laneB, _ = db.GetNode(laneB.ID)
	line := lineNode(t, db, "TWOLANE-LINE")

	// The dig: a compound parent whose children source out of laneA.
	parent := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = StatusReshuffling
	})
	child := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.ParentOrderID = &parent.ID
		o.Sequence = 1
		o.Status = StatusPending
		o.SourceNode = a[0].Name
		o.DeliveryNode = b[0].Name
	})
	_ = child

	// It holds BOTH lanes. That is the state the walk cannot describe.
	if !d.laneLock.TryLock(laneA.ID, parent.ID) {
		t.Fatal("precondition: the dig must hold laneA")
	}
	if !d.laneLock.TryLock(laneB.ID, parent.ID) {
		t.Fatal("precondition: the dig must hold laneB")
	}

	// A robot dwelling at laneB's mark, aimed at an empty slot, refused by nothing
	// but the dig lock above it.
	dweller := stageGatedStore(t, db, d, line, b[1], nil)
	if !IsGateStaged(dweller) {
		t.Fatalf("the dweller is not gate-staged (wait_index=%d vendor=%q) — the fixture is not "+
			"reproducing a robot parked at a mark", dweller.WaitIndex, dweller.VendorOrderID)
	}
	markStaged(t, db, dweller.ID)
	if n := appendsTo(backend, dweller.VendorOrderID); n != 0 {
		t.Fatalf("the dweller already has %d tail append(s) — it is not actually dwelling", n)
	}

	// ── THE DIG ENDS. The pair every teardown path uses: snapshot the lanes
	// while the rows still exist, then release and wake. ──
	heldLanes := d.digLanesHeld(parent.ID)
	if len(heldLanes) != 2 {
		t.Fatalf("the snapshot saw %d lane(s), want 2 — the teardown cannot wake a lane it never "+
			"knew the dig held", len(heldLanes))
	}
	d.unlockLaneForCompound(parent.ID, heldLanes)

	// (a) EVERY LANE IT HELD IS FREE, not just the first child's.
	if d.laneLock.IsLocked(laneB.ID) {
		t.Error("laneB is still dig-locked after the compound was unlocked. The dig held two lanes " +
			"and the release found one of them — the second is held by an order that no longer " +
			"exists as a dig, and nothing will ever release it")
	}
	if d.laneLock.IsLocked(laneA.ID) {
		t.Error("laneA is still dig-locked after the compound was unlocked")
	}

	// (b) NOTHING IS LEFT BEHIND. The owner-scoped release is the whole cleanup.
	if n := gateMouthRows(t, db, laneA.ID); n != 0 {
		t.Errorf("laneA still carries %d mouth row(s) owned by the finished dig", n)
	}
	if n := gateMouthRows(t, db, laneB.ID); n != 0 {
		t.Errorf("laneB still carries %d mouth row(s) owned by the finished dig", n)
	}

	// (c) THE DWELLER BEHIND THE SECOND LANE WAS WOKEN. This is the assertion the
	// lock table cannot make: releasing a lock frees the lane, and only an
	// evaluation turns that into a robot moving.
	if n := appendsTo(backend, dweller.VendorOrderID); n == 0 {
		after, _ := db.GetOrder(dweller.ID)
		t.Fatalf("the dweller at laneB never got its tail — status %s, wait_index %d. The dig's "+
			"lock was the only thing refusing it, and the lock dropping is the last event in the "+
			"dig's life: every bin and order event it emitted was consumed while the lock was "+
			"still held, so if the unlock does not evaluate, nothing ever will",
			after.Status, after.WaitIndex)
	}
}
