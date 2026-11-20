//go:build docker

package store_test

import (
	"database/sql"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// TestHandoverIDWrite_TheYesRefusesAnEndedRow: the fleet's job is named on the
// order by the yes write (FleetYes), in the statement that writes `dispatched`,
// guarded on the status the hand found and on the ask it opened. An order
// cancelled while the fleet was creating its job is not written: the yes finds
// no row, the cancelled row names no job, and the handover cancels the job and
// keeps it named on the ended row only if the fleet did not take the cancel
// (dispatch TestAsk_ACancelInsideTheCreateKeepsTheJobNamed).
//
// A FLEET pin: as built before, the handover named the job in a write of its own
// (UpdateOrderVendor) whose WHERE carried no status, after a re-read not atomic
// with it, so a cancel landing between the two left a cancelled row naming a
// live job for the terminate chase to find 30 s later.
func TestHandoverIDWrite_TheYesRefusesAnEndedRow(t *testing.T) {
	t.Parallel()
	testdb.ExpectAskLeftOpen(t, "an ended row keeps the ask the handover settles; this pin stops before the handover")
	db := testdb.Open(t)
	o := &orders.Order{EdgeUUID: "hiw-ended", StationID: "edge.1", OrderType: "retrieve",
		Status: protocol.StatusPending, Quantity: 1, DeliveryNode: "LINE"}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`UPDATE orders SET asked_vendor_order_id='sg-hiw-1' WHERE id=$1`, o.ID); err != nil {
		t.Fatalf("the hand's ask: %v", err)
	}
	won, err := db.TerminalizeOrder(o.ID, protocol.StatusCancelled, "an operator cancel during the create")
	if err != nil || !won {
		t.Fatalf("the cancel: won=%v err=%v", won, err)
	}

	// The fleet said yes.
	_, moved, err := db.FleetYes(o.ID, protocol.StatusPending, "sg-hiw-1", "", nil, "vendor order sg-hiw-1 created",
		store.HistoryReason{}, protocol.OrderUpdate{})
	if err != nil {
		t.Fatalf("the yes write: %v", err)
	}
	after, err := db.GetOrder(o.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if moved || after.Status != protocol.StatusCancelled || after.VendorOrderID != "" {
		t.Fatalf("the yes on a cancelled row: moved %v, it reads %s vendor %q; want no write, cancelled, no job",
			moved, after.Status, after.VendorOrderID)
	}
	if after.AskedVendorOrderID != "sg-hiw-1" {
		t.Errorf("the cancelled row's ask = %q, want sg-hiw-1 kept for the handover to settle", after.AskedVendorOrderID)
	}
}

// TestBlockRecord_TheYesReplacesAndTheAppendMerges reads the two writers of
// orders.block_actions back. The fleet's yes (FleetYes) replaces the record
// with the blocks the create sent; the append's claim (ClaimAppend) merges its
// segment's blocks in, and records nothing when the claim refuses. An order
// sent before the record (NULL) stays NULL through an append, so it is read by
// its block names for its whole life, until a create of a new vendor order
// records every block it sends. Moved whole from store's
// TestBlockRecord_TheCreateReplacesAndTheAppendMerges, which sat in
// migration_v179_block_record_test.go and ran no migration.
func TestBlockRecord_TheYesReplacesAndTheAppendMerges(t *testing.T) {
	db := testdb.Open(t)
	live := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "rec-live"; o.StepsJSON = `[{"action":"wait"}]` })
	old := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "rec-old"; o.StepsJSON = `[{"action":"wait"}]` })
	_, err := db.Exec(`UPDATE orders SET block_actions = NULL WHERE id = $1`, old.ID)
	testutil.MustNoErr(t, err, "the order sent before the record")
	read := func(id int64) map[string]string {
		t.Helper()
		o, err := db.GetOrder(id)
		return testutil.Must(t, o, err, "read the order").BlockActions
	}

	// The create's write is the fleet's yes: Core asks for the job, and the yes
	// names it and records the blocks it sent.
	yes := func(id int64, job string, actions map[string]string, what string) {
		t.Helper()
		testdb.SeedAsk(t, db, id, protocol.StatusQueued, job)
		_, moved, err := db.FleetYes(id, protocol.StatusQueued, job, "", actions, "vendor order "+job+" created",
			store.HistoryReason{}, protocol.OrderUpdate{})
		if err != nil || !moved {
			t.Fatalf("%s: moved=%v err=%v", what, moved, err)
		}
	}
	yes(live.ID, "V-live", map[string]string{"V-live-b1": "pickup", "V-live-b2": "wait"}, "the create's write")
	if got := read(live.ID); len(got) != 2 || got["V-live-b1"] != "pickup" || got["V-live-b2"] != "wait" {
		t.Fatalf("after the create the record is %v, want the two blocks it sent", got)
	}
	owed, err := db.ClaimAppend(live.ID, 1, `[{"action":"wait"}]`, map[string]string{"V-live-b3": "dropoff"})
	testutil.MustNoErr(t, err, "a refused claim")
	if owed || len(read(live.ID)) != 2 {
		t.Errorf("a claim at the wrong wait index answered %v and left %v, want refused and the record unchanged", owed, read(live.ID))
	}
	owed, err = db.ClaimAppend(live.ID, 0, `[{"action":"wait"}]`, map[string]string{"V-live-b3": "dropoff", "V-live-b4": ""})
	testutil.MustNoErr(t, err, "the claim")
	if got := read(live.ID); !owed || len(got) != 4 || got["V-live-b3"] != "dropoff" || got["V-live-b4"] != "" || got["V-live-b1"] != "pickup" {
		t.Errorf("after the append (owed %v) the record is %v, want the create's two and the segment's two", owed, got)
	}

	owed, err = db.ClaimAppend(old.ID, 0, `[{"action":"wait"}]`, map[string]string{"V-old-b3": "dropoff"})
	testutil.MustNoErr(t, err, "the old order's claim")
	var raw sql.NullString
	testutil.MustNoErr(t, db.QueryRow(`SELECT block_actions::text FROM orders WHERE id=$1`, old.ID).Scan(&raw), "read the column")
	if !owed || raw.Valid {
		t.Errorf("the old order's claim answered %v and left block_actions %q, want claimed and NULL", owed, raw.String)
	}

	// An order born before the record and sent after it (it queued across the
	// deploy) is recorded by its create: every block of the new vendor order is
	// sent now, so the create's write replaces the NULL.
	yes(old.ID, "V-old2", map[string]string{"V-old2-b1": "pickup"}, "the create of an order born before the record")
	if got := read(old.ID); len(got) != 1 || got["V-old2-b1"] != "pickup" {
		t.Errorf("an order sent after the record reads %v, want the one block its create sent", got)
	}
}
