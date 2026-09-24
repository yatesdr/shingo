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

	o := newPendingOrder("uuid-wait-ends-on-move")
	o.Status = protocol.StatusQueued
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

	// Still waiting: queued→sourcing keeps the wait.
	moved, err := orders.UpdateStatusFrom(db, o.ID, string(protocol.StatusQueued), string(protocol.StatusSourcing), "reserving")
	testutil.MustNoErr(t, err, "queued→sourcing")
	if !moved {
		t.Fatal("queued→sourcing was refused")
	}
	if r, _, _ := wait("sourcing"); r == "" {
		t.Error("queued→sourcing cleared the wait — the order is still waiting")
	}

	// Moving: sourcing→dispatched ends it.
	moved, err = orders.UpdateStatusFrom(db, o.ID, string(protocol.StatusSourcing), string(protocol.StatusDispatched), "dispatched")
	testutil.MustNoErr(t, err, "sourcing→dispatched")
	if !moved {
		t.Fatal("sourcing→dispatched was refused")
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
