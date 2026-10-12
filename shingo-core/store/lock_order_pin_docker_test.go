//go:build docker

package store_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// lock_order_pin_docker_test.go — a take and a terminalize lock one holder's two
// rows in one order: the order row, then its reservation (LOCKS.md §3, D1).
//
// TerminalizeOrderWithReason updates the holder's order row (store/orders.go,
// "UPDATE orders SET status") and then releases its reservations
// (ReleaseByOrder). A take locked the holder's reservation first and wrote its
// order row after. Caught between the terminalize's two statements, each waited
// for the other; Postgres broke the cycle after deadlock_timeout by aborting
// one, while the take's caller held the scan lock or a lane gate.
//
// The terminalizing side is a transaction that takes the same two locks, in the
// same order, with a take started between them.

// terminalizeAround runs a terminalize's two locks on holder in a transaction,
// with take running between them, and returns take's error and the
// terminalize's.
func terminalizeAround(t *testing.T, db *store.DB, holder int64, take func() error) (takeErr, termErr error) {
	t.Helper()
	tx, err := db.Begin()
	testutil.MustNoErr(t, err, "begin the terminalize")
	defer tx.Rollback()
	var pid int
	testutil.MustNoErr(t, tx.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid), "its backend")
	_, err = tx.Exec(`UPDATE orders SET status = 'cancelled', updated_at = NOW() WHERE id = $1`, holder)
	testutil.MustNoErr(t, err, "the terminalize's order row")

	done := make(chan error, 1)
	go func() { done <- take() }()
	// The take is under way when it waits on the terminalize's lock.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		testutil.MustNoErr(t, db.DB.QueryRow(`SELECT count(*) FROM pg_stat_activity
			WHERE $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&waiting), "who waits on the terminalize")
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the take never waited on the terminalize's order row")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, termErr = tx.Exec(`DELETE FROM reservations WHERE order_id = $1`, holder); termErr == nil {
		termErr = tx.Commit()
	}
	select {
	case takeErr = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the take did not return after the terminalize ended")
	}
	return takeErr, termErr
}

func deadlocked(err error) bool {
	return err != nil && strings.Contains(err.Error(), "deadlock detected")
}

// TestLockOrderPin_ASlotTakeAndATerminalizeDoNotDeadlock: takeSlotTx locked the
// holder's slot row, then waited on its order row. RED at 545b8b57: one side is
// aborted with "deadlock detected".
func TestLockOrderPin_ASlotTakeAndATerminalizeDoNotDeadlock(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	slot := sd.StorageNode
	holder := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "lock-order-slot-holder" })
	testutil.MustNoErr(t, db.ReserveSlot(slot.ID, holder.ID), "the holder's promise")
	taker := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "lock-order-slot-taker" })
	_, err := db.Exec(`UPDATE orders SET priority = 100 WHERE id = $1`, taker.ID)
	testutil.MustNoErr(t, err, "rank the taker")

	takeErr, termErr := terminalizeAround(t, db, holder.ID, func() error {
		_, err := db.TakeSlotByRank(slot.ID, taker.ID, taker.ID, slotTakeNote)
		return err
	})
	if deadlocked(takeErr) || deadlocked(termErr) {
		t.Fatalf("a slot take and a terminalize deadlocked: take %v, terminalize %v", takeErr, termErr)
	}
	testutil.MustNoErr(t, termErr, "the terminalize")
	// The terminalize released the promise first, so there was nothing to take.
	if !errors.Is(takeErr, store.ErrNoSlotTake) {
		t.Fatalf("the take after the holder let go: %v, want nothing to take", takeErr)
	}
}

// TestLockOrderPin_ASoftHoldTakeAndATerminalizeDoNotDeadlock: TakeSoftHolds
// deleted the holder's bin promise, then waited on its order row. RED at
// 545b8b57.
func TestLockOrderPin_ASoftHoldTakeAndATerminalizeDoNotDeadlock(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "LOCK-ORDER-BIN")
	holder := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "lock-order-bin-holder" })
	testutil.MustNoErr(t, reservations.Acquire(db.DB, holder.ID, holder.ID, bin.ID, "test"), "holder's promise")
	testutil.MustNoErr(t, db.UpdateOrderBinID(holder.ID, bin.ID), "holder points at the bin")
	taker := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "lock-order-bin-taker" })

	takeErr, termErr := terminalizeAround(t, db, holder.ID, func() error {
		return inTx(db, func(tx *sql.Tx) error {
			_, err := store.TakeSoftHolds(tx, bin.ID, taker.ID, takeNote)
			return err
		})
	})
	if deadlocked(takeErr) || deadlocked(termErr) {
		t.Fatalf("a soft-hold take and a terminalize deadlocked: take %v, terminalize %v", takeErr, termErr)
	}
	testutil.MustNoErr(t, termErr, "the terminalize")
	testutil.MustNoErr(t, takeErr, "the take")
}

func inTx(db *store.DB, fn func(*sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// TestLockOrderPin_ADigStealAndATerminalizeDoNotDeadlock: stealSoftHolds clears
// a victim's bin pointer and deletes its promise in one statement, the delete in
// a WITH the UPDATE does not read. Postgres runs such a WITH after the main
// statement, so the UPDATE waits on the terminalize's order row before the
// promise is touched: the same order as the terminalize. Pinned so a rewrite of
// that statement keeps it.
func TestLockOrderPin_ADigStealAndATerminalizeDoNotDeadlock(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "LOCK-ORDER-STEAL")
	holder := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "lock-order-steal-holder" })
	testutil.MustNoErr(t, reservations.Acquire(db.DB, holder.ID, holder.ID, bin.ID, "test"), "holder's promise")
	testutil.MustNoErr(t, db.UpdateOrderBinID(holder.ID, bin.ID), "holder points at the bin")
	parent := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "lock-order-steal-dig" })
	_, err := db.Exec(`UPDATE orders SET priority = 100 WHERE id = $1`, parent.ID)
	testutil.MustNoErr(t, err, "rank the dig")
	child := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "lock-order-steal-leg"
		o.ParentOrderID = &parent.ID
	})

	takeErr, termErr := terminalizeAround(t, db, holder.ID, func() error {
		return db.StealSoftHoldsForTest(bin.ID, child.ID, parent.ID)
	})
	if deadlocked(takeErr) || deadlocked(termErr) {
		t.Fatalf("a dig's steal and a terminalize deadlocked: steal %v, terminalize %v", takeErr, termErr)
	}
	testutil.MustNoErr(t, termErr, "the terminalize")
	testutil.MustNoErr(t, takeErr, "the steal")
}
