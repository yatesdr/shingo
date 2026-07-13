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
	"shingocore/store/bins"
)

// TestBuriedRetrieve_NoShuffleSlot_WaitsThenReshuffles pins the D79
// reshuffle-disposition rider: a buried retrieve with nowhere to park its
// blockers must WAIT for a shuffle slot, not die.
//
// This is sim order 21 from the 2026-07-10 houseserver run, which failed
// terminally with "cannot plan reshuffle: need 1 slot, 0 available". A lane with
// no free shuffle slot right now is CROWDED, not BROKEN — a slot frees the moment
// any other order releases one. Failing the order drops demand that only needed to
// wait, which contradicts the D18-Q4 wait-not-fail principle the simple path
// upholds everywhere else.
//
// The fix could only ever land HERE, with the scanner able to re-plan: at intake
// there was no second chance, so "no slot" had nowhere to go but a terminal fail.
// Waiting is only meaningful if something tries again.
func TestBuriedRetrieve_NoShuffleSlot_WaitsThenReshuffles(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	// Lane of exactly 2 slots, both occupied (blocker at the mouth, target behind
	// it), and exactly 1 shuffle slot. No empty lane slot anywhere, so the shuffle
	// slot is the ONLY place a blocker could go.
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix:      "NOSHUF",
		NumSlots:    2,
		NumShuffles: 1,
		TargetSlot:  2,
		TargetAge:   2 * time.Hour,
	})

	// Occupy the only shuffle slot → the reshuffle has nowhere to dig to.
	squatter := &bins.Bin{BinTypeID: sc.BinType.ID, Label: "NOSHUF-SQUAT", NodeID: &sc.ShuffleSlots[0].ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(squatter), "occupy the shuffle slot")

	eng := newTestEngine(t, db, simulator.New())
	eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:    "buried-no-shuffle-slot",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  sc.Payload.Code,
		SourceNode:   sc.Grp.Name,
		DeliveryNode: sc.LineNode.Name,
		Quantity:     1,
	})

	order := testdb.RequireOrder(t, db, "buried-no-shuffle-slot")
	if protocol.IsTerminal(order.Status) {
		t.Fatalf("order was TERMINALIZED (%q, %q) because no shuffle slot was free.\n"+
			"That is congestion, not a broken lane — a slot frees as soon as any other order "+
			"releases one. It must WAIT (D18-Q4 wait-not-fail; D79 rider, sim order 21).",
			order.Status, order.ErrorDetail)
	}
	if order.Status != dispatch.StatusQueued {
		t.Fatalf("status = %q, want %q (waiting for a shuffle slot)", order.Status, dispatch.StatusQueued)
	}

	// A shuffle slot frees — some other order cleared it.
	mustExec(t, db, `DELETE FROM bins WHERE id=$1`, squatter.ID)

	// The scanner replays. NOW the reshuffle is plannable, and the order that waited
	// gets its dig. This is the half that could not exist before the scanner could
	// plan on replay: waiting is only useful if someone tries again.
	for i := 0; i < 3; i++ {
		eng.RunFulfillmentScan()
	}

	order = testdb.RequireOrder(t, db, "buried-no-shuffle-slot")
	children, err := db.ListChildOrders(order.ID)
	testutil.MustNoErr(t, err, "list child orders")

	if order.Status != dispatch.StatusReshuffling || len(children) == 0 {
		t.Fatalf("after a shuffle slot freed: status = %q (%d children), want %q with compound children.\n"+
			"queue_reason = %q\nThe order waited correctly but never got re-planned.",
			order.Status, len(children), dispatch.StatusReshuffling, order.QueueReason)
	}
}
