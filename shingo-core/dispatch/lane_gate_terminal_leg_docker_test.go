//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// NOTHING IS APPENDED TO A LEG THAT HAS ENDED.
//
// A gated leg waits at the lane's mark with a vendor order that has no tail. A
// cancel can land while it waits; the lane then opens, and the append funnel
// (appendGateTail) used to reload the row and ask IsGateStaged, which reads the
// plan and the wait index, never the status. So it appended the tail to a
// cancelled vendor order (sim R1: leg 94, 2 blocks, 5 ms after the fleet
// cancelled it). The check is now a compare-and-set on the leg's status at the
// fleet call.

func cancelLeg(t *testing.T, d *Dispatcher, id int64) *orders.Order {
	t.Helper()
	o, err := d.db.GetOrder(id)
	testutil.MustNoErr(t, err, "leg")
	d.lifecycle.CancelOrder(o, "core", "test: the leg's parent was cancelled", CancelCause{})
	ended, err := d.db.GetOrder(id)
	testutil.MustNoErr(t, err, "reload")
	return ended
}

// The valve's path in R1: the funnel is handed a row read before the cancel.
func TestGateAppend_ACancelledLegGetsNoTail(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	laneID, s0, s1 := gateChoreoLane(t, db, "GTERM1", "GTERM1-WAIT")
	line := lineNode(t, db, "GTERM1-LINE")
	blocker := testdb.CreateOrder(t, db, func(o *orders.Order) { o.DeliveryNode = s1.Name; o.Status = "queued" })
	leg := stageGatedStore(t, db, d, line, s0, nil)
	if !IsGateStaged(leg) {
		t.Fatalf("fixture: the leg must stage behind the blocker (wait=%d)", leg.WaitIndex)
	}
	markStaged(t, db, leg.ID)
	stale, err := db.GetOrder(leg.ID)
	testutil.MustNoErr(t, err, "the row as the valve read it")

	cancelLeg(t, d, leg.ID)
	placeDeeperBlocker(t, db, d, blocker.ID, s1.Name)

	_ = d.appendGateTail(stale, "lane gate open")
	d.EvaluateLaneReleases(laneID)

	if n := appendsTo(backend, leg.VendorOrderID); n != 0 {
		t.Fatalf("%d tail append(s) to the cancelled leg's vendor order %s", n, leg.VendorOrderID)
	}
}

// A dead leg skipped at the lane does not swallow the wake: the live one
// waiting behind the same lane is still released, and the dead one gets no
// wait sentence written on it.
func TestGateAppend_ADeadLegDoesNotSwallowTheWake(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	laneID, _, _ := gateChoreoLane(t, db, "GTERM2", "GTERM2-WAIT")
	deepenLane(t, db, laneID, "GTERM2", 3)
	slots := laneSlotsByDepth(t, db, laneID) // S0 shallow … S2 deepest
	line := lineNode(t, db, "GTERM2-LINE")
	blocker := testdb.CreateOrder(t, db, func(o *orders.Order) { o.DeliveryNode = slots[2].Name; o.Status = "queued" })
	dead := stageGatedStore(t, db, d, line, slots[1], nil)
	live := stageGatedStore(t, db, d, line, slots[0], nil)
	if !IsGateStaged(dead) || !IsGateStaged(live) {
		t.Fatalf("fixture: both legs must stage behind the blocker (dead wait=%d, live wait=%d)", dead.WaitIndex, live.WaitIndex)
	}
	markStaged(t, db, dead.ID)
	markStaged(t, db, live.ID)
	ended := cancelLeg(t, d, dead.ID)

	placeDeeperBlocker(t, db, d, blocker.ID, slots[2].Name)
	d.EvaluateLaneReleases(laneID)

	if n := appendsTo(backend, dead.VendorOrderID); n != 0 {
		t.Errorf("%d tail append(s) to the cancelled leg", n)
	}
	if n := appendsTo(backend, live.VendorOrderID); n != 1 {
		t.Fatalf("the live leg behind the dead one got %d append(s), want 1: the dead leg swallowed the lane's wake", n)
	}
	after, err := db.GetOrder(dead.ID)
	testutil.MustNoErr(t, err, "dead leg")
	if after.QueueCause != ended.QueueCause || after.QueueReason != ended.QueueReason {
		t.Errorf("a wait was written on the cancelled leg: %q/%q -> %q/%q",
			ended.QueueCause, ended.QueueReason, after.QueueCause, after.QueueReason)
	}
}
