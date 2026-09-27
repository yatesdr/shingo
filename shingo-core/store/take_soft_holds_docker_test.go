//go:build docker

package store_test

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// take_soft_holds_docker_test.go — a person's move takes a promised bin (Q1).
//
// The person is authoritative over soft holds and never over a robot. The take
// runs in the transaction that births the person's move, deletes the holder's
// promise, and leaves the holder un-pointed with the reason on its row and in
// its history; a holder that is itself a person's move keeps its pointer and is
// handed back to be ended by name.

var takeNote = store.HolderNote{
	Reason: "Its bin was taken by order #0 (a person's move) — finding another",
	Code:   protocol.QueueWaitingForMaterial,
	Cause:  "bin-taken",
	Actor:  "operator",
}

// take runs TakeSoftHolds and the taker's own reservation in one transaction,
// as the bin-move door does.
func take(t *testing.T, db *store.DB, binID, takerID int64) []int64 {
	t.Helper()
	tx, err := db.Begin()
	testutil.MustNoErr(t, err, "begin")
	defer tx.Rollback()
	handPlaced, err := store.TakeSoftHolds(tx, binID, takerID, takeNote)
	testutil.MustNoErr(t, err, "take")
	testutil.MustNoErr(t, reservations.Acquire(tx, takerID, takerID, binID, "test-take"),
		"the taker's own reservation must fit once the promise is taken")
	testutil.MustNoErr(t, tx.Commit(), "commit")
	return handPlaced
}

func TestTakeSoftHolds_UnpointsTheHolderAndLeavesTheTrail(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "TAKE-CORE")
	holder := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "take-holder" })
	testutil.MustNoErr(t, reservations.Acquire(db.DB, holder.ID, holder.ID, bin.ID, "test"), "holder's promise")
	testutil.MustNoErr(t, db.UpdateOrderBinID(holder.ID, bin.ID), "holder points at the bin")
	taker := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "take-taker"
		o.OriginClass = protocol.OriginClassNoDemand
	})

	if hp := take(t, db, bin.ID, taker.ID); len(hp) != 0 {
		t.Fatalf("hand-placed = %v, want none: the holder is a Core-sourced order", hp)
	}

	after, err := db.GetOrder(holder.ID)
	testutil.MustNoErr(t, err, "reload holder")
	if after.BinID != nil {
		t.Errorf("holder still points at bin %d: it would confirm a bin that is leaving", *after.BinID)
	}
	if after.QueueCause != "bin-taken" || after.QueueReason != takeNote.Reason {
		t.Errorf("holder queue detail = %q / %q, want bin-taken and the sentence naming the taker",
			after.QueueCause, after.QueueReason)
	}
	if protocol.IsTerminal(after.Status) {
		t.Errorf("holder is %q: losing a promised bin is a recalculation, not a failure", after.Status)
	}
	var code, actor string
	var ref []byte
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT code, actor, ref FROM order_history
		WHERE order_id=$1 AND code='bin-taken'`, holder.ID).Scan(&code, &actor, &ref), "the trail")
	var r map[string]int64
	testutil.MustNoErr(t, json.Unmarshal(ref, &r), "ref")
	if r["taken_by"] != taker.ID || actor != "operator" {
		t.Errorf("history ref = %s actor = %q, want taken_by %d by the operator", ref, actor, taker.ID)
	}
	if rows := activeBinReservations(t, db, bin.ID); len(rows) != 1 {
		t.Errorf("bin has %d active reservations, want 1 (the taker's)", len(rows))
	}
}

func TestTakeSoftHolds_APersonsMoveIsHandedBackNotReaimed(t *testing.T) {
	t.Parallel()
	// The store reports the person's move and leaves it pointed; ending it by
	// name is the bin-move door's, which is not in this test (the door's own
	// docker test ends it). So the row this test ends on is the wedge, on purpose.
	testdb.DisableWedgeSweep(t, "the store hands the person's move back; the bin-move door ends it, and is not here")
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "TAKE-HAND")
	holder := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "take-hand-holder"
		o.OriginClass = protocol.OriginClassNoDemand
	})
	testutil.MustNoErr(t, reservations.Acquire(db.DB, holder.ID, holder.ID, bin.ID, "test"), "holder's promise")
	testutil.MustNoErr(t, db.UpdateOrderBinID(holder.ID, bin.ID), "holder points at the bin")
	taker := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "take-hand-taker" })

	hp := take(t, db, bin.ID, taker.ID)
	if len(hp) != 1 || hp[0] != holder.ID {
		t.Fatalf("hand-placed = %v, want [%d]: a person's move is ended by name, not re-aimed", hp, holder.ID)
	}
	after, err := db.GetOrder(holder.ID)
	testutil.MustNoErr(t, err, "reload holder")
	if after.BinID == nil || *after.BinID != bin.ID {
		t.Errorf("the hand-placed holder lost its pointer: re-finding a move re-aims it at another bin")
	}
}

// TestTakeSoftHolds_NeverTakesARobotsBin: a confirmed row is a robot's, and the
// take leaves it exactly as it was.
func TestTakeSoftHolds_NeverTakesARobotsBin(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "TAKE-ROBOT")
	holder := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "take-robot-holder" })
	_, err := db.DB.Exec(`INSERT INTO reservations (order_id, resource_kind, bin_id, state, reserved_by)
		VALUES ($1, 'bin', $2, 'confirmed', 'test')`, holder.ID, bin.ID)
	testutil.MustNoErr(t, err, "the robot's row")
	testutil.MustNoErr(t, db.UpdateOrderBinID(holder.ID, bin.ID), "holder points at the bin")
	taker := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "take-robot-taker" })

	tx, err := db.Begin()
	testutil.MustNoErr(t, err, "begin")
	defer tx.Rollback()
	hp, err := store.TakeSoftHolds(tx, bin.ID, taker.ID, takeNote)
	testutil.MustNoErr(t, err, "take")
	testutil.MustNoErr(t, tx.Commit(), "commit")
	if len(hp) != 0 {
		t.Errorf("hand-placed = %v, want none", hp)
	}
	after, err := db.GetOrder(holder.ID)
	testutil.MustNoErr(t, err, "reload holder")
	if after.BinID == nil || after.QueueCause == "bin-taken" {
		t.Errorf("the robot's order was touched: bin_id %v, cause %q", after.BinID, after.QueueCause)
	}
	if rows := activeBinReservations(t, db, bin.ID); len(rows) != 1 {
		t.Errorf("bin has %d active reservations, want the robot's 1", len(rows))
	}
}
