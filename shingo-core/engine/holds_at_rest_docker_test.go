//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/internal/testdb"
)

// holds_at_rest_docker_test.go — the invariant that no order holds a hard claim
// while it waits (orders.HoldsAtRestSQL), driven through the real scanner.
//
// A hard claim (bins.claimed_by, nodes.claimed_by) exists for the fleet call. An
// order that is still pending, sourcing or queued has no fleet call in flight,
// so whatever it holds hard is withheld from every other order for as long as it
// waits, and it counts at the dropoff gate as if it were on its way.

// TestHoldsAtRest_OrdinaryTrafficHoldsNothingWhileWaiting is the characterisation
// half: the ordinary ways an order waits today already hold nothing hard. A
// retrieve parked for want of material, a second retrieve parked behind the first
// at the dropoff gate, and an order the fleet refused (the demote takes the armor
// off) are each checked at rest.
//
// Green at the base and green after the one-step acquire. If this goes red, a
// new way to wait while holding has been introduced, which is the regression the
// invariant exists to catch.
func TestHoldsAtRest_OrdinaryTrafficHoldsNothingWhileWaiting(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, db, backend)

	submit := func(uuid string) {
		eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
			OrderUUID: uuid, OrderType: dispatch.OrderTypeRetrieve, PayloadCode: sd.Payload.Code,
			SourceNode: sd.StorageNode.Name, DeliveryNode: sd.LineNode.Name, Quantity: 1,
		})
		eng.RunFulfillmentScan()
	}
	rest := func(when string) {
		t.Helper()
		testdb.AssertNoHoldsAtRest(t, db, when)
	}

	// No material: it waits for a bin and holds nothing.
	submit("har-nomat")
	if o := testdb.RequireOrder(t, db, "har-nomat"); !protocol.IsPreDispatch(o.Status) {
		t.Fatalf("fixture: with no bin in storage the retrieve is %q, want it waiting", o.Status)
	}
	rest("a retrieve waiting for material")

	// The fleet refuses: the demote takes the armor off.
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "HAR-BIN-A")
	backend.SetFail(true)
	eng.RunFulfillmentScan()
	if o := testdb.RequireOrder(t, db, "har-nomat"); o.QueueCause != string(dispatch.CauseFleetRefusedCreate) {
		t.Fatalf("fixture: after a bin arrived with the fleet refusing, the order is %q under %q, "+
			"want it demoted under %s", o.Status, o.QueueCause, dispatch.CauseFleetRefusedCreate)
	}
	rest("an order the fleet refused")

	// The fleet recovers and the first order goes; a second to the same line parks
	// behind it at the dropoff gate.
	backend.SetFail(false)
	eng.RunFulfillmentScan()
	if o := testdb.RequireOrder(t, db, "har-nomat"); o.VendorOrderID == "" {
		t.Fatalf("fixture: after the fleet recovered the first order is %q under %q, want it dispatched",
			o.Status, o.QueueCause)
	}
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "HAR-BIN-B")
	submit("har-behind")
	if o := testdb.RequireOrder(t, db, "har-behind"); o.VendorOrderID != "" {
		t.Fatalf("fixture: the second retrieve to %s dispatched while the first is inbound", sd.LineNode.Name)
	}
	rest("a retrieve parked behind another at the dropoff gate")
}

// TestHoldsAtRest_SplitConfirmParksHoldingTheSlot — red at the base, green
// with the one-step acquire.
//
// At the base, ConfirmForDispatch hard-claimed the destination slot in one
// transaction and then the bin in another. When the bin leg failed, the scanner
// parked the order in `sourcing` under claim-failed and the slot's hard claim was
// never rolled back: the order waited holding nodes.claimed_by (and a confirmed
// slot reservation) on its destination. Flat bug, SYNTH §0 row 1.
//
// Now the slot and the bin are confirmed in the order's hand (DispatchHand), one
// transaction: the bin's refusal rolls the slot back, and the order waits
// holding only its SOFT reservations — which it must keep (Rule 1), so the next
// tick re-enters through the held-bin arm without re-shopping.
//
// The lever is a lock on the bin after the order soft-held it: the hold still
// stands (so the arm is the transient "retry" one, not the lost-hold one), the
// slot confirms inside the hand, the bin's claim CAS refuses.
func TestHoldsAtRest_SplitConfirmParksHoldingTheSlot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, db, backend)

	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "HAR-SPLIT-BIN")

	// First pass: the fleet refuses, so the order is left soft-holding its bin and
	// its destination slot, with its bin pointer stamped. That is the state the
	// held-bin arm re-enters from.
	backend.SetFail(true)
	eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "har-split", OrderType: dispatch.OrderTypeMove, PayloadCode: sd.Payload.Code,
		SourceNode: sd.LineNode.Name, DeliveryNode: sd.StorageNode.Name, Quantity: 1,
	})
	eng.RunFulfillmentScan()
	o := testdb.RequireOrder(t, db, "har-split")
	if o.BinID == nil || *o.BinID != bin.ID || o.QueueCause != string(dispatch.CauseFleetRefusedCreate) {
		t.Fatalf("fixture: the move is %q under %q with bin %v, want it demoted by the fleet refusal "+
			"holding bin %d", o.Status, o.QueueCause, o.BinID, bin.ID)
	}
	if v := testdb.HoldsAtRest(t, db); len(v) != 0 {
		t.Fatalf("fixture: the demoted move already holds hard before the split confirm runs: %v", v)
	}

	// The bin is locked under the order's soft hold; the fleet is back.
	_, err := db.DB.Exec(`UPDATE bins SET locked = true WHERE id = $1`, bin.ID)
	testutil.MustNoErr(t, err, "lock the bin")
	backend.SetFail(false)
	eng.RunFulfillmentScan()

	o = testdb.RequireOrder(t, db, "har-split")
	if o.VendorOrderID != "" || o.QueueCause != string(dispatch.CauseClaimFailed) {
		t.Fatalf("fixture: the move is %q under %q (vendor %q), want it parked under %s by the "+
			"failed bin leg", o.Status, o.QueueCause, o.VendorOrderID, dispatch.CauseClaimFailed)
	}
	testdb.AssertNoHoldsAtRest(t, db, "a move whose bin refused after its slot")
	var soft []string
	rows, err := db.DB.Query(`SELECT resource_kind || ':' || state FROM reservations WHERE order_id = $1 ORDER BY 1`, o.ID)
	testutil.MustNoErr(t, err, "read the order's reservations")
	defer rows.Close()
	for rows.Next() {
		var r string
		testutil.MustNoErr(t, rows.Scan(&r), "scan reservation")
		soft = append(soft, r)
	}
	if len(soft) != 2 || soft[0] != "bin:pending" || soft[1] != "slot:pending" {
		t.Errorf("the parked move holds %v, want exactly [bin:pending slot:pending]: a refused hand keeps "+
			"its soft holds (Rule 1), so the held-bin arm can re-confirm them next tick", soft)
	}
}
