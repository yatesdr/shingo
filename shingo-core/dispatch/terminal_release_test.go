//go:build docker

// terminal_release_test.go — the terminal-chokepoint invariant (commit 1).
//
// Pins: reaching ANY terminal status releases the order's reservations. The
// success terminal 'confirmed' historically routed through UpdateOrderStatus
// (status + history only, no release), leaking a 'confirmed' reservation row
// that permanently bricked the bin via the uq_reservations_bin_active partial
// unique index (WHERE state IN ('pending','confirmed')). This test is RED on
// the 'confirmed' transitions before the fix and green after.

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
