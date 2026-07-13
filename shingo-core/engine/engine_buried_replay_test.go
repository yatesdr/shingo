//go:build docker

package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
)

// TestBuriedAfterQueue_ScannerMustPlanReshuffle pins the liveness invariant the
// buried-bin path is missing: an order that becomes buried WHILE QUEUED must
// still get a reshuffle.
//
// Reshuffle planning lives only in planTransport, which runs once, at intake
// (PlanningService.Register wires it to the three simple order types and nothing
// re-invokes it). Replay goes through the fulfillment scanner, and the scanner's
// OutcomeReshuffle arm does not plan — it re-queues with "source bin buried;
// awaiting reshuffle" (scanner.go). So any order whose source is accessible at
// intake but buried by the time its destination frees is stuck forever: no
// component in the system will ever unbury that lane on its behalf.
//
// The sequence below is ordinary plant behaviour, not a contrived race:
//
//  1. A retrieve is raised while its delivery line is occupied → capacity-gated,
//     queued. Its source is accessible at this point, so intake resolves
//     OutcomeFound and plans nothing.
//  2. While it waits, a store drops a bin into the lane mouth ahead of the
//     target. The queued order's source is now buried.
//  3. The line frees. The scanner replays: the capacity gate passes (scanner.go
//     checks it BEFORE resolving the source), the source resolves buried — and
//     the scanner has no way to act on that.
//
// The order must end up reshuffling with compound children. Deliberately
// ordering-independent: at intake the source is NOT buried, so this fails the
// same way whether the capacity gate runs before or after source resolution —
// it is not a regression in the branch's reorder, it is the gap the reorder was
// reaching for.
func TestBuriedAfterQueue_ScannerMustPlanReshuffle(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	// Lane: slot 1 (mouth) + slot 2 (target, oldest → FIFO picks it).
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix:     "DLKB",
		NumSlots:   2,
		TargetSlot: 2,
		TargetAge:  2 * time.Hour,
	})
	blocker := sc.Blockers[0]

	// Pull the blocker out of the lane mouth so the target is ACCESSIBLE at intake.
	// The order must queue on capacity alone, with nothing buried yet.
	mustExec(t, db, `UPDATE bins SET node_id=$1 WHERE id=$2`, sc.ShuffleSlots[0].ID, blocker.ID)

	// Occupy the delivery line so the retrieve is capacity-gated.
	lineBin := &bins.Bin{BinTypeID: sc.BinType.ID, Label: "DLKB-LINE-OCC", NodeID: &sc.LineNode.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(lineBin), "occupy the line")

	eng := newTestEngine(t, db, simulator.New())
	eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:    "buried-after-queue",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  sc.Payload.Code,
		SourceNode:   sc.Grp.Name,
		DeliveryNode: sc.LineNode.Name,
		Quantity:     1,
	})

	order := testdb.RequireOrder(t, db, "buried-after-queue")
	if order.Status != dispatch.StatusQueued {
		t.Fatalf("precondition: order status = %q, want %q — the occupied line must gate it (source is accessible, so no reshuffle is expected yet)",
			order.Status, dispatch.StatusQueued)
	}

	// The lane gets buried while the order sits queued: a store puts a bin back
	// in the mouth, ahead of the target.
	mustExec(t, db, `UPDATE bins SET node_id=$1 WHERE id=$2`, sc.Slots[0].ID, blocker.ID)

	// The line frees. Every gate the order was waiting on is now clear.
	mustExec(t, db, `DELETE FROM bins WHERE id=$1`, lineBin.ID)

	// Replay. Several passes: this is a liveness claim, not a timing one — if the
	// order can ever recover, it recovers here.
	for i := 0; i < 3; i++ {
		eng.RunFulfillmentScan()
	}

	order = testdb.RequireOrder(t, db, "buried-after-queue")
	children, err := db.ListChildOrders(order.ID)
	testutil.MustNoErr(t, err, "list child orders")

	if order.Status != dispatch.StatusReshuffling || len(children) == 0 {
		t.Fatalf("order stranded: status = %q (%d children), want %q with compound children.\n"+
			"queue_reason = %q\n"+
			"The source is buried and the destination is free, but reshuffle planning lives only at "+
			"intake (planTransport) and the scanner cannot spawn a compound on replay — so nothing "+
			"will ever unbury this lane and the order never moves.",
			order.Status, len(children), dispatch.StatusReshuffling, order.QueueReason)
	}
}

// mustExec runs a raw statement for test fixture manipulation (moving a bin
// between nodes mid-scenario), which the store API deliberately has no verb for.
func mustExec(t *testing.T, db *store.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
