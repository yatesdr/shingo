//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// historyPairs returns an order's history as (status, code) pairs, oldest first.
func historyPairs(t *testing.T, db *store.DB, orderID int64) [][2]string {
	t.Helper()
	rows, err := db.ListOrderHistory(orderID)
	testutil.MustNoErr(t, err, "list order history")
	out := make([][2]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, [2]string{string(r.Status), r.Code})
	}
	return out
}

// TestComplexPark_TheWaitLandsOnItsOwnEpisode is the end-to-end form of the
// total-loss defect, driven through the real dispatch path.
//
// A complex order is born `queued` by INSERT — no transition, so once there was
// no queued history row — and the stamp named 'queued', so the UPDATE matched
// nothing, silently, for the order's whole life. Every swap leg's and every
// changeover leg's wait history was never recorded at all.
//
// The source bin here is present but taken by another order — sourceable once
// that order finishes, so the reserve HOLDS. That is the reserve-holding park,
// and it happens after MoveToSourcing, so the order rests in `sourcing`: the
// cause must land on the SOURCING row, the wait it describes, and the queued
// birth row it left must be untouched, which is the mis-attribution half.
func TestComplexPark_TheWaitLandsOnItsOwnEpisode(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	nodeA := &nodes.Node{Name: "QEH-A", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(nodeA), "node A")
	binA := testdb.CreateBinAtNode(t, db, bp.Code, nodeA.ID, "QEH-BIN-A")
	other := &orders.Order{EdgeUUID: "qeh-other", StationID: "s", OrderType: OrderTypeRetrieve,
		Status: StatusSourcing, Quantity: 1}
	testutil.MustNoErr(t, db.CreateOrder(other), "create the holding order")
	testdb.ClaimBinForTest(t, db, binA.ID, other.ID)

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: nodeA.Name},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
	}
	order := mkComplexOrder(t, db, "qeh-partial", nodeA.Name, nodeA.Name, lineNode.Name, bp.Code, steps)
	// Born `queued` by the INSERT, exactly as complex intake writes it.
	_, err := db.DB.Exec(`UPDATE orders SET status=$1 WHERE id=$2`, string(StatusQueued), order.ID)
	testutil.MustNoErr(t, err, "born queued")
	_, err = db.DB.Exec(`UPDATE order_history SET status=$1 WHERE order_id=$2`, string(StatusQueued), order.ID)
	testutil.MustNoErr(t, err, "birth row born queued")
	order.Status = StatusQueued

	_ = d.DispatchPreparedComplex(order)

	got, gerr := db.GetOrder(order.ID)
	testutil.MustNoErr(t, gerr, "re-read order")
	if got.QueueCode != string(protocol.QueueWaitingForMaterial) {
		t.Fatalf("live queue_code = %q, want %q — this test is on the reserve-holding arm or it is testing nothing",
			got.QueueCode, protocol.QueueWaitingForMaterial)
	}

	pairs := historyPairs(t, db, order.ID)
	if len(pairs) == 0 {
		t.Fatal("the order has no history at all")
	}
	last := pairs[len(pairs)-1]
	if last[0] != string(StatusSourcing) || last[1] != string(protocol.QueueWaitingForMaterial) {
		t.Errorf("the wait's own row = %v, want [sourcing %s].\n"+
			"A complex order parks in sourcing, so that is the episode its cause describes. Full history: %v",
			last, protocol.QueueWaitingForMaterial, pairs)
	}
	if pairs[0][0] != string(StatusQueued) {
		t.Fatalf("history[0] = %v, want the queued birth row. Full history: %v", pairs[0], pairs)
	}
	if pairs[0][1] != "" {
		t.Errorf("the birth row carries code %q. It is a PREVIOUS episode: nothing was waiting when the "+
			"order was created, and a cause written for the sourcing wait must not be attributed to it. Full history: %v",
			pairs[0][1], pairs)
	}
}

// TestOrders_ASecondWaitDoesNotRewriteTheFirst is the mis-attribution case on a
// plain order, at the store layer where the stamp lives.
//
// It is the case round 6 found and it is the worse one: an order parked in
// `sourcing` had its cause written onto its last QUEUED row — a different,
// earlier wait, minutes old — so the time series carried the new reason at the
// old moment, on the old rung.
func TestOrders_ASecondWaitDoesNotRewriteTheFirst(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	setupTestData(t, db)

	o := &orders.Order{
		EdgeUUID: "qeh-two-waits", StationID: "line-1", OrderType: protocol.OrderTypeRetrieve,
		Status: StatusQueued, Quantity: 1, DeliveryNode: "LINE1-IN",
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")

	testutil.MustNoErr(t, db.SetOrderQueueDetail(o.ID,
		"Storage is being rearranged", protocol.QueueStorageRearranging, string(CauseLaneHeldTraffic)), "first wait")

	moved, err := db.UpdateOrderStatusFromWithReason(o.ID,
		string(StatusQueued), string(StatusSourcing), "reserving source bins", store.HistoryReason{})
	testutil.MustNoErr(t, err, "queued→sourcing")
	if !moved {
		t.Fatal("queued→sourcing refused")
	}
	testutil.MustNoErr(t, db.SetOrderQueueDetail(o.ID,
		"Waiting for material: PART-A", protocol.QueueWaitingForMaterial, string(CauseReserveHolding)), "second wait")

	want := [][2]string{
		{string(StatusQueued), string(protocol.QueueStorageRearranging)},
		{string(StatusSourcing), string(protocol.QueueWaitingForMaterial)},
	}
	got := historyPairs(t, db, o.ID)
	if len(got) != len(want) {
		t.Fatalf("history = %v, want one row per wait %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("history[%d] = %v, want %v — each wait keeps its own reason at its own moment. Full history: %v",
				i, got[i], want[i], got)
		}
	}
}
