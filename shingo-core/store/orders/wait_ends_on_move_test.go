//go:build docker

// Black-box (package orders_test) per the cycle note in orders_test.go.
package orders_test

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestWaitEndsWhenTheOrderMoves: the live queue columns say what an order is
// waiting for NOW, so a transition into a moving status clears them in the same
// write — and a transition between waiting statuses does not.
//
// Springfield 2026-09-24: orders 6908/6913 waited for an empty bin, dispatched,
// and every board that prints queue_reason went on saying "Waiting for an empty
// bin" beside a robot that was driving.
func TestWaitEndsWhenTheOrderMoves(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB

	// (C1: the waiting-to-waiting move is pending → queued, the park; queued →
	// sourcing is no longer an edge.)
	o := newPendingOrder("uuid-wait-ends-on-move")
	testutil.MustNoErr(t, orders.Create(db, o), "create")
	testutil.MustNoErr(t, orders.SetQueueDetail(db, o.ID,
		"Waiting for an empty bin in AMR Supermarket", string(protocol.QueueWaitingForMaterial), "finder-group-empty"),
		"park")

	wait := func(step string) (reason, code, cause string) {
		t.Helper()
		got, err := orders.Get(db, o.ID)
		testutil.MustNoErr(t, err, step+": reload")
		return got.QueueReason, got.QueueCode, got.QueueCause
	}

	// Still waiting: pending→queued keeps the wait.
	moved, err := orders.UpdateStatusFromWithReason(db, o.ID, string(protocol.StatusPending), string(protocol.StatusQueued),
		"Waiting for an empty bin in AMR Supermarket", string(protocol.QueueWaitingForMaterial), "", nil)
	testutil.MustNoErr(t, err, "pending→queued")
	if !moved {
		t.Fatal("pending→queued was refused")
	}
	if r, _, _ := wait("queued"); r == "" {
		t.Error("pending→queued cleared the wait — the order is still waiting")
	}

	// Moving: queued→dispatched ends it.
	moved, err = orders.UpdateStatusFrom(db, o.ID, string(protocol.StatusQueued), string(protocol.StatusDispatched), "dispatched")
	testutil.MustNoErr(t, err, "queued→dispatched")
	if !moved {
		t.Fatal("queued→dispatched was refused")
	}
	if r, c, k := wait("dispatched"); r != "" || c != "" || k != "" {
		t.Errorf("dispatched order still carries its wait: reason=%q code=%q cause=%q", r, c, k)
	}

	// The history keeps the wait it had.
	var sawCode bool
	for _, row := range episodeCodes(t, db, o.ID) {
		if row[1] == string(protocol.QueueWaitingForMaterial) {
			sawCode = true
		}
	}
	if !sawCode {
		t.Errorf("the wait's code left the history: %v", episodeCodes(t, db, o.ID))
	}

	// A station wait on a staged robot, released: staged→in_transit ends it too,
	// through the non-CAS writer. (SPR 6903 read "Waiting for partner robot"
	// mid-unload.)
	testutil.MustNoErr(t, orders.UpdateStatus(db, o.ID, string(protocol.StatusStaged), "at the station"), "→staged")
	testutil.MustNoErr(t, orders.SetQueueDetail(db, o.ID,
		"Waiting for partner robot", string(protocol.QueueWaitingForPartner), "station-wait"), "station wait")
	if r, _, _ := wait("staged"); r == "" {
		t.Fatal("setup: the staged order holds no wait")
	}
	testutil.MustNoErr(t, orders.UpdateStatus(db, o.ID, string(protocol.StatusInTransit), "released"), "staged→in_transit")
	if r, c, k := wait("in_transit"); r != "" || c != "" || k != "" {
		t.Errorf("released order still carries its station wait: reason=%q code=%q cause=%q", r, c, k)
	}
}

// TestF1Pin_AWaitIndexAdvanceEndsTheWaitItLeft — DECISIONS D27 F1
// (TRACE-floor-releases.md (a)). Leaving a wait ends it as much as a status move
// does (waitEndsSQL's rule), and the wait_index write is what leaves it.
// appendSegmentAndAdvance writes wait_index one statement before
// lifecycle.Release clears the cause, so for that one write a lane waiter
// carried the station wait's `station-wait` and a floor pass in the gap
// recorded it under that cause (c2b orders 451 and 514; c-after 220 and 753).
// The advance clears the columns in the same statement, +0.
func TestF1Pin_AWaitIndexAdvanceEndsTheWaitItLeft(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB

	o := newPendingOrder("uuid-f1-wait-index")
	testutil.MustNoErr(t, orders.Create(db, o), "create")
	testutil.MustNoErr(t, orders.UpdateStatus(db, o.ID, string(protocol.StatusStaged), "at the station"), "→staged")
	testutil.MustNoErr(t, orders.SetQueueDetail(db, o.ID,
		"Waiting for partner robot", string(protocol.QueueWaitingForPartner), "station-wait"), "the station wait")

	testutil.MustNoErr(t, orders.UpdateWaitIndex(db, o.ID, 1), "advance to the lane wait")

	got, err := orders.Get(db, o.ID)
	testutil.MustNoErr(t, err, "reload")
	if got.WaitIndex != 1 {
		t.Fatalf("wait_index = %d, want 1", got.WaitIndex)
	}
	if got.QueueReason != "" || got.QueueCode != "" || got.QueueCause != "" {
		t.Errorf("the order left wait 0 still carrying its wait: reason=%q code=%q cause=%q — a lane "+
			"waiter labelled with the station wait it already left", got.QueueReason, got.QueueCode, got.QueueCause)
	}
}
