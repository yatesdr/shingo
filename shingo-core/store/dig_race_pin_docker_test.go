//go:build docker

package store_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// dig_race_pin_docker_test.go — a dig's steal and a hand's claim of one bin
// (RULINGS-D2-lane0-dig-race; TRACE-231-236-243). Both write the holder's
// reservation row; whichever is first, the other sees it:
//   - hand first: the dig is refused with the holder named, and waits;
//   - dig first: the hand's claim is refused, the holder un-pointed;
//   - never: claimed_by set with the reservation gone, a confirmed reservation
//     deleted, or a holder's pointer cleared when its row was not taken.
// Each order is driven with the first transaction held open at its critical
// statement while the second starts and waits, and once in plain sequence.

type digRace struct {
	db     *store.DB
	binID  int64
	holder *orders.Order
	parent *orders.Order
	child  *orders.Order
}

func newDigRace(t *testing.T, prefix string, holderPoints bool) *digRace {
	t.Helper()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, prefix+"-BIN")
	holder := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = prefix + "-holder" })
	testutil.MustNoErr(t, reservations.Acquire(db.DB, holder.ID, holder.ID, bin.ID, "test"), "the holder's soft hold")
	if holderPoints {
		testutil.MustNoErr(t, db.UpdateOrderBinID(holder.ID, bin.ID), "the holder points at the bin")
	}
	parent := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = prefix + "-dig" })
	_, err := db.Exec(`UPDATE orders SET priority = 100 WHERE id = $1`, parent.ID)
	testutil.MustNoErr(t, err, "the dig outranks the holder")
	child := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = prefix + "-leg"
		o.ParentOrderID = &parent.ID
	})
	return &digRace{db: db, binID: bin.ID, holder: holder, parent: parent, child: child}
}

// hand is the hand's claim, in its order: the holder's order row (the status
// CAS, takeHandTx's first row lock), the bin (claimBin), the reservation
// (Confirm). Returns the open transaction, or the error that refused it.
func (r *digRace) hand(t *testing.T) (*sql.Tx, error) {
	t.Helper()
	tx, err := r.db.Begin()
	testutil.MustNoErr(t, err, "begin the hand")
	if _, err := tx.Exec(`UPDATE orders SET status = 'dispatched', updated_at = NOW() WHERE id = $1`, r.holder.ID); err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := bins.ClaimTx(tx, r.binID, r.holder.ID); err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := reservations.Confirm(tx, r.holder.ID, r.binID); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func pid(t *testing.T, tx *sql.Tx) int {
	t.Helper()
	var p int
	testutil.MustNoErr(t, tx.QueryRow(`SELECT pg_backend_pid()`).Scan(&p), "backend")
	return p
}

// waitsOn returns once some backend waits on pid, or once done has a result
// (the second transaction did not wait at all), or after 5 s.
func waitsOn(t *testing.T, db *store.DB, p int, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		testutil.MustNoErr(t, db.DB.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, p).Scan(&n), "waiters")
		if n > 0 || len(done) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// invariants checks the three nevers after both transactions ended.
func (r *digRace) invariants(t *testing.T, digTook bool) {
	t.Helper()
	var claimedBy sql.NullInt64
	var resState sql.NullString
	var pointer sql.NullInt64
	testutil.MustNoErr(t, r.db.DB.QueryRow(`SELECT b.claimed_by,
		(SELECT state FROM reservations WHERE bin_id = b.id AND order_id = $2 AND resource_kind = 'bin'),
		(SELECT bin_id FROM orders WHERE id = $2)
		FROM bins b WHERE b.id = $1`, r.binID, r.holder.ID).Scan(&claimedBy, &resState, &pointer), "the book")
	if claimedBy.Valid && claimedBy.Int64 == r.holder.ID && !resState.Valid {
		t.Errorf("claimed_by = holder %d with its reservation gone", r.holder.ID)
	}
	if digTook {
		if claimedBy.Valid {
			t.Errorf("the dig took the bin and claimed_by is %d", claimedBy.Int64)
		}
		if resState.Valid {
			t.Errorf("the dig took the bin and the holder's reservation is still %s", resState.String)
		}
	} else {
		if !claimedBy.Valid || claimedBy.Int64 != r.holder.ID || resState.String != string(reservations.StateConfirmed) {
			t.Errorf("the hand won: claimed_by %v, reservation %q; want the holder's claim and a confirmed row", claimedBy, resState.String)
		}
		if r.holder.BinID != nil && (!pointer.Valid || pointer.Int64 != r.binID) {
			t.Errorf("the holder's pointer was cleared (%v) when its row was not taken", pointer)
		}
	}
}

func refusedNamingHolder(t *testing.T, err error, holder int64) {
	t.Helper()
	var blocked *store.BlockerClaimedError
	if !errors.As(err, &blocked) || blocked.HolderID != holder {
		t.Fatalf("the dig after the hand: %v; want refused with holder %d named", err, holder)
	}
}

// The hand claims and confirms, held open; the dig starts and must wait, then
// be refused naming the holder. RED at 545b8b57: the dig's unconditioned DELETE
// removes the confirmed row and clears the pointer once the hand commits.
func TestDigRacePin_HandFirstWhileTheDigWaits(t *testing.T) {
	t.Parallel()
	for _, points := range []bool{true, false} {
		r := newDigRace(t, map[bool]string{true: "DRP-HW-P", false: "DRP-HW-N"}[points], points)
		if points {
			r.holder.BinID = &r.binID
		}
		htx, err := r.hand(t)
		testutil.MustNoErr(t, err, "the hand")
		done := make(chan error, 1)
		go func() { done <- r.db.StealSoftHoldsForTest(r.binID, r.child.ID, r.parent.ID) }()
		waitsOn(t, r.db, pid(t, htx), done)
		testutil.MustNoErr(t, htx.Commit(), "the hand commits")
		refusedNamingHolder(t, <-done, r.holder.ID)
		r.invariants(t, false)
	}
}

// The dig takes the holder's row, held open; the hand starts and must wait, then
// be refused. RED at 545b8b57 for a holder not pointing at the bin: nothing the
// dig wrote locked a row the hand needs, the hand claims on a reservation the
// dig deleted, and its confirm changes nothing and is not checked.
func TestDigRacePin_DigFirstWhileTheHandWaits(t *testing.T) {
	t.Parallel()
	for _, points := range []bool{true, false} {
		r := newDigRace(t, map[bool]string{true: "DRP-DW-P", false: "DRP-DW-N"}[points], points)
		stx, err := r.db.Begin()
		testutil.MustNoErr(t, err, "begin the dig")
		testutil.MustNoErr(t, store.StealSoftHoldsInTx(stx, r.binID, r.child.ID, r.parent.ID), "the dig takes the holder's row")
		done := make(chan error, 1)
		var htx *sql.Tx
		go func() {
			tx, err := r.hand(t)
			htx = tx
			done <- err
		}()
		waitsOn(t, r.db, pid(t, stx), done)
		testutil.MustNoErr(t, stx.Commit(), "the dig commits")
		if err := <-done; err == nil {
			htx.Commit()
			t.Errorf("holder points=%v: the hand claimed after the dig took its row", points)
		}
		r.invariants(t, true)
	}
}

// Plain sequence, both orders.
func TestDigRacePin_InSequence(t *testing.T) {
	t.Parallel()
	r := newDigRace(t, "DRP-SEQ-H", true)
	r.holder.BinID = &r.binID
	htx, err := r.hand(t)
	testutil.MustNoErr(t, err, "the hand")
	testutil.MustNoErr(t, htx.Commit(), "commit")
	refusedNamingHolder(t, r.db.StealSoftHoldsForTest(r.binID, r.child.ID, r.parent.ID), r.holder.ID)
	r.invariants(t, false)

	d := newDigRace(t, "DRP-SEQ-D", true)
	testutil.MustNoErr(t, d.db.StealSoftHoldsForTest(d.binID, d.child.ID, d.parent.ID), "the dig")
	if tx, err := d.hand(t); err == nil {
		tx.Commit()
		t.Error("the hand claimed after the dig took its row")
	}
	d.invariants(t, true)
}
