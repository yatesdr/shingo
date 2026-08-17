//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// R04-2: HandleChildOrderFailure fires once (engine wiring, on the failure
// event) with no retry, so it must not strand the lane lock. The release is
// deferred and works off a snapshot taken BEFORE the parent is terminalized,
// because terminalization deletes the parent's reservations and a lane read
// after it would find nothing and wake nobody.
//
// ── THIS TEST USED TO ASSERT NOTHING, AND THE DEPTH-1 EXEMPTION IS WHY ────
//
// It was written as `TryLock(4242, 9999)` — a lane node and a parent order that
// both did not exist — to reach the handler's unresolvable-parent early return,
// and it passed. It passed because `reservations` FKs `order_id` to orders and
// `node_id` to nodes, so that row could never be inserted; AcquireLanes never
// tried, because it asked laneDepth1Exempt FIRST, counted zero child slots
// under node 4242, and skipped the lane. TryLock returned true having written
// nothing, and `IsLocked` then correctly reported a lock that had never been
// taken. Removing the exemption turned it red on the FK, which is how it was
// found.
//
// ── SO WHAT IS PINNED NOW, STATED PLAINLY ────────────────────────────────
//
// The RELEASE, not the error arm. A reservation cannot be owned by an order
// that does not exist, so the unresolvable-parent path is not constructible
// against the real schema at all — and a fixture that has to break a foreign
// key to reach a branch is telling you the branch is defensive rather than
// reachable. The branch stays (it costs nothing and the handler has no retry);
// what this test now holds down is that a compound whose child fails does not
// leave its lane locked, which is the floor behaviour R04-2 was about.
func TestHandleChildOrderFailure_ReleasesTheLane(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewFailingBackend())

	testdb.SetupStandardData(t, db)
	lane := mirrorLane(t, db, "CLR-LANE", 3)
	parent := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = StatusReshuffling
	})
	// A leg that ACTUALLY DIED, plus one still pending behind it. The old
	// fixture had a single pending child and passed it in as the failed one,
	// which meant nothing was ever cancelled and nothing closed.
	deadLeg := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.ParentOrderID = &parent.ID
		o.Status = StatusFailed
		o.ErrorDetail = "the robot stopped responding"
	})
	child := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.ParentOrderID = &parent.ID
		o.Status = StatusPending
	})
	_ = child

	if !d.laneLock.TryLock(lane, parent.ID) {
		t.Fatal("TryLock failed")
	}
	if !d.laneLock.IsLocked(lane) {
		t.Fatal("precondition: the lane must actually be locked, or this test is the old one again")
	}

	d.HandleChildOrderFailure(parent.ID, deadLeg.ID)

	// THE DISPOSITION IS THE SECOND STEP AND IT IS WHAT RELEASES THE LANE.
	// HandleChildOrderFailure used to terminalize the parent and drop the lane
	// itself; under gate 1 (§R.91) it only closes the chapter, and the corridor
	// stays the parent's until the cancelled legs have landed — a lane released
	// with a still-moving leg inside it is the re-burial window Hold A exists to
	// close. In the plant the cancel above emits and the engine's chapter-end arm
	// makes this call on another goroutine.
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "dispose of the closed chapter")

	if d.laneLock.IsLocked(lane) {
		t.Error("lane still locked after the chapter closed; want released")
	}
	after, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload parent")
	if after.Status != StatusQueued {
		t.Errorf("parent status = %q, want %q — the demand survives its dig's failure",
			after.Status, StatusQueued)
	}
}
