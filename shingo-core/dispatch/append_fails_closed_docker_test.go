//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// append_fails_closed_docker_test.go — §R.98 stage A3.
//
// appendSegmentAndAdvance is the one fleet-append door, and the append is the
// one irreversible step behind it. Everything after it — the durable wait_index
// witness, the staged→in_transit transition — used to be logged on failure and
// then reported as a successful release. That is how a mission the fleet had
// forgotten got recorded as a good final append: the last observable signal was
// an illegal in_transit→in_transit transition, and it went to a log line.
//
// Two properties are pinned here. The failure is an ERROR (a release that could
// not move the order out of staging did not complete), and it is an error that
// says THE FLEET ALREADY HAS THE BLOCKS — because the rollback arms above it
// drop occupancy rows on the strength of "the robot never got the tail", and
// that sentence is false past the append.

func aDropoffSegment() []resolvedStep {
	return []resolvedStep{{Action: protocol.ActionDropoff, Node: "APPCLOSED-DEST"}}
}

// An order that moved somewhere the release cannot reach from — delivered, here —
// is un-releasable, and the append still went out. That is not a completed
// release and must not be reported as one.
func TestAppendSegment_UnreleasableOrderIsNotSuccess(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	ord := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = protocol.StatusDelivered
		o.VendorOrderID = "sg-appendclosed-illegal"
	})

	err := d.appendSegmentAndAdvance(ord, aDropoffSegment(), false, 0, "fails-closed probe")
	if err == nil {
		t.Fatal("a release that could not move the order out of staging must not report success")
	}
	if !IsAppendLanded(err) {
		t.Fatalf("the error must carry that the fleet already has the blocks, or the rollback arms "+
			"above will free a corridor the robot is driving into; got %v", err)
	}
	if n := len(backend.ReleaseCalls()); n != 1 {
		t.Fatalf("the append itself must still have gone out; release calls = %d", n)
	}

	// The witness advanced before the transition was attempted, so it stays
	// advanced — the fleet genuinely has the tail and a second append would be a
	// duplicate. The in-memory struct has to agree with the row.
	fresh, gerr := db.GetOrder(ord.ID)
	if gerr != nil {
		t.Fatalf("reload order: %v", gerr)
	}
	if fresh.WaitIndex != 1 {
		t.Fatalf("wait_index in the row = %d, want 1", fresh.WaitIndex)
	}
	if ord.WaitIndex != fresh.WaitIndex {
		t.Fatalf("the caller's struct says wait_index=%d while the row says %d", ord.WaitIndex, fresh.WaitIndex)
	}
}

// And the case that keeps the tolerance honest: a SECOND append on one order —
// a gated entry followed by the dwell's own release — finds the order already in
// transit, because the entry put it there. The state machine has no self-edge,
// so a healthy idempotent release surfaces as `in_transit → in_transit`. It is
// success, and it is the reason the discriminator is "already where the
// transition was going" rather than the error class.
func TestAppendSegment_AlreadyInTransitIsIdempotentSuccess(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	ord := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = protocol.StatusInTransit
		o.VendorOrderID = "sg-appendclosed-second"
	})

	if err := d.appendSegmentAndAdvance(ord, aDropoffSegment(), false, 0, "fails-closed probe"); err != nil {
		t.Fatalf("a second append on an already-in-transit order is idempotent, not refused: %v", err)
	}
	if ord.WaitIndex != 1 {
		t.Fatalf("the witness must advance on a completed release; wait_index = %d", ord.WaitIndex)
	}
}

// The witness arm. wait_index is what every release path re-reads to decide
// whether a tail is still owed, so an append that landed while the witness write
// failed leaves the row saying "still owed" about work the robot already has.
// That was logged and reported as success; a second pass would then append the
// same segment twice with nothing anywhere saying why.
func TestAppendSegment_UnwrittenWitnessIsNotSuccess(t *testing.T) {
	t.Parallel()

	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	ord := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = protocol.StatusStaged
		o.VendorOrderID = "sg-appendclosed-witness"
	})

	// Take the database away between the append and the write after it. The
	// backend is not on the database, so the segment still goes out.
	if err := db.DB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	err := d.appendSegmentAndAdvance(ord, aDropoffSegment(), false, 0, "fails-closed probe")
	if err == nil {
		t.Fatal("an append whose witness never persisted must not report success")
	}
	if !IsAppendLanded(err) {
		t.Fatalf("the fleet has the blocks; the error must say so; got %v", err)
	}
	if n := len(backend.ReleaseCalls()); n != 1 {
		t.Fatalf("the append itself must still have gone out; release calls = %d", n)
	}
	if ord.WaitIndex != 0 {
		t.Fatalf("the caller's struct must not claim a witness that was never written; wait_index = %d", ord.WaitIndex)
	}
}

// The other side of the same door: an append the fleet REFUSED is not an
// append-landed error, because nothing landed. The rollback arms must still fire
// for it, and the witness must not move.
func TestAppendSegment_RefusedAppendIsNotAppendLanded(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewFailingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	ord := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = protocol.StatusStaged
		o.VendorOrderID = "sg-appendclosed-refused"
	})

	err := d.appendSegmentAndAdvance(ord, aDropoffSegment(), false, 0, "fails-closed probe")
	if err == nil {
		t.Fatal("a refused append must be an error")
	}
	if IsAppendLanded(err) {
		t.Fatal("a refused append must NOT claim the fleet has the blocks — the rollback has to run")
	}

	fresh, gerr := db.GetOrder(ord.ID)
	if gerr != nil {
		t.Fatalf("reload order: %v", gerr)
	}
	if fresh.WaitIndex != 0 {
		t.Fatalf("wait_index advanced on an unproven append: %d", fresh.WaitIndex)
	}
	if ord.WaitIndex != 0 {
		t.Fatalf("the caller's struct advanced on an unproven append: %d", ord.WaitIndex)
	}
}
