//go:build docker

package store_test

import (
	"database/sql"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// claim_sweep_pin_docker_test.go — the narrow sweep's findings (RULINGS-D2-
// lane0-dig-race §2.6; census/claim-rw-sweep.md; LOCKS.md §3): a transaction
// that reads a bin's holds and writes on them must lock what it read or check
// what it wrote. Each finding here never leaves a pointer standing on a bin its
// order holds no row on.

func pointerAndRow(t *testing.T, db *store.DB, orderID, binID int64) (pointer sql.NullInt64, row bool) {
	t.Helper()
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT bin_id, EXISTS (SELECT 1 FROM reservations
		WHERE order_id = $1 AND bin_id = $2 AND resource_kind = 'bin') FROM orders WHERE id = $1`,
		orderID, binID).Scan(&pointer, &row), "the order's pointer and row")
	return pointer, row
}

// F1 · A dig leg's handover writes the bin's one row as the leg's. A stranger
// that soft-held the bin after the dig was born (its hold landed in the dig
// birth's window) loses its row there, and its pointer must go with it. RED at
// adb03118: the handover deleted the stranger's row and left it pointing.
func TestClaimSweepPin_ALegHandoverUnpointsTheStrangerWhoseRowItTakes(t *testing.T) {
	t.Parallel()
	r := newDigRace(t, "CSP-F1", false)
	testutil.MustNoErr(t, r.db.StealSoftHoldsForTest(r.binID, r.child.ID, r.parent.ID), "the dig is born, taking the first holder's row")

	late := testdb.CreateOrder(t, r.db, func(o *orders.Order) { o.EdgeUUID = "CSP-F1-late" })
	testutil.MustNoErr(t, reservations.Acquire(r.db.DB, late.ID, late.ID, r.binID, "test"), "a stranger's hold lands after the birth")
	testutil.MustNoErr(t, r.db.UpdateOrderBinID(late.ID, r.binID), "and points at the bin")

	testutil.MustNoErr(t, inTx(r.db, func(tx *sql.Tx) error {
		return store.ClaimCompoundLegBinTx(tx, r.binID, r.child.ID, r.parent.ID)
	}), "the leg's handover")
	if pointer, row := pointerAndRow(t, r.db, late.ID, r.binID); pointer.Valid && !row {
		t.Fatalf("the stranger %d still points at bin %d with no row on it after the leg's handover", late.ID, r.binID)
	}
}

// F2 · A person's move takes the soft holds on its bin. A holder whose hand
// confirmed while the take waited must not be un-pointed or told its bin was
// taken: its row was not taken. RED at adb03118: the take un-pointed the holder
// and wrote "bin-taken" whatever its delete changed.
func TestClaimSweepPin_ATakeLeavesAHolderWhoseHandConfirmedFirst(t *testing.T) {
	t.Parallel()
	r := newDigRace(t, "CSP-F2", true)
	htx, err := r.hand(t)
	testutil.MustNoErr(t, err, "the holder's hand claims and confirms")
	taker := testdb.CreateOrder(t, r.db, func(o *orders.Order) { o.EdgeUUID = "CSP-F2-taker" })
	done := make(chan error, 1)
	go func() {
		done <- inTx(r.db, func(tx *sql.Tx) error {
			_, err := store.TakeSoftHolds(tx, r.binID, taker.ID, takeNote)
			return err
		})
	}()
	waitsOn(t, r.db, pid(t, htx), done)
	testutil.MustNoErr(t, htx.Commit(), "the hand commits")
	<-done
	pointer, row := pointerAndRow(t, r.db, r.holder.ID, r.binID)
	if !row || !pointer.Valid || pointer.Int64 != r.binID {
		t.Fatalf("after a take that came second the holder's pointer is %v and its row exists=%v; want both kept", pointer, row)
	}
	var told int
	testutil.MustNoErr(t, r.db.DB.QueryRow(`SELECT COUNT(*) FROM order_history WHERE order_id = $1 AND code = 'bin-taken'`, r.holder.ID).Scan(&told), "history")
	if told != 0 {
		t.Fatalf("the holder was told its bin was taken %d time(s); nothing of its was taken", told)
	}
}

// F3 · The scanner soft-reserves a bin and points the order at it. A steal
// between the two left a pointer with no row. The reserve and the pointer are
// one statement now (ReserveAndPoint): there is no between. Driven here as the
// sequence the scanner used to run, which must leave no pointer without a row.
func TestClaimSweepPin_AReserveAndItsPointerHaveNothingBetweenThem(t *testing.T) {
	t.Parallel()
	r := newDigRace(t, "CSP-F3", false)
	_, err := r.db.Exec(`DELETE FROM reservations WHERE order_id = $1`, r.holder.ID)
	testutil.MustNoErr(t, err, "start with no hold")
	testutil.MustNoErr(t, scannerReserve(r.db, r.holder.ID, r.binID, func() {
		testutil.MustNoErr(t, r.db.StealSoftHoldsForTest(r.binID, r.child.ID, r.parent.ID), "a dig steals between the reserve and the pointer")
	}), "the scanner's reserve")
	if pointer, row := pointerAndRow(t, r.db, r.holder.ID, r.binID); pointer.Valid && !row {
		t.Fatalf("order %d points at bin %d with no row on it", r.holder.ID, r.binID)
	}
}
