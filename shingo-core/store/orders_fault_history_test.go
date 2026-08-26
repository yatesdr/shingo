//go:build docker

package store_test

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// LatestHistoryForStatus answers "when did this fault start", and an order can
// fault more than once. The first faulted row would time a recovery from a
// fault the order already recovered from — a 20-second replan reported as an
// hour-old stall, which is exactly backwards from the distinction the whole
// fault-presentation design rests on.
func TestLatestOrderHistoryForStatus_ReturnsTheNewerOfTwoFaults(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	ord := &orders.Order{
		EdgeUUID: "fault-history-1", StationID: "edge.test",
		OrderType: "retrieve", Status: protocol.StatusInTransit,
		Quantity: 1, DeliveryNode: "DELV.1",
	}
	testutil.MustNoErr(t, db.CreateOrder(ord), "create order")

	// Two faults on one order, the second carrying a different fleet reason.
	// Written directly so the two rows' order is unambiguous.
	insert := func(detail string, code int, desc string, at time.Time) {
		t.Helper()
		_, err := db.DB.Exec(
			`INSERT INTO order_history (order_id, status, detail, actor, ref, created_at)
			 VALUES ($1, 'faulted', $2, 'fleet',
			         jsonb_build_object('node','ALN_003','vendor_code',$3::int,'vendor_desc',$4::text), $5)`,
			ord.ID, detail, code, desc, at)
		testutil.MustNoErr(t, err, "insert faulted row")
	}
	base := time.Now().UTC().Add(-time.Hour)
	insert("first fault", 60011, "cannot replan", base)
	insert("second fault", 54018, "robot suspended", base.Add(30*time.Minute))

	got, err := db.LatestOrderHistoryForStatus(ord.ID, protocol.StatusFaulted)
	testutil.MustNoErr(t, err, "LatestOrderHistoryForStatus")
	if got == nil {
		t.Fatal("an order with two faulted rows must return one, not nil")
	}
	if got.Detail != "second fault" {
		t.Errorf("detail = %q, want the NEWER row's %q", got.Detail, "second fault")
	}
	if got.Ref == nil {
		t.Fatal("the ref must survive the read — it is the fleet's reason")
	}
	if got.Ref.VendorCode != 54018 || got.Ref.VendorDesc != "robot suspended" {
		t.Errorf("ref = %+v, want the newer row's vendor pair", *got.Ref)
	}

	// A status the order never reached is nil and no error: the caller asking
	// "when did this fault start" about an order that never faulted decides
	// what that means, rather than being handed an error it must special-case.
	none, err := db.LatestOrderHistoryForStatus(ord.ID, protocol.StatusCancelled)
	testutil.MustNoErr(t, err, "LatestOrderHistoryForStatus for an unreached status")
	if none != nil {
		t.Errorf("a status the order never reached must be nil, got %+v", none)
	}
}
