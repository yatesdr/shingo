//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/reservations"
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
	d.CancelOrderWithCascade(stale, stale.StationUID, "test: changeover start", CancelCause{})

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

// TestCancelCascade_TheCarrierDrivesOutAndTheLaneReopens is docket B16's
// check: the cascade lets the lane go with a carrying leg kept, where the
// dissolve keeps it. Pinned as it is today, after a trace and a run:
//
//   - The lane goes with the parent's own end, not with the cascade's unlock:
//     the cancel's terminal write deletes the parent's rows, the dig's lane row
//     among them. The dissolve keeps the lane only because it never ends its
//     parent, and it keeps it for that parent's re-plan.
//   - The cascade's wake releases the carrier from its dwell at once: Core
//     chooses its parking, appends the tail, and the carrier's presence row on
//     the lane drops as it drives out. The carrier still lands
//     (TestCancelCascade_ALiftedLegFinishesItsStore).
//   - So another order is admitted into the lane while the carrier drives
//     out. That is the overlap every pickout already allows
//     (HandleTransitForLaneGate). Before the cancel the carrier's presence
//     row is the lane's, as for any dwelling leg.
func TestCancelCascade_TheCarrierDrivesOutAndTheLaneReopens(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent, lane, slots, legs := planDigToDissolve(t, db, d, "CC-FILE", 1)

	transit, err := db.GetNodeByDotName("_TRANSIT")
	testutil.MustNoErr(t, err, "resolve _TRANSIT")
	liftBin(t, db, d, legs[0], slots[0], transit)
	occupants, err := reservations.OccupantsOf(db.DB, lane.ID)
	testutil.MustNoErr(t, err, "read the lane's occupants before the cancel")
	if len(occupants) != 1 || occupants[0] != legs[0].ID {
		t.Fatalf("fixture: lane %s is occupied by %v before the cancel, want the carrier %d dwelling",
			lane.Name, occupants, legs[0].ID)
	}
	stale, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload the parent")
	d.CancelOrderWithCascade(stale, stale.StationUID, "test: changeover start", CancelCause{})

	carrier, err := db.GetOrder(legs[0].ID)
	testutil.MustNoErr(t, err, "reload the carrier")
	if protocol.IsTerminal(carrier.Status) {
		t.Fatalf("fixture: the carrier is %q, want it kept", carrier.Status)
	}
	if carrier.DeliveryNode == "" {
		t.Errorf("the carrier is still dwelling after the cascade (queue_cause %q); today the cascade's "+
			"wake releases it to a parking slot", carrier.QueueCause)
	}
	if d.laneLock.IsLocked(lane.ID) {
		t.Errorf("lane %s is still held after its parent's cancel; today the parent's end takes the "+
			"dig's lane row with it", lane.Name)
	}
	occupants, err = reservations.OccupantsOf(db.DB, lane.ID)
	testutil.MustNoErr(t, err, "read the lane's occupants")
	if len(occupants) != 0 {
		t.Errorf("lane %s is occupied by %v after the cascade, want nobody: the carrier is driving out "+
			"to %s", lane.Name, occupants, carrier.DeliveryNode)
	}

	entrant := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "CC-FILE-entrant"
		o.OrderType = OrderTypeMove
		o.DeliveryNode = slots[2].Name
		o.Status = protocol.StatusPending
	})
	v, err := d.admit(admissionSituation{order: entrant, destNode: slots[2]})
	testutil.MustNoErr(t, err, "admission after the cascade")
	if !v.Admitted() {
		t.Errorf("an order placing into lane %s after the cascade is refused (%s); today it is admitted "+
			"while the carrier drives out", lane.Name, v.Cause())
	}
}
