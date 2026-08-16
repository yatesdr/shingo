//go:build docker

package dispatch

import (
	"testing"

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
	child := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.ParentOrderID = &parent.ID
		o.Status = StatusPending
	})

	if !d.laneLock.TryLock(lane, parent.ID) {
		t.Fatal("TryLock failed")
	}
	if !d.laneLock.IsLocked(lane) {
		t.Fatal("precondition: the lane must actually be locked, or this test is the old one again")
	}

	d.HandleChildOrderFailure(parent.ID, child.ID)

	if d.laneLock.IsLocked(lane) {
		t.Error("lane still locked after HandleChildOrderFailure; want released")
	}
}
