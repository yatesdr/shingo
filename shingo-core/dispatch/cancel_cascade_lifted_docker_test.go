//go:build docker

package dispatch

import (
	"errors"
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/payloads"
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

// TestCancelCascade_ADigCannotTakeTheParkingItsCarrierWaitsFor is B16's
// residual (docket R86), with one parking spot in the group. The interleaving,
// constructed by reading in brief B:
//
//  1. the carrier's dwell release is refused at the cascade's wake for lack of
//     parking (here a bin stands on the one spot: the group closed while the
//     robot worked), so it stays in its dwell with only its presence row, the
//     parent's end having taken the lane's mouth row;
//  2. the bin leaves the parking spot;
//  3. before the carrier is asked again, a dig for a bin deeper in the lane is
//     planned (the planner asks the lane's lock, not who stands in it) and
//     pencils the only parking;
//  4. the carrier is refused for want of that parking and the dig's first leg
//     lane-occupied by the carrier: each waits on the other. (The construction
//     named the carrier's cause dig-holds-parking; under the mutation below it
//     reads no-shuffle-slot.)
//
// Pinned as it is today: step 3 does not happen. The carrier's parking pencil,
// written at its dig's birth, is the leg's own row, so the parent's cancel
// leaves it; and a sent leg's pencil is no stranger's to take
// (nodes.SlotTakeableSQL; TakeableSlotsInGroup offers a take only against a
// holder still awaiting the fleet). So the second dig waits for a shuffle slot,
// takes no lane and pencils nothing, and the carrier is released onto its
// parking at the group's next pass. The second demand outranks the first, so
// the pencil alone keeps it out: without it a ranked take would not hand the
// parking back to the carrier.
//
// MUTATION: release the kept leg's pending slots in cancelCompoundChildren.
// The dig plans and pencils the parking, its first leg waits lane-occupied
// behind the carrier, and the carrier waits no-shuffle-slot: the residual.
func TestCancelCascade_ADigCannotTakeTheParkingItsCarrierWaitsFor(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, lane, _, park, slots, _, bp := setupDwellGroup(t, db, "CC-PARK", 0, true)
	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "CC-PARK-BLK")
	createTestBinAtNode(t, db, bp.Code, slots[1].ID, "CC-PARK-TGT")
	deepPayload := &payloads.Payload{Code: "CC-PARK-P2"}
	testutil.MustNoErr(t, db.CreatePayload(deepPayload), "create the deeper bin's payload")
	deep := createTestBinAtNode(t, db, deepPayload.Code, slots[2].ID, "CC-PARK-DEEP")
	line := lineNode(t, db, "CC-PARK-LINE")

	parent := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "CC-PARK-parent"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = line.Name
		o.Status = protocol.StatusPending
	})
	legs := planDigFor(t, db, d, bp, lane, slots[1], parent)
	if len(legs) != 2 || legs[0].VendorOrderID == "" {
		t.Fatalf("fixture: the dig planned %d leg(s), leg 1 sent %q; want an unbury, sent, and a retrieve",
			len(legs), legs[0].VendorOrderID)
	}
	carrierID := legs[0].ID
	if holder := c2cSlotHolder(t, db, park); holder != carrierID {
		t.Fatalf("fixture: the one parking spot %s is pencilled by order %d, want the unbury leg %d",
			park.Name, holder, carrierID)
	}

	// Step 1: the group closes while the robot works, the blocker lifts, and
	// neither the arrival's release nor the cascade's wake can place it.
	closer := createTestBinAtNode(t, db, bp.Code, park.ID, "CC-PARK-CLOSER")
	transit, err := db.GetNodeByDotName("_TRANSIT")
	testutil.MustNoErr(t, err, "resolve _TRANSIT")
	liftBin(t, db, d, legs[0], slots[0], transit)
	d.EvaluateWaitLaneForStagedOrder(carrierID)
	stale, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload the parent")
	d.CancelOrderWithCascade(stale, stale.StationUID, "test: changeover start", CancelCause{})

	carrier, err := db.GetOrder(carrierID)
	testutil.MustNoErr(t, err, "reload the carrier")
	if protocol.IsTerminal(carrier.Status) || carrier.DeliveryNode != "" {
		t.Fatalf("fixture: the carrier is %q aimed at %q after the cascade (cause %q); want it kept and "+
			"still dwelling, refused for lack of parking", carrier.Status, carrier.DeliveryNode, carrier.QueueCause)
	}
	t.Logf("step 1: the carrier %d dwells after the cascade's wake, cause %q", carrierID, carrier.QueueCause)
	if d.laneLock.IsLocked(lane.ID) {
		t.Fatalf("fixture: lane %s is still held after its parent's cancel", lane.Name)
	}
	occupants, err := reservations.OccupantsOf(db.DB, lane.ID)
	testutil.MustNoErr(t, err, "read the lane's occupants")
	if len(occupants) != 1 || occupants[0] != carrierID {
		t.Fatalf("fixture: lane %s is occupied by %v, want the carrier %d alone", lane.Name, occupants, carrierID)
	}

	// Step 2, and step 3 before the carrier is asked again. The second demand
	// outranks the first (priority first, DemandRank), so a ranked take cannot
	// hand the parking back to the carrier: only its pencil keeps the dig out.
	testutil.MustNoErr(t, db.MoveBinClearingStaging(closer.ID, line.ID, false), "clear the parking spot")
	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "CC-PARK-deep"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = deepPayload.Code
		o.DeliveryNode = line.Name
		o.Status = protocol.StatusPending
		o.Priority = 1
	})
	planErr := d.PlanBuriedReshuffle(demand, &BuriedError{Bin: deep, Slot: slots[2], LaneID: lane.ID})
	var wait *ReshuffleWaitError
	if planErr == nil {
		// The residual: the dig took the lane and the parking. Say what each
		// side is waiting on, then fail.
		holder := c2cSlotHolder(t, db, park)
		for _, laneID := range d.DwellerLanesSharingGroupWith(park.ID) {
			d.EvaluateLaneReleases(laneID)
		}
		after, err := db.GetOrder(carrierID)
		testutil.MustNoErr(t, err, "reload the carrier")
		var digLegs []string
		for _, l := range legsOf(t, db, demand.ID) {
			digLegs = append(digLegs, fmt.Sprintf("leg %d %s cause %q", l.ID, l.Status, l.QueueCause))
		}
		t.Fatalf("B16's residual: a dig for %s was planned into lane %s while the carrier %d dwelt there, "+
			"and the parking %s was pencilled by order %d. The carrier, woken, is aimed at %q with cause %q; "+
			"the dig's legs: %v", deep.Label, lane.Name, carrierID, park.Name, holder,
			after.DeliveryNode, after.QueueCause, digLegs)
	}
	if !errors.As(planErr, &wait) {
		t.Fatalf("the dig for %s failed outright (%v); want it to wait for a shuffle slot", deep.Label, planErr)
	}
	t.Logf("step 3: the dig for %s waits, cause %q (%s)", deep.Label, wait.Cause, wait.Detail)
	if d.laneLock.IsLocked(lane.ID) {
		t.Errorf("the waiting dig holds lane %s: a dig that cannot park its blocker should take nothing", lane.Name)
	}
	if holder := c2cSlotHolder(t, db, park); holder != carrierID {
		t.Errorf("the parking %s is pencilled by order %d after the second dig's plan, want the carrier %d",
			park.Name, holder, carrierID)
	}

	// The group's next pass, as a bin leaving a slot wakes it: the carrier goes.
	for _, laneID := range d.DwellerLanesSharingGroupWith(park.ID) {
		d.EvaluateLaneReleases(laneID)
	}
	freed, err := db.GetOrder(carrierID)
	testutil.MustNoErr(t, err, "reload the carrier after the group's pass")
	if freed.DeliveryNode != park.Name {
		t.Fatalf("the carrier %d is aimed at %q (cause %q) after the parking cleared, want %s",
			carrierID, freed.DeliveryNode, freed.QueueCause, park.Name)
	}
	occupants, err = reservations.OccupantsOf(db.DB, lane.ID)
	testutil.MustNoErr(t, err, "read the lane's occupants after the release")
	if len(occupants) != 0 {
		t.Errorf("lane %s is occupied by %v after the carrier was released, want nobody", lane.Name, occupants)
	}
}
