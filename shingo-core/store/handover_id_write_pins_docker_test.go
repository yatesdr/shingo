//go:build docker

package store_test

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestHandoverIDWrite_LandsOnAnEndedRow: the handover names the fleet's job on
// the order after the create returns, in a write of its own
// (dispatch/fleet_handover.go, UpdateOrderVendor) whose WHERE carries no status
// and no version, after a re-read that is not atomic with it. A cancel that
// lands between the re-read and the write leaves a cancelled row naming a live
// job (`vendor_state` CREATED); the cancel's own fleet call ran before there was
// an id to cancel. The terminate chase finds it 30 s later
// (engine/fleet_terminate_chase.go); nothing stops it sooner.
//
// A FLEET pin: it flips when `dispatched` and the vendor id are written in one
// statement guarded on the status and the claim, which refuses an ended row and
// sends the handover to cancel the job.
func TestHandoverIDWrite_LandsOnAnEndedRow(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	o := &orders.Order{EdgeUUID: "hiw-ended", StationID: "edge.1", OrderType: "retrieve",
		Status: protocol.StatusPending, Quantity: 1, DeliveryNode: "LINE"}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create: %v", err)
	}
	won, err := db.TerminalizeOrder(o.ID, protocol.StatusCancelled, "an operator cancel during the create")
	if err != nil || !won {
		t.Fatalf("the cancel: won=%v err=%v", won, err)
	}

	// The handover, past its re-read.
	if err := db.UpdateOrderVendor(o.ID, "sg-hiw-1", "CREATED", ""); err != nil {
		t.Fatalf("the handover's id write: %v", err)
	}
	after, err := db.GetOrder(o.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if after.Status != protocol.StatusCancelled || after.VendorOrderID != "sg-hiw-1" || after.VendorState != "CREATED" {
		t.Fatalf("the cancelled row reads %s vendor %q state %q; as built the write lands on it",
			after.Status, after.VendorOrderID, after.VendorState)
	}
}
