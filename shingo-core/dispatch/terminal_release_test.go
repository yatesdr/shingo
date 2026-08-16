//go:build docker

// terminal_release_test.go — the terminal-chokepoint invariant (commit 1).
//
// Pins: reaching ANY terminal status releases the order's reservations. The
// success terminal 'confirmed' historically routed through UpdateOrderStatus
// (status + history only, no release), leaking a 'confirmed' reservation row
// that permanently bricked the bin via the uq_reservations_bin_active partial
// unique index (WHERE state IN ('pending','confirmed')). This test is RED on
// the 'confirmed' transitions before the fix and green after.
//
// It counts rows for ONE BIN, which is right for that regression and blind to
// every other hold an order can carry. TestEveryTerminalTransitionReleasesEveryHoldKind
// below walks the same matrix over the whole ledger — bin, slot, mouth,
// occupancy, the dig lock, and both claimed_by columns. The narrow one is kept
// as it is rather than folded in: it is the pin for a specific bricking, its
// re-acquirability probe is about that index in particular, and a matrix test
// that fails for five reasons at once tells the next reader less than two that
// fail for one each.

package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store/reservations"
)

func TestEveryTerminalTransitionReleasesReservations(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	lc, _ := newLifecycleForTest(t, db)

	seen := 0
	// Cover EVERY (from → terminal) transition in the canonical matrix so a
	// future terminal status can't skip release.
	for from, allowed := range protocol.AllValidTransitions() {
		for _, to := range allowed {
			if !protocol.IsTerminal(to) {
				continue
			}
			seen++
			label := fmt.Sprintf("%s-to-%s", from, to)

			// An order at `from`, holding a reservation on its own bin.
			ord := makeOrderAt(t, db, "term-"+label, from)
			bin := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-"+label)
			if err := reservations.Acquire(db, ord.ID, bin.ID, "test"); err != nil {
				t.Fatalf("%s: Acquire: %v", label, err)
			}

			if err := lc.transition(ord, to, Event{Actor: "test", Reason: "terminal-release"}); err != nil {
				t.Fatalf("%s: transition: %v", label, err)
			}

			// (a) zero reservation rows survive.
			var n int
			if err := db.DB.QueryRow(`SELECT COUNT(*) FROM reservations WHERE bin_id=$1`, bin.ID).Scan(&n); err != nil {
				t.Fatalf("%s: count reservations: %v", label, err)
			}
			if n != 0 {
				t.Errorf("%s: %d reservation row(s) survived a terminal transition, want 0 (a leaked row bricks the bin)", label, n)
			}

			// (b) the freed bin is re-acquirable by a fresh order.
			probe := makeOrderAt(t, db, "probe-"+label, protocol.StatusQueued)
			if err := reservations.Acquire(db, probe.ID, bin.ID, "test"); err != nil {
				t.Errorf("%s: bin not re-acquirable after terminal transition: %v", label, err)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no terminal transitions found in the matrix — test is vacuous")
	}
}

// TestEveryTerminalTransitionReleasesEveryHoldKind is the test above, widened
// from one bin to the whole ledger.
//
// ── WHAT THE ONE-BIN COUNT COULD NOT SEE ──────────────────────────────────
//
// The test above walks the same matrix and then counts `WHERE bin_id=$1`. That
// is the right shape for the regression it was written for — a leaked
// `confirmed` bin row bricking a bin through uq_reservations_bin_active — and it
// is blind to every hold the lane work added since. An order on a lane path
// holds, at once:
//
//	bin        reservation + bins.claimed_by   the bin it is carrying
//	slot       reservation + nodes.claimed_by  the slot it is dropping into
//	mouth      reservation (mode inbound)      the lane's mouth, its direction
//	occupancy  reservation                     it is INSIDE the lane
//	mouth      reservation (mode dig)          the lane lock, when it digs
//
// Four of those five leak silently past a bin-scoped count, and each leaks into
// a different failure: a stranded mouth row makes a lane refuse the opposite
// direction forever, a stranded occupancy row makes a lane read permanently
// busy, a stranded dig lock excludes EVERYTHING from a lane for good, and a
// stranded nodes.claimed_by makes a slot look taken to every placer. None of
// them is visible on the bin.
//
// So this holds one of every kind, takes the transition, and asserts the order
// is left holding nothing at all — plus the ledger-wide sweep, which is what
// catches a release that fired against the wrong owner rather than not firing.
//
// The dig lock sits on a SECOND lane, because a dig admits no other holder on
// its own lane (reservations/mouth.go admitMouth): one order cannot hold both a
// mouth row and a dig on one lane, and pretending otherwise would build a
// fixture the system cannot produce.
//
// MUTATION (verified): delete the `UPDATE nodes SET claimed_by=NULL` statement
// from TerminalizeOrder (store/orders.go). Every transition fires the slot-claim
// assertion. The test above stays GREEN through it — nodes.claimed_by is not
// bins.claimed_by and not a reservation row, so nothing it looks at moves.
func TestEveryTerminalTransitionReleasesEveryHoldKind(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	lc, _ := newLifecycleForTest(t, db)

	seen := 0
	for from, allowed := range protocol.AllValidTransitions() {
		for _, to := range allowed {
			if !protocol.IsTerminal(to) {
				continue
			}
			seen++
			label := fmt.Sprintf("%s-to-%s", from, to)

			ord := makeOrderAt(t, db, "allkinds-"+label, from)

			// bin: reservation (confirmed) + bins.claimed_by, through the
			// production reserve → claim → confirm sequence.
			bin := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-AK-"+label)
			testdb.ClaimBinForTest(t, db, bin.ID, ord.ID)

			// slot: reservation + nodes.claimed_by, on a slot in the lane it is
			// dropping into.
			_, laneID, slot := gatedLane(t, db, "AK-"+label, "")
			if err := db.ReserveSlot(slot.ID, ord.ID); err != nil {
				t.Fatalf("%s: reserve slot: %v", label, err)
			}
			testdb.ClaimSlotForTest(t, db, slot.ID, ord.ID)

			// mouth + occupancy on that lane: it owns the mouth inbound and it is
			// inside.
			if err := reservations.AcquireLanes(db.DB, ord.ID, reservations.ModeInbound, "test", laneID); err != nil {
				t.Fatalf("%s: acquire mouth: %v", label, err)
			}
			if err := reservations.AcquireOccupancy(db.DB, ord.ID, laneID); err != nil {
				t.Fatalf("%s: acquire occupancy: %v", label, err)
			}

			// the lane LOCK, on a second lane — a dig excludes every other holder
			// on its own lane, so it cannot share one with the mouth row above.
			_, digLaneID, _ := gatedLane(t, db, "AKDIG-"+label, "")
			if err := reservations.AcquireLanes(db.DB, ord.ID, reservations.ModeDig, "test", digLaneID); err != nil {
				t.Fatalf("%s: acquire dig lock: %v", label, err)
			}

			if err := lc.transition(ord, to, Event{Actor: "test", Reason: "all-kinds-release"}); err != nil {
				t.Fatalf("%s: transition: %v", label, err)
			}

			// (a) NO reservation of ANY kind survives, named by kind so a failure
			// says which hold leaked rather than only how many did.
			held, err := reservations.ListByOrder(db.DB, ord.ID)
			if err != nil {
				t.Fatalf("%s: list reservations: %v", label, err)
			}
			for _, h := range held {
				t.Errorf("%s: %s reservation (state %s, bin %d, node %d) survived a terminal "+
					"transition", label, h.Kind, h.State, h.BinID, h.NodeID)
			}

			// (b) both hard-claim columns are clear.
			if b := testdb.RequireBin(t, db, bin.ID); b.ClaimedBy != nil {
				t.Errorf("%s: bins.claimed_by = %d after a terminal transition", label, *b.ClaimedBy)
			}
			slotNow, err := db.GetNode(slot.ID)
			if err != nil {
				t.Fatalf("%s: reload slot: %v", label, err)
			}
			if slotNow.ClaimedBy != nil {
				t.Errorf("%s: nodes.claimed_by = %d on slot %s after a terminal transition — a slot "+
					"held by a dead order reads as taken to every placer", label, *slotNow.ClaimedBy, slot.Name)
			}

			// (c) the lane lock is gone, asked through the same reader admission
			// uses, so the assertion is about what the next dig will actually see.
			owner, err := reservations.DigHoldOwner(db.DB, digLaneID)
			if err != nil {
				t.Fatalf("%s: dig hold owner: %v", label, err)
			}
			if owner != 0 {
				t.Errorf("%s: lane %d is still dig-locked by order %d — a dig lock excludes "+
					"everything, so one left behind by a dead order closes that lane permanently",
					label, digLaneID, owner)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no terminal transitions found in the matrix — test is vacuous")
	}

	// And the ledger as a whole, which is the assertion the per-resource checks
	// above cannot make: a release that fired against the WRONG owner clears the
	// row this order was checked on and leaves somebody else's behind.
	testdb.AssertNoOrphanedHolds(t, db)
}
