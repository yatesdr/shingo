//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestAdvanceCompoundOrder_CancelledChildRequeuesParent INVERTS
// TestAdvanceCompoundOrder_CancelledChildFailsParent, which pinned the
// 2026-07-09 decision in these words: "a compound (reshuffle) whose children are
// ALL terminal but include a CANCELLED child fails the parent — it does NOT take
// the success (resume/complete) branch. A cancelled reshuffle leg means the
// housekeeping didn't complete, so a plain parent must not be marked Confirmed
// with its retrieve never run (and a coordinated parent must not resume against
// a still-buried bin)." Its assertion was `want StatusFailed`.
//
// Everything that decision says about the two alternatives it weighed is still
// true. Resume would re-run the pickup against a lane still walled; complete
// would claim a retrieve that never happened. Gate 1 (§R.91) supplies the third
// option neither of them had: RE-QUEUE. The demand goes back to the acquiring
// set, the scanner re-plans against the lane as it now stands, and the
// still-buried bin the rationale worried about is exactly what the new plan
// digs out. A dig failing is congestion; only the demand's own work failing
// fails the demand.
//
// Inverted in place rather than deleted and rewritten: the assertion that
// changed sign is the record of what changed.
//
// MUTATION (verified): restore the Fail arm in AdvanceCompoundOrder — this
// reports the parent failed.
func TestAdvanceCompoundOrder_CancelledChildRequeuesParent(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	parent := &orders.Order{
		EdgeUUID:     "parent-cancel-leg",
		StationID:    "line-1",
		OrderType:    OrderTypeRetrieve, // plain-retrieve compound
		Status:       StatusReshuffling,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent")

	// One completed child + one cancelled child — all terminal, one cancelled.
	doneChild := &orders.Order{
		EdgeUUID: "child-done", StationID: parent.StationID, OrderType: OrderTypeMove,
		Status: StatusConfirmed, ParentOrderID: &parent.ID, Sequence: 1,
		SourceNode: lineNode.Name, DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(doneChild), "create done child")
	cancelledChild := &orders.Order{
		EdgeUUID: "child-cancelled", StationID: parent.StationID, OrderType: OrderTypeMove,
		Status: StatusCancelled, ParentOrderID: &parent.ID, Sequence: 2,
		SourceNode: lineNode.Name, DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(cancelledChild), "create cancelled child")

	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "AdvanceCompoundOrder")

	got, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload parent")
	if got.Status != StatusQueued {
		t.Fatalf("parent status = %q, want %q. A leg stopping is congestion, not the demand's own work "+
			"failing: the chapter closes and the demand goes back to be re-planned against the lane as "+
			"it now stands. %q would take a live demand out of the plant because a robot broke down",
			got.Status, StatusQueued, StatusFailed)
	}
}

// TestAdvanceCompoundOrder_AllChildrenConfirmed_Completes is the positive twin: a
// plain-retrieve compound whose children all CONFIRMED cleanly completes the parent
// (Reshuffling → Confirmed). Guards against the cancelled-fails-parent change
// over-firing on a clean compound.
func TestAdvanceCompoundOrder_AllChildrenConfirmed_Completes(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	parent := &orders.Order{
		EdgeUUID:     "parent-clean",
		StationID:    "line-1",
		OrderType:    OrderTypeRetrieve,
		Status:       StatusReshuffling,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent")
	child := &orders.Order{
		EdgeUUID: "child-clean", StationID: parent.StationID, OrderType: OrderTypeMove,
		Status: StatusConfirmed, ParentOrderID: &parent.ID, Sequence: 1,
		SourceNode: lineNode.Name, DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(child), "create child")

	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "AdvanceCompoundOrder")

	got, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload parent")
	if got.Status != StatusConfirmed {
		t.Fatalf("parent status = %q, want %q (a clean all-confirmed plain compound completes)", got.Status, StatusConfirmed)
	}
}

// TestGate1_TheDemandSurvivesItsDigsFailure is gate 1 end to end, on the shape
// the owner asked about: "the demand still exists — isn't this the point of
// heal?"
//
// ── WHAT USED TO HAPPEN ───────────────────────────────────────────────────
//
// A dig leg failed — a robot stopped responding, a fleet fault, the abandon
// sweep — and the failure travelled UP. HandleChildOrderFailure cancelled the
// siblings and failed the parent, and the parent IS the demand: on the plain
// path planBuriedReshuffle re-parents the retrieve onto its own excavation, so
// one broken robot in a corridor took a live demand out of the plant.
//
// The 2026-07-09 decision that put it there is quoted at ReshuffleLegFailedDetail
// and at the test above. It weighed FAIL against RESUME and against COMPLETE and
// was right about both of those; the option it did not have was RE-QUEUE.
//
// ── AND THE PARTNER, WHICH IS THE HALF NOBODY WAS LOOKING AT ──────────────
//
// A two-robot swap leg unwinds its sibling when it goes terminal. So a dig leg
// breaking down failed the demand, and the demand's failure reached across and
// took the other robot's order with it — two orders lost to one robot, on a
// congestion event. Nothing reaches across now, because nothing terminal
// happens: the assertion is here rather than in a comment because "we no longer
// do X" is only true while nothing else starts doing it.
//
// MUTATION (verified): revert gate 1 whole — the failure veto back into
// digWasDissolved, endsItsChapter back to marker-only, and the Fail arm back in
// the disposition. Assertion (a) fires: the demand is "failed".
//
// ASSERTION (c) DID NOT FIRE UNDER THAT MUTATION, and saying so is the point of
// writing mutations down. The unwind runs through HandleSwapPeerTerminal, which
// is engine wiring, and there is none under a dispatch unit test — so this test
// cannot reproduce the partner cascade and does not claim to. (c) is a GUARD
// that nothing in this path starts reaching across, not a reproduction of the
// old one. The cascade itself is engine-level and is not pinned here.
func TestGate1_TheDemandSurvivesItsDigsFailure(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, emitter := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// THE DEMAND, wearing `reshuffling` because it re-parented onto its own dig.
	demand := &orders.Order{
		EdgeUUID: "g1-demand", StationID: "line-1", OrderType: OrderTypeRetrieve,
		Status: StatusReshuffling, PayloadCode: bp.Code, DeliveryNode: lineNode.Name, Quantity: 1,
	}
	testutil.MustNoErr(t, db.CreateOrder(demand), "create the demand")

	// ITS PARTNER: a two-robot swap peer, pointed at the demand and untouched by
	// anything this test does directly.
	partner := &orders.Order{
		EdgeUUID: "g1-partner", StationID: "line-1", OrderType: OrderTypeRetrieve,
		Status: StatusQueued, PayloadCode: bp.Code, DeliveryNode: lineNode.Name, Quantity: 1,
		SiblingOrderUUID: demand.EdgeUUID,
	}
	testutil.MustNoErr(t, db.CreateOrder(partner), "create the partner")

	// A leg that died of something a re-plan can fix.
	brokeDown := &orders.Order{
		EdgeUUID: "g1-leg-dead", StationID: demand.StationID, OrderType: OrderTypeMove,
		Status: StatusFailed, ParentOrderID: &demand.ID, Sequence: 1,
		SourceNode: lineNode.Name, DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(brokeDown), "create the dead leg")

	d.HandleChildOrderFailure(demand.ID, brokeDown.ID)
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(demand.ID), "dispose of the closed chapter")

	// (a) THE DEMAND IS BACK IN THE ACQUIRING SET, not dead.
	got, err := db.GetOrder(demand.ID)
	testutil.MustNoErr(t, err, "reload the demand")
	if got.Status != StatusQueued {
		t.Fatalf("the demand is %q after its dig lost a leg, want %q. A dig failing is the plant "+
			"being congested; only the demand's OWN work failing fails the demand", got.Status, StatusQueued)
	}

	// (b) AND IT IS TOLD WHY, in words that match what happened. Telling a demand
	// its plan "went stale" when a robot broke down puts the wrong sentence in
	// front of the operator reading the board.
	if got.ErrorDetail == ReshuffleDissolveDetail {
		t.Errorf("the demand carries the STALE-PLAN wording after a leg failure: %q", got.ErrorDetail)
	}

	// (c) THE PARTNER IS UNTOUCHED. It unwinds when its sibling goes terminal, and
	// the whole point is that its sibling did not.
	partnerGot, err := db.GetOrder(partner.ID)
	testutil.MustNoErr(t, err, "reload the partner")
	if partnerGot.Status != StatusQueued {
		t.Errorf("the partner is %q, want %q — one robot breaking down in a corridor must not take "+
			"the other robot's order with it", partnerGot.Status, StatusQueued)
	}

	// (d) NOTHING ANNOUNCED A FAILURE.
	for _, f := range emitter.failed {
		if f.orderID == demand.ID || f.orderID == partner.ID {
			t.Errorf("order %d announced a failure it did not have", f.orderID)
		}
	}
}

// TestGate1_AChapterWhoseLastLegFailedDoesNotHauntTheNextOne is the accounting
// half of gate 1, and it is the half that is easy to leave out.
//
// ── THE SHAPE ─────────────────────────────────────────────────────────────
//
// A chapter closes and the parent re-plans, so the compound now has children
// from two generations and ListChildOrders returns all of them. compoundGenerations
// splits them on a BOUNDARY: terminal children at or below the newest child that
// ended a chapter are a closed book.
//
// The boundary used to be "cancelled under the dissolve marker", and that was
// complete, because a FAILED leg killed the demand — there was never a next
// generation for it to haunt. Gate 1 makes the demand survive, and a chapter
// whose LAST leg failed leaves nothing for the dissolve to cancel: no marker is
// written, no boundary exists, and the dead leg is counted in every generation
// that follows. The parent re-queues, re-plans, is dissolved again on the
// strength of a failure two digs old, and never finishes.
//
// So a failed leg has to BE a boundary, not merely be forgiven by one.
//
// MUTATION (verified): drop the StatusFailed arm from endsItsChapter — the
// second chapter's clean completion is read as another chapter ending and the
// demand goes round again instead of confirming.
func TestGate1_AChapterWhoseLastLegFailedDoesNotHauntTheNextOne(t *testing.T) {
	t.Parallel()

	// Chapter 1: one leg, and it died. Nothing was left to cancel behind it.
	dead := &orders.Order{ID: 10, Status: StatusFailed, ErrorDetail: "the robot stopped responding"}
	// Chapter 2: planned after the re-queue, and it finished cleanly.
	fresh := &orders.Order{ID: 11, Status: StatusConfirmed}

	open, superseded := compoundGenerations([]*orders.Order{dead, fresh})
	if !superseded {
		t.Fatal("a chapter that ended in a failed leg left no boundary, so nothing is ever closed. " +
			"The dead leg is now part of every generation that follows and the demand cannot finish")
	}
	if len(open) != 1 || open[0].ID != fresh.ID {
		t.Fatalf("open chapter = %v, want just the second chapter's leg %d — the first chapter's "+
			"dead leg is a closed book and must not be counted against the dig now running",
			idsOf(open), fresh.ID)
	}
	if digWasDissolved([]*orders.Order{dead, fresh}) {
		t.Error("the second chapter finished cleanly and still read as a chapter that stopped short. " +
			"That is the loop: re-queue, re-plan, dissolve, forever")
	}
}

// idsOf renders an order slice as its ids, for a failure message that has to say
// WHICH children were in the open set.
func idsOf(os []*orders.Order) []int64 {
	out := make([]int64, 0, len(os))
	for _, o := range os {
		out = append(out, o.ID)
	}
	return out
}
