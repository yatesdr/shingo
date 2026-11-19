//go:build docker

package store_test

import (
	"testing"

	"shingo/protocol"
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
	_, moved, err := db.FleetYes(o.ID, protocol.StatusPending, "sg-hiw-1", "", "vendor order sg-hiw-1 created",
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
