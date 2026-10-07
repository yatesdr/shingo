//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// dig_lock_at_rest_docker_test.go — THE DIG PARENT HOLDS ITS LANE WHILE IT
// DIGS, AND NOTHING WHILE IT WAITS.
//
// A complex order whose supply bin is buried becomes the parent of its own
// excavation. The lane is the parent's while the dig runs (`reshuffling`: legs
// are moving under it). When the dig finishes, the demand goes back to `pending`
// and waits to be dispatched like any other order.
//
// VERIFY-RED for C2b (A.1): today the dig-end handoff converts the dig's row into
// an outbound `dighandoff` row for the resumed demand, so it waits holding the
// lane hard: a dig lock at rest (orders.DigLocksAtRestSQL). Under v4 §2.6 a wait
// keeps soft holds only; the demand's aim at its bin (bin_id + source_node) is
// the soft hold the store selector's burial clause already protects, and the
// demand takes the lane again in its own hand when it dispatches. When the fix
// lands, replace the VerifyRed call with the assertion spelled out beside it.
func TestDigParent_TakesTheLaneInItsHandAndRestsHoldingNothing(t *testing.T) {
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
	// (C1: the resumed demand rests in `pending`.)
	if after.Status != StatusPending {
		t.Fatalf("demand status = %q after its dig, want %q", after.Status, StatusPending)
	}
	dugRaw, err := orders.ListDigLocksAtRest(db.DB)
	dug := testutil.Must(t, dugRaw, err, "orders.ListDigLocksAtRest")
	var atRest []string
	for _, h := range dug {
		atRest = append(atRest, h.Line())
	}
	mouths, err := reservations.ActiveMouthRows(db.DB, lane.ID)
	testutil.MustNoErr(t, err, "read the lane's mouth rows after the dig")
	for _, m := range mouths {
		if m.OrderID == demand.ID {
			atRest = append(atRest, "the resumed demand holds a "+string(m.Mode)+" mouth row on its dug lane, tagged "+m.ReservedBy)
		}
	}
	if len(atRest) != 0 {
		t.Fatalf("the demand waits holding its dug lane: %v", atRest)
	}
	// WHAT PROTECTS THE BIN IT DUG FOR instead (C2b A.1b): its own pending row on
	// it, taken at the dig's birth, which both burial guards read.
	var soft int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM reservations
		WHERE order_id = $1 AND resource_kind = 'bin' AND bin_id = $2 AND state = 'pending'`,
		demand.ID, target.ID).Scan(&soft), "count the demand's soft hold on its target")
	if soft != 1 {
		t.Fatalf("the demand holds %d pending rows on bin %d after its dig, want 1: the soft hold its "+
			"dig took at birth is what keeps the slots in front of that bin closed", soft, target.ID)
	}

	// THE SPLIT STAYS: the same order is not a hold-at-rest today, because its
	// lane is a dig lock and is counted apart. After A.1 there is nothing to split.
	testdb.AssertNoHoldsAtRest(t, db, "after the demand's own dig")
}
