//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// TestHeldBinDig_NeverParksOnTheOrdersOwnDestination pins that a dig does not
// count the destination of the order it digs for as parking.
//
// A plain reshuffle's plan ends with a retrieve that delivers to the requesting
// order's own destination: compound.go backfills the retrieve child's delivery
// node from the parent, and the child claims the target bin when it is created.
// So at release time that destination reads as inbound to the dig's own
// retrieve, and the release-time resolver (dwellDestination) never offers it.
// The plan-time count has to agree with that, or a dig starts that can never
// put its blocker down.
//
// It did not agree for a soft-held order. A held-bin order holds its bin and its
// destination as pending reservations and claims nothing, so the dropoff count
// (orders.InFlightForDropoffSQL, holders only) reads its destination as free. When
// that destination was the only free slot in the group, the dig was planned
// against it, locked the lane, sent its first leg, and then refused that leg's
// release under no-shuffle-slot on every pass: the one candidate was the order's
// own delivery. planUnbury now excludes the destination PlanReshuffle's retrieve
// will deliver to.
//
// Driven through the real engine. Every lane is marked, so the held-bin order is
// sent to the lane's mark and the dig is the gate's own (proposeLaneClearDig),
// not the held-bin path's PlanReshuffle: what keeps the destination out of its
// parking is the slot rule every chooser reads (a slot a live order names as
// its delivery is not free, nodes.SlotTakeableSQL), since the dig's order is the
// one that names it. The release goes through the lift and then
// EvaluateWaitLaneForStagedOrder.
//
// MUTATION (not yet run): drop the delivery_node clause from slotTakeable. The
// destination is offered as parking and the first assertion fires.
func TestHeldBinDig_NeverParksOnTheOrdersOwnDestination(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	// GRP-HBD: a two-slot lane (S1 a blocker, S2 the held bin) and two shuffle
	// slots under the group. SHUF1 is the held-bin order's destination; SHUF2 is
	// occupied, so the destination is the only FREE parking in the group.
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{Prefix: "HBD", NumSlots: 2, NumShuffles: 2})
	dest, other := sc.ShuffleSlots[0], sc.ShuffleSlots[1]
	occupant := testdb.CreateBinAtNode(t, db, sc.Payload.Code, other.ID, "HBD-OCCUPANT")
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	move := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "hbd-move"
		o.OrderType = dispatch.OrderTypeMove
		o.SourceIntent = dispatch.SourceIntentLocal
		o.PayloadCode = sc.Payload.Code
		o.SourceNode = sc.Slots[1].Name
		o.DeliveryNode = dest.Name
		o.Status = protocol.StatusPending // (C1: born pending; was sourcing)
	})
	testdb.ReserveBin(t, db, move.ID, sc.TargetBin.ID)
	testutil.MustNoErr(t, db.UpdateOrderBinID(move.ID, sc.TargetBin.ID), "stamp the held bin")

	// ── The only free parking is the order's own destination: no dig ──────
	// Every lane is marked: the move is sent to the lane's mark, and the dig is
	// the gate's own (the order digs its lane as its own children, staying at
	// the mark), asked when the lane is evaluated.
	eng.RunFulfillmentScan()
	eng.Dispatcher().EvaluateLaneReleases(sc.Lane.ID)
	move = testdb.RequireOrder(t, db, "hbd-move")
	legs, err := db.ListChildOrders(move.ID)
	testutil.MustNoErr(t, err, "list the dig's legs")
	if len(legs) != 0 || excavating(t, db, sc.Lane.ID) {
		t.Fatalf("the dig started with %s — the held-bin order's own destination — as its only parking: "+
			"%d leg(s), excavation on %s %v. Its retrieve will deliver there, so the release can never put "+
			"the blocker down and the lane stays locked with nothing able to free it",
			dest.Name, len(legs), sc.Lane.Name, excavating(t, db, sc.Lane.ID))
	}
	if !dispatch.IsGateStaged(move) || move.QueueCause == "" {
		t.Errorf("the held-bin order is %q under cause %q (gate-staged %v), want it held at the lane's "+
			"mark under a named cause", move.Status, move.QueueCause, dispatch.IsGateStaged(move))
	}

	// ── The releaser: another slot in the group frees ─────────────────────
	testutil.MustNoErr(t, db.MoveBinClearingStaging(occupant.ID, sc.LineNode.ID, false), "free the other shuffle slot")
	eng.Dispatcher().EvaluateLaneReleases(sc.Lane.ID)
	legs, err = db.ListChildOrders(move.ID)
	testutil.MustNoErr(t, err, "list the dig's legs after a slot freed")
	if len(legs) == 0 {
		move = testdb.RequireOrder(t, db, "hbd-move")
		t.Fatalf("%s freed and the dig still did not start: order %s under %q", other.Name, move.Status, move.QueueCause)
	}

	// ── And the blocker goes to the slot that freed ────────────────────────
	// The robot lifts the blocker and then parks on its wait, as at the plant.
	unbury := legs[0]
	eng.handlePickupBlockCompleted(BlockCompletedEvent{
		OrderID: unbury.ID, BlockID: "hbd-lift", Location: unbury.SourceNode, BinTask: "JackLoad",
	})
	eng.Dispatcher().EvaluateWaitLaneForStagedOrder(unbury.ID)
	unbury, err = db.GetOrder(unbury.ID)
	testutil.MustNoErr(t, err, "reload the dig's first leg")
	if unbury.DeliveryNode != other.Name {
		t.Fatalf("the dig's first leg was released to %q (cause %q), want the slot that freed, %s",
			unbury.DeliveryNode, unbury.QueueCause, other.Name)
	}
}

// excavating reports whether a dig (an excavation, not an order's source lock)
// holds laneID.
func excavating(t *testing.T, db *store.DB, laneID int64) bool {
	t.Helper()
	rows, err := reservations.ActiveMouthRows(db.DB, laneID)
	testutil.MustNoErr(t, err, "read the lane's mouth rows")
	for _, r := range rows {
		if reservations.IsExcavation(r.ReservedBy) {
			return true
		}
	}
	return false
}
