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

// TestC2bPin_ADigWhoseParkingWasTakenIsRefusedInItsHand is Shape 4 in the
// dig's hand (SYNTH Shape 4, C2.1; C2b A.2), as the dig-parking ruling of
// 2026-10-09 re-made it. The plan's walk found a parking slot for the blocker.
// Another order reserves that slot before the dig is born. The birth's pencil on
// it refuses: nothing is written and nothing is held, and the refusal is the
// verdict's own (no free shuffle slot), so the requester waits as it would have
// at plan time. It used to re-ask an owed count of running digs' legs; the
// parking is held on paper now, and the paper is what refuses.
//
// MUTATION: make pencilParkingTx ignore ErrReservationConflict; the dig is born
// on a slot another order holds.
func TestC2bPin_ADigWhoseParkingWasTakenIsRefusedInItsHand(t *testing.T) {
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
	if planned.Fit == nil || len(planned.Steps) == 0 || planned.Steps[0].Park == nil {
		t.Fatalf("fixture: the plan carries %+v, want its blocker's parking", planned.Steps)
	}
	_ = park

	// Another order reserves the slot the walk found for the blocker.
	other := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "shape4-other" })
	testutil.MustNoErr(t, reservations.AcquireSlot(db.DB, other.ID, planned.Steps[0].Park.ID, "reserveSlot"),
		"the other order reserves the dig's parking")

	err = d.createCompoundOrder(requester, planned, nil,
		&digLaneTake{laneID: wall.ID, asker: digAskerFor(requester), staged: stagedAtMarkOnLane(d.db, wall.ID)})
	if !errors.Is(err, ErrNoShuffleSlot) {
		t.Fatalf("the dig's birth returned %v, want a no-shuffle-slot refusal: its parking was taken "+
			"since its walk", err)
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
