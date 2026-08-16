//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
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

// TestCancelDoor_WakesLanesWhenAnOperatorCancelsADig is the mutation the test
// above cannot run, driven through the path that actually terminalizes.
//
// ── THE GAP THIS CLOSES ───────────────────────────────────────────────────
//
// Read mutation 3 in the header above: "ignore the snapshot and read the lanes
// inside unlockLaneForCompound instead → THIS TEST STILL PASSES (nothing
// terminalizes the parent here)." That asymmetry was noted and left, because the
// two window-3 tests drive the full completion teardown and do catch it.
//
// The OPERATOR CANCEL path does not go through completion. It went:
//
//	lifecycle.CancelOrder(parent)      ← terminalizes; deletes the parent's
//	                                     reservations in the same transaction
//	cancelCompoundChildren(parent)     ← took its lane snapshot HERE, as its
//	                                     first line — after the delete
//
// The rows ARE the lane lock, so that snapshot read an already-emptied set every
// time. unlockLaneForCompound then iterated nothing, and every dweller behind
// every lane the dig held waited out the 60-second floor. Invisible from the
// lock table, because the locks really were released — what was lost is the
// evaluation, which is the half that turns a freed lane into a robot moving.
//
// ── THE FIXTURE IS THE SECOND LANE, DELIBERATELY ──────────────────────────
//
// One lane would pass under a snapshot bug that returned a partial answer. The
// dweller waits behind laneB — the lane no children walk reaches and the one a
// late snapshot loses along with the first.
//
// MUTATION (verified): move the snapshot back inside cancelCompoundChildren —
// i.e. take it after lifecycle.CancelOrder — and the dweller never gets its
// tail, naming the lane and the dweller's status.
//
// A MUTATION THAT DOES NOT FIRE, AND IS WORTH KNOWING: reversing the other
// ordering — cancelling the CHILDREN before the parent — passes this test, and
// passes the ENTIRE dispatch docker suite. That ordering is documented at length
// at CancelOrderWithCascade and at HandleOrderCancel, with a measured failure
// behind it (the redrive admits the next leg, hits a reachability refusal,
// DISSOLVES the dig, and the terminal arm races the parent's own cancel to a
// `failed` finish — an operator asked for cancelled and got failed). Nothing
// enforces it. Reproducing it needs the redrive machinery to fire against a real
// dig with a next leg and a reachability refusal, which is a fixture of its own
// and is NOT built here. Recorded rather than claimed: the snapshot ordering is
// pinned, the cancel ordering is documented-only.
func TestCancelDoor_WakesLanesWhenAnOperatorCancelsADig(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	laneA, laneB, a, b, _ := healLaneFixture(t, db, "CANCELWAKE")
	testutil.MustNoErr(t,
		db.SetNodeProperty(laneB.ID, PropLaneGatePoint, "CANCELWAKE-PARK-WAIT"), "mark laneB")
	laneB, _ = db.GetNode(laneB.ID)
	line := lineNode(t, db, "CANCELWAKE-LINE")

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

	if !d.laneLock.TryLock(laneA.ID, parent.ID) {
		t.Fatal("precondition: the dig must hold laneA")
	}
	if !d.laneLock.TryLock(laneB.ID, parent.ID) {
		t.Fatal("precondition: the dig must hold laneB")
	}

	dweller := stageGatedStore(t, db, d, line, b[1], nil)
	if !IsGateStaged(dweller) {
		t.Fatalf("the dweller is not gate-staged (wait_index=%d vendor=%q)",
			dweller.WaitIndex, dweller.VendorOrderID)
	}
	markStaged(t, db, dweller.ID)
	if n := appendsTo(backend, dweller.VendorOrderID); n != 0 {
		t.Fatalf("the dweller already has %d tail append(s) — it is not actually dwelling", n)
	}

	// ── THE OPERATOR CANCELS THE DIG, through the one cancel door. ──
	d.CancelOrderWithCascade(parent, parent.StationID, "cancelled by operator")

	// (a) THE CHILD WAS CASCADED. Without it the leg keeps its vendor order and
	// its bin claim while the parent is gone — the orphan the UI door produced.
	gotChild, err := db.GetOrder(child.ID)
	testutil.MustNoErr(t, err, "reload the child")
	if !protocol.IsTerminal(gotChild.Status) {
		t.Errorf("the dig's leg is %s after its parent was cancelled. A leg outliving its parent "+
			"still has a live vendor order driving a robot and a bin still claimed", gotChild.Status)
	}

	// (b) BOTH LANES ARE FREE.
	if d.laneLock.IsLocked(laneA.ID) || d.laneLock.IsLocked(laneB.ID) {
		t.Errorf("lanes still dig-locked after the cancel (A=%v B=%v) — held by an order that no "+
			"longer exists", d.laneLock.IsLocked(laneA.ID), d.laneLock.IsLocked(laneB.ID))
	}

	// (c) AND THE DWELLER BEHIND THE SECOND LANE WAS WOKEN. This is the one the
	// late snapshot lost: the locks were released either way, and only the
	// evaluation turns that into a robot moving.
	if n := appendsTo(backend, dweller.VendorOrderID); n == 0 {
		after, _ := db.GetOrder(dweller.ID)
		t.Fatalf("the dweller at laneB never got its tail after the dig was CANCELLED — status %s, "+
			"wait_index %d. The cancel path terminalizes the parent, which deletes the reservations "+
			"that are the lane lock; a snapshot taken after that reads empty, and an empty snapshot "+
			"is indistinguishable from 'held nothing'. The lane is free and nobody was told.",
			after.Status, after.WaitIndex)
	}
}
