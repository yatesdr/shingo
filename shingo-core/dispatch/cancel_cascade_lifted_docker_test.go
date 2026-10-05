//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// TestCancelCascade_ALiftedLegFinishesItsStore: a parent cancelled while one of
// its dig legs carries its bin keeps that leg. The leg lands where the dwell
// puts it, the rest of the dig is cancelled, and no order is minted to return
// the leg's bin (owner, 2026-10-03: "the robot still continues storing the
// blocker and that's it"). The changeover-start cancel of a digging order is
// this door (HandleOrderCancel).
//
// MUTATION: drop the splitLiftedLegs call in cancelCompoundChildren. Leg 1 is
// cancelled with its bin at _TRANSIT, and the first assertion fires.
func TestCancelCascade_ALiftedLegFinishesItsStore(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent, _, slots, legs := planDigToDissolve(t, db, d, "CC-LIFT", 1)

	transit, err := db.GetNodeByDotName("_TRANSIT")
	testutil.MustNoErr(t, err, "resolve _TRANSIT")
	liftBin(t, db, d, legs[0], slots[0], transit)

	stale, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload the parent")
	d.CancelOrderWithCascade(stale, stale.StationID, "test: changeover start", CancelCause{})

	after, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload the parent")
	if after.Status != protocol.StatusCancelled {
		t.Fatalf("parent is %q after its cancel, want cancelled", after.Status)
	}
	lifted, err := db.GetOrder(legs[0].ID)
	testutil.MustNoErr(t, err, "reload the lifted leg")
	if protocol.IsTerminal(lifted.Status) {
		t.Fatalf("the lifted leg %d is %q (%s): a robot is holding its bin, and the parent's cancel "+
			"must let it finish its store", lifted.ID, lifted.Status, lifted.ErrorDetail)
	}
	rest, err := db.GetOrder(legs[1].ID)
	testutil.MustNoErr(t, err, "reload leg 2")
	if rest.Status != protocol.StatusCancelled {
		t.Errorf("leg 2 is %q, want cancelled by the cascade: it had not lifted anything", rest.Status)
	}

	// THE KEPT LEG LANDS, and the re-drive its landing fires decides nothing more.
	landLeg(t, d, db, lifted)
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "the landing's re-drive")
	landed, err := db.GetOrder(lifted.ID)
	testutil.MustNoErr(t, err, "reload the landed leg")
	if landed.Status != protocol.StatusConfirmed {
		t.Errorf("the kept leg is %q after landing, want confirmed", landed.Status)
	}

	// NO RETURN IS MINTED FOR IT: nothing recovers the leg, and nothing recovers
	// the parent on the leg's bin.
	var recovers int
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT count(*) FROM orders WHERE recovers_order_id IN ($1, $2)`, lifted.ID, parent.ID).Scan(&recovers),
		"count return orders")
	if recovers != 0 {
		t.Errorf("%d order(s) recover the kept leg or its parent, want none", recovers)
	}
}
