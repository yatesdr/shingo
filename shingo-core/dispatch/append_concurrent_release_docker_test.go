//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// append_concurrent_release_docker_test.go — the release that lost a CAS race
// to a writer heading for the same status.
//
// Release and MarkInTransit both target in_transit. appendSegmentAndAdvance
// hands the segment to the fleet FIRST, so between the robot starting to move
// and the staged→in_transit write landing, the wiring layer's fleet poll can
// see it running and call MarkInTransit. The CAS then finds in_transit where
// its snapshot said staged and refuses.
//
// That refusal used to be reported as a failed release. It is not one — the
// order is exactly where the release was taking it, and the robot is already
// driving. The observable damage was a split brain: Edge marked the order
// failed while Core had it confirmed. Springfield PLN_002 at 2026-08-26
// 21:14:31, and ten times across the fleet in the ten days before it.
//
// The sibling file append_fails_closed_docker_test.go pins the opposite
// property — that a release which genuinely did NOT land stays an error. These
// two are the same discriminator read in both directions, so they belong
// together: is the order where the transition was going?

// concurrentWriter moves the ROW without touching the caller's struct — which
// is precisely the shape of the race. The in-memory order still says staged;
// the database has moved on.
//
// Terminal statuses take the TerminalizeOrder door: the store refuses a raw
// UpdateOrderStatus into one ("route terminal transitions through
// TerminalizeOrder"), so staging the hostile cases through the front door is
// not optional here — and it is the more faithful simulation anyway, since a
// real concurrent cancel arrives that way too.
func concurrentWriter(t *testing.T, db *store.DB, id int64, to protocol.Status) {
	t.Helper()
	if protocol.IsTerminal(to) {
		if _, err := db.TerminalizeOrder(id, to, "concurrent writer"); err != nil {
			t.Fatalf("stage the concurrent terminal write to %s: %v", to, err)
		}
		return
	}
	if err := db.UpdateOrderStatus(id, string(to), "concurrent writer"); err != nil {
		t.Fatalf("stage the concurrent write to %s: %v", to, err)
	}
}

// The benign race: the fleet poll got there first and wrote the SAME status
// the release wanted. The postcondition holds, so the release completed.
func TestAppendSegment_ConcurrentInTransitIsNotAFailure(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	ord := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = protocol.StatusStaged
		o.VendorOrderID = "sg-appendconc-benign"
	})
	// MarkInTransit lands between the fleet append and our transition.
	concurrentWriter(t, db, ord.ID, protocol.StatusInTransit)

	err := d.appendSegmentAndAdvance(ord, aDropoffSegment(), false, 0, "concurrent probe")
	if err != nil {
		t.Fatalf("a release whose order is already in_transit has met its postcondition and must "+
			"report success; got %v", err)
	}
	if n := len(backend.ReleaseCalls()); n != 1 {
		t.Fatalf("the append must still have gone out exactly once; release calls = %d", n)
	}
	// The caller keeps using this struct — the gate path reads WaitIndex off it,
	// and a stale `staged` here is the next frame's bug.
	if ord.Status != protocol.StatusInTransit {
		t.Fatalf("the struct must adopt the status we lost the race to write; got %s", ord.Status)
	}
	if ord.WaitIndex != 1 {
		t.Fatalf("the durable witness advanced, so the struct must agree; wait_index = %d", ord.WaitIndex)
	}
	testdb.RequireOrderStatus(t, db, ord.EdgeUUID, protocol.StatusInTransit)
}

// The hostile race, and the reason this cannot be "a refused CAS means someone
// else did it for us": the concurrent writer CANCELLED the order. The release
// did not land, the caller must hear about it, and the error must still carry
// that the fleet has the blocks.
func TestAppendSegment_ConcurrentCancelStaysAFailure(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	ord := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = protocol.StatusStaged
		o.VendorOrderID = "sg-appendconc-cancel"
	})
	concurrentWriter(t, db, ord.ID, protocol.StatusCancelled)

	err := d.appendSegmentAndAdvance(ord, aDropoffSegment(), false, 0, "concurrent probe")
	if err == nil {
		t.Fatal("a release onto a cancelled order did not complete and must not report success")
	}
	if !IsAppendLanded(err) {
		t.Fatalf("the error must carry that the fleet already has the blocks, or a rollback arm "+
			"frees a corridor the robot is driving into; got %v", err)
	}
	if n := len(backend.ReleaseCalls()); n != 1 {
		t.Fatalf("the append itself must still have gone out; release calls = %d", n)
	}
}

// A concurrent write to any other non-target status is the same failure. Pinned
// separately from the cancel case because `delivered` is the one an over-eager
// "it's fine, someone moved it" reading would most plausibly wave through — it
// looks like progress, and it is not the status this release was writing.
func TestAppendSegment_ConcurrentDeliveredStaysAFailure(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	ord := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = protocol.StatusStaged
		o.VendorOrderID = "sg-appendconc-delivered"
	})
	concurrentWriter(t, db, ord.ID, protocol.StatusDelivered)

	err := d.appendSegmentAndAdvance(ord, aDropoffSegment(), false, 0, "concurrent probe")
	if err == nil {
		t.Fatal("delivered is not the status this release was writing; it must not report success")
	}
	if !IsAppendLanded(err) {
		t.Fatalf("the error must carry that the fleet already has the blocks; got %v", err)
	}
}
