//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// dig_lock_at_rest_docker_test.go — THE DIG PARENT HOLDS ITS LANE WHILE IT WAITS.
//
// A characterisation, green today. A complex order whose supply bin is buried
// becomes the parent of its own excavation, and the reshuffle planner takes the
// lane for it at PLAN time (proposeLaneClearDig: LaneLock.TryLockFor, then
// CreateCompoundOrder) — before any robot is committed to the order. When the dig
// finishes, the demand goes back to `queued` and keeps the lane as its own
// outbound hold until it has its bin. In both it is waiting and the lane is held
// hard.
//
// That is not the hand's hold, and holds-at-rest does not count it: it is
// counted apart as dig-lock-at-rest (orders.DigLocksAtRestSQL). Moving the dig
// parent's lane into the hand is C2 ("the dig parent holds the hand, and each leg
// takes its part at handover"), and C2 flips this test: the lane is then held
// soft until the parent's own dispatch.
func TestDigParent_TakesTheLaneAtPlanTimeAndWaitsHoldingIt(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)
	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-DLR-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-DLR-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	demand := mkQueuedComplexParent(t, db, "uuid-dig-lock-at-rest", bp.Code)
	// An origin, as production always stamps: the corridor handoff at the dig's
	// end goes to the live demand in the dig's episode.
	_, err := db.Exec(`UPDATE orders SET origin_id = $1, origin_class = 'demand' WHERE id = $2`,
		"66666666-6666-6666-6666-666666666666", demand.ID)
	testutil.MustNoErr(t, err, "stamp the demand's origin")
	demand, err = db.GetOrder(demand.ID)
	testutil.MustNoErr(t, err, "reload the demand with its origin")

	d.handleComplexBuriedOnReplay(demand, &BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID})

	// (1) AT PLAN TIME: the lane is the demand's, as a dig lock, and no robot is
	// committed to the demand.
	rows, err := reservations.ActiveMouthRows(db.DB, lane.ID)
	testutil.MustNoErr(t, err, "read the lane's mouth rows after the burial")
	if len(rows) != 1 || rows[0].OrderID != demand.ID || rows[0].Mode != reservations.ModeDig ||
		rows[0].ReservedBy != reservations.ByExcavation {
		t.Fatalf("lane %s holds %+v after the burial, want one dig-mode %q row for the demand %d — "+
			"the planner takes the lane when it plans the dig", lane.Name, rows, reservations.ByExcavation, demand.ID)
	}
	parkedRaw, err := db.GetOrder(demand.ID)
	parked := testutil.Must(t, parkedRaw, err, "db.GetOrder(demand.ID)")
	if parked.VendorOrderID != "" {
		t.Fatalf("the demand has a fleet order %q — it is supposed to be waiting on its dig", parked.VendorOrderID)
	}

	// (2) AFTER ITS OWN DIG: back in the pre-dispatch set, still holding the lane.
	driveDigToCompletion(t, db, d, laneClearFor(t, db, demand))
	afterRaw, err := db.GetOrder(demand.ID)
	after := testutil.Must(t, afterRaw, err, "db.GetOrder(demand.ID) after the dig")
	// (C1: the resumed demand rests in `pending`, still holding the lane.)
	if after.Status != StatusPending {
		t.Fatalf("demand status = %q after its dig, want %q", after.Status, StatusPending)
	}
	dugRaw, err := orders.ListDigLocksAtRest(db.DB)
	dug := testutil.Must(t, dugRaw, err, "orders.ListDigLocksAtRest")
	found := false
	for _, h := range dug {
		if h.OrderID == demand.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("dig-lock-at-rest = %+v, want the demand %d: it is `queued` and keeps its lane as the "+
			"dig's handoff until it has its bin", dug, demand.ID)
	}

	// (3) THE SPLIT: the same order is not a hold-at-rest. A1's gate counts what
	// the acquire takes; this lane was taken by the planner.
	heldRaw, err := orders.ListHoldsAtRest(db.DB)
	held := testutil.Must(t, heldRaw, err, "orders.ListHoldsAtRest")
	for _, h := range held {
		if h.OrderID == demand.ID {
			t.Fatalf("holds-at-rest names the dig parent: %s — its lane is a dig lock, counted apart", h.Line())
		}
	}
}
