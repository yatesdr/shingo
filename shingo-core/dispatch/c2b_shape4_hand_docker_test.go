//go:build docker

package dispatch

import (
	"errors"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// TestC2bPin_ADigWhoseGroupNowOwesMoreIsRefusedInItsHand is Shape 4 re-asked in
// the dig's hand (SYNTH Shape 4, C2.1; C2b A.2). The plan-time verdict passed a
// dig against what running digs owed when it was planned. Another dig starts in
// the same group before this one is born, and its leg is owed a parking slot the
// first verdict counted as free. The birth transaction reads the facts again and
// refuses: nothing is written and nothing is held, and the refusal is the
// verdict's own (no free shuffle slot), so the requester waits as it would have
// at plan time.
//
// MUTATION: drop the plan.DigStillFits call from digLaneTake.inTx; the dig is
// born on a count already spent.
func TestC2bPin_ADigWhoseGroupNowOwesMoreIsRefusedInItsHand(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	wall, park, w, _, bp := clearLaneFixture(t, db, "SHAPE4")
	createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-SHAPE4-BLK")
	createTestBinAtNode(t, db, bp.Code, w[1].ID, "BIN-SHAPE4-TGT")

	requester := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "shape4-requester"
		o.Coordinated = true
	})
	planned, err := PlanLaneMouthClear(d.db, w[1], wall, *wall.ParentID, reservations.Anyone)
	testutil.MustNoErr(t, err, "the dig fits at plan time")
	if planned.Fit == nil || planned.Fit.Facts.Owed != 0 {
		t.Fatalf("fixture: the plan carries %+v, want its verdict's facts with nothing owed", planned.Fit)
	}

	// Another dig starts in the group: it holds the park lane and its leg is owed
	// a slot.
	other := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "shape4-other-dig" })
	if !d.laneLock.TryLock(park.ID, other.ID) {
		t.Fatal("fixture: the other dig could not take its lane")
	}
	testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "shape4-other-leg"
		o.ParentOrderID = &other.ID
	})

	err = d.createCompoundOrder(requester, planned, nil,
		&digLaneTake{laneID: wall.ID, asker: digAskerFor(requester), staged: stagedAtMarkOnLane(d.db, wall.ID)})
	if !errors.Is(err, ErrNoShuffleSlot) {
		t.Fatalf("the dig's birth returned %v, want a no-shuffle-slot refusal: the group owes more than "+
			"its verdict counted", err)
	}
	legs, err := db.ListChildOrders(requester.ID)
	testutil.MustNoErr(t, err, "list the requester's legs")
	if len(legs) != 0 {
		t.Fatalf("the refused dig wrote %d leg(s)", len(legs))
	}
	rows, err := reservations.ActiveMouthRows(db.DB, wall.ID)
	testutil.MustNoErr(t, err, "read the wall's mouth rows")
	if len(rows) != 0 {
		t.Fatalf("the refused dig holds %+v on %s", rows, wall.Name)
	}
}
