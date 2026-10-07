//go:build docker

package store_test

import (
	"encoding/json"
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// take_slot_by_rank_docker_test.go — a dig takes a promised slot by rank (C2b
// A.3; Rule 2 ⚖). The rank is compared under the lock the take holds, a tie or
// an older holder keeps the slot, and a taken holder is told who took it, on its
// row and in its history.

var slotTakeNote = store.HolderNote{
	Reason: "Its slot was taken by order #0, a dig that outranks it — finding another",
	Code:   protocol.QueueWaitingForSlot,
	Cause:  "slot-taken",
	Actor:  "dispatcher",
}

func TestTakeSlotByRank(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	slot := sd.StorageNode

	holder := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "slot-take-holder" })
	testutil.MustNoErr(t, db.ReserveSlot(slot.ID, holder.ID), "the holder's promise")

	// A TIE KEEPS THE INCUMBENT: same priority, and the taker is younger.
	peer := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "slot-take-peer" })
	if _, err := db.TakeSlotByRank(slot.ID, peer.ID, peer.ID, slotTakeNote); !errors.Is(err, store.ErrNoSlotTake) {
		t.Fatalf("a younger taker of the same priority took the slot (err %v)", err)
	}

	// AN OUTRANKING TAKER TAKES IT, AND THE HOLDER IS TOLD.
	taker := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "slot-take-taker" })
	_, err := db.Exec(`UPDATE orders SET priority = 100 WHERE id = $1`, taker.ID)
	testutil.MustNoErr(t, err, "rank the taker")
	got, err := db.TakeSlotByRank(slot.ID, taker.ID, taker.ID, slotTakeNote)
	testutil.MustNoErr(t, err, "the outranking take")
	if got != holder.ID {
		t.Fatalf("took the slot from order %d, want the holder %d", got, holder.ID)
	}
	var owner int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT order_id FROM reservations
		WHERE node_id = $1 AND resource_kind = 'slot' AND state = 'pending'`, slot.ID).Scan(&owner), "the slot's row")
	if owner != taker.ID {
		t.Fatalf("the slot's pending row belongs to %d, want the taker %d", owner, taker.ID)
	}
	after, err := db.GetOrder(holder.ID)
	testutil.MustNoErr(t, err, "reload the holder")
	if after.QueueCause != "slot-taken" || after.QueueReason != slotTakeNote.Reason {
		t.Errorf("the holder's wait is %q (%q), want slot-taken with the note", after.QueueCause, after.QueueReason)
	}
	var ref []byte
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT ref FROM order_history
		WHERE order_id = $1 AND code = 'slot-taken'`, holder.ID).Scan(&ref), "the trail")
	var r map[string]int64
	testutil.MustNoErr(t, json.Unmarshal(ref, &r), "parse the trail's ref")
	if r["taken_by"] != taker.ID {
		t.Errorf("the trail names %d, want the taker %d", r["taken_by"], taker.ID)
	}

	// A HOLDER WITH NOTHING PENDING IS NOT A TAKE.
	if _, err := db.TakeSlotByRank(slot.ID, taker.ID, taker.ID, slotTakeNote); !errors.Is(err, store.ErrNoSlotTake) {
		t.Fatalf("a second take found something to take (err %v)", err)
	}
}
