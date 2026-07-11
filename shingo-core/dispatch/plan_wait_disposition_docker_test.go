//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// TestSimpleRetrieve_UnsourceableWaits_NeverSkipped pins the wait disposition:
// an unsatisfiable SIMPLE order WAITS (stays acquiring, re-scanned each tick) —
// it is NEVER skipped (that disposition is complex-only, emitted by
// reserveComplexPlan for a void evac) and never terminally failed for a merely-
// absent source. Once the source appears it dispatches. A simple order keeps its
// single-bin path and never routes through the complex reserve, so a skip is
// structurally unreachable for it.
func TestSimpleRetrieve_UnsourceableWaits_NeverSkipped(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// A retrieve to a free line, but NO source bin exists yet.
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "q1-wait-1", OrderType: OrderTypeRetrieve, PayloadCode: bp.Code,
		DeliveryNode: lineNode.Name, Quantity: 1,
	})

	// Intake queued it — not skipped, not failed.
	o, err := db.GetOrderByUUID("q1-wait-1")
	testutil.MustNoErr(t, err, "get order")
	if !protocol.IsAcquiring(o.Status) {
		t.Fatalf("unsourceable retrieve status = %q, want acquiring (queued/sourcing)", o.Status)
	}

	// Re-scanned several times with the source still absent — stays acquiring, is
	// never skipped or failed (demand never evaporates).
	for i := 0; i < 3; i++ {
		o = dispatchSimpleViaScanner(t, d, db, "q1-wait-1")
		if o.Status == protocol.StatusSkipped {
			t.Fatalf("tick %d: unsourceable simple was SKIPPED — skip is complex-only", i)
		}
		if protocol.IsTerminal(o.Status) {
			t.Fatalf("tick %d: unsourceable simple reached terminal %q — a merely-absent source must WAIT", i, o.Status)
		}
		if !protocol.IsAcquiring(o.Status) {
			t.Fatalf("tick %d: status = %q, want still acquiring", i, o.Status)
		}
	}

	// Source appears → the next scan claims + dispatches.
	src := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "WAIT-SRC")
	o = dispatchSimpleViaScanner(t, d, db, "q1-wait-1")
	if o.Status != StatusDispatched {
		t.Fatalf("after source appeared, status = %q, want dispatched", o.Status)
	}
	if o.BinID == nil || *o.BinID != src.ID {
		t.Fatalf("claimed bin = %v, want %d", o.BinID, src.ID)
	}
}

// TestSimpleMove_OccupiedDest_Waits pins the destination-occupancy invariant:
// a simple order has NO evac leg, so a move — including a changeover delivery
// that Edge downgraded to a plain move on stale telemetry — to an OCCUPIED
// destination WAITS on the Core-side occupancy guard rather than double-dropping
// onto the incumbent bin (the exact bug an evac leg exists to prevent). It is
// never skipped either. Once the dest clears, it delivers.
func TestSimpleMove_OccupiedDest_Waits(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})

	srcNode := &nodes.Node{Name: "MOVE-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(srcNode), "create src")
	dstNode := &nodes.Node{Name: "MOVE-DST", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dstNode), "create dst")

	// The move's source bin.
	mvBin := &bins.Bin{BinTypeID: bt.ID, Label: "MOVE-MV", NodeID: &srcNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(mvBin), "create move bin")
	testutil.MustNoErr(t, db.SetBinManifest(mvBin.ID, `{"items":[{"catid":"PART-A","qty":40}]}`, bp.Code, 40), "manifest")
	testutil.MustNoErr(t, db.ConfirmBinManifest(mvBin.ID, ""), "confirm")

	// The destination is OCCUPIED — an incumbent bin is already there.
	incumbent := &bins.Bin{BinTypeID: bt.ID, Label: "MOVE-INCUMBENT", NodeID: &dstNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(incumbent), "create incumbent bin")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "move-occ-1", OrderType: OrderTypeMove, PayloadCode: bp.Code,
		SourceNode: srcNode.Name, DeliveryNode: dstNode.Name, Quantity: 1,
	})

	// The occupancy guard queued it — never dispatched onto the incumbent, never skipped.
	o := dispatchSimpleViaScanner(t, d, db, "move-occ-1")
	if o.Status == StatusDispatched {
		t.Fatal("move to an OCCUPIED dest dispatched — double-drop; a simple order must WAIT")
	}
	if o.Status == protocol.StatusSkipped {
		t.Fatal("move to an OCCUPIED dest was SKIPPED — must WAIT, not skip")
	}
	if !protocol.IsAcquiring(o.Status) {
		t.Fatalf("status = %q, want acquiring (waiting on the occupied dest)", o.Status)
	}

	// Clear the destination → the move delivers.
	testutil.MustNoErr(t, db.DeleteBin(incumbent.ID), "clear the occupied dest")
	o = dispatchSimpleViaScanner(t, d, db, "move-occ-1")
	if o.Status != StatusDispatched {
		t.Fatalf("after dest cleared, status = %q, want dispatched", o.Status)
	}
	if o.BinID == nil || *o.BinID != mvBin.ID {
		t.Fatalf("claimed bin = %v, want move bin %d", o.BinID, mvBin.ID)
	}
}
