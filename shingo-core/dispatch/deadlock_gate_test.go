//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/orders"
)

// TestDeadlockGate_CoordinatedToOccupiedLine_NotGated is the dedicated,
// end-to-end test for the deadlock-critical fast-path (2b05dce): a COORDINATED
// order whose delivery is a LINE the resident bin still occupies must NOT be
// gated — a sibling evac clears the line and Core can't model that, so gating it
// deadlocks. It runs the real DispatchPreparedComplex (not a stub), so it
// exercises the role gate at complex_dispatch.go:359 with an occupied line.
//
// The plan is 2-step and NO-wait on purpose: it is the exact shape of
// BuildStagedDeliverSteps (a real no-wait complex changeover delivery to the
// line) — the case the earlier wait-presence discriminator would have
// misclassified. It must dispatch, proving both the line fast-path and that a
// no-wait coordinated line delivery is not gated.
func TestDeadlockGate_CoordinatedToOccupiedLine_NotGated(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})

	// The line is OCCUPIED — the resident a sibling evac would remove.
	lineBin := &bins.Bin{BinTypeID: bt.ID, Label: "LINE-RESIDENT", NodeID: &lineNode.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(lineBin), "occupy the line")
	testutil.MustNoErr(t, db.SetBinManifest(lineBin.ID, `{"items":[{"catid":"PART-A","qty":10}]}`, bp.Code, 10), "manifest")
	testutil.MustNoErr(t, db.ConfirmBinManifest(lineBin.ID, ""), "confirm")

	// The delivery's source bin at storage.
	srcBin := &bins.Bin{BinTypeID: bt.ID, Label: "DELIVER-BIN", NodeID: &storageNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(srcBin), "create source bin")
	testutil.MustNoErr(t, db.SetBinManifest(srcBin.ID, `{"items":[{"catid":"PART-A","qty":40}]}`, bp.Code, 40), "manifest")
	testutil.MustNoErr(t, db.ConfirmBinManifest(srcBin.ID, ""), "confirm")

	// Coordinated (StepsJSON), no wait, delivering TO the occupied line, no sibling
	// link (so swapRemovalLegHeld is not the reason it could hold).
	order := &orders.Order{
		EdgeUUID: "coord-occ-line", StationID: "line-1", OrderType: OrderTypeComplex,
		Status: StatusQueued, Quantity: 1, PayloadCode: bp.Code,
		SourceNode: storageNode.Name, DeliveryNode: lineNode.Name, ProcessNode: lineNode.Name,
		StepsJSON: `[{"action":"pickup","node":"` + storageNode.Name + `"},{"action":"dropoff","node":"` + lineNode.Name + `"}]`,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	order, _ = db.GetOrderByUUID("coord-occ-line")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	if err := d.DispatchPreparedComplex(order); err != nil {
		t.Fatalf("coordinated delivery to an OCCUPIED line must NOT be gated (2b05dce fast-path), got: %v", err)
	}
	got, _ := db.GetOrderByUUID("coord-occ-line")
	if got.Status != StatusDispatched {
		t.Fatalf("status = %q, want dispatched — the occupied line must not block a coordinated delivery", got.Status)
	}
	if got.VendorOrderID == "" {
		t.Error("expected a vendor order id (dispatched to fleet)")
	}
}

// TestDeadlockGate_PlainRetrieveToOccupiedLine_Gated is the dedicated companion:
// a PLAIN retrieve delivering to an occupied line MUST be gated (the full
// occupancy gate) — the round-7 no-leak requirement. The fast-path lives only in
// the coordinated branch, so a single-transport order to an occupied line stays
// blocked, never dispatches into the collision.
func TestDeadlockGate_PlainRetrieveToOccupiedLine_Gated(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})

	// A source bin is available (so the ONLY blocker is the occupied line).
	srcBin := &bins.Bin{BinTypeID: bt.ID, Label: "RET-SRC", NodeID: &storageNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(srcBin), "create source bin")
	testutil.MustNoErr(t, db.SetBinManifest(srcBin.ID, `{"items":[{"catid":"PART-A","qty":40}]}`, bp.Code, 40), "manifest")
	testutil.MustNoErr(t, db.ConfirmBinManifest(srcBin.ID, ""), "confirm")

	// The line (delivery) is OCCUPIED.
	lineBin := &bins.Bin{BinTypeID: bt.ID, Label: "LINE-OCC", NodeID: &lineNode.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(lineBin), "occupy the line")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	order := &orders.Order{
		EdgeUUID: "plain-ret-occ-line", StationID: "line-1", OrderType: OrderTypeRetrieve,
		Status: StatusPending, Quantity: 1, PayloadCode: bp.Code, DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	result, perr := d.planner.planRetrieve(order, testEnvelope(), bp.Code)
	if perr != nil {
		t.Fatalf("planRetrieve: %s", perr.Detail)
	}
	if !result.Queued {
		t.Fatal("a plain retrieve to an OCCUPIED line MUST be gated (queued), never dispatched")
	}
	if order.BinID != nil {
		t.Errorf("a gated retrieve must not claim a source bin, got bin %d", *order.BinID)
	}
	if reloaded, err := db.GetOrder(order.ID); err == nil && reloaded.QueueReason == "" {
		t.Error("gated retrieve must carry a queue_reason")
	}
}
