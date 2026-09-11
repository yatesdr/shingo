//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/internal/testdb"
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
// Driven through the real engine: the scanner's held-bin path is what reaches
// digForBuriedHeldBin, and the release goes through EvaluateWaitLaneForStagedOrder.
//
// RED before the fix: the first scan planned the dig (two legs, the lane locked)
// with the destination as its only parking. MUTATION: pass no destination to
// PlanReshuffle from planBuriedReshuffle — the same dig starts again and the
// first assertion fires.
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
		o.Status = protocol.StatusSourcing
	})
	testdb.ReserveBin(t, db, move.ID, sc.TargetBin.ID)
	testutil.MustNoErr(t, db.UpdateOrderBinID(move.ID, sc.TargetBin.ID), "stamp the held bin")

	// ── The only free parking is the order's own destination: no dig ──────
	eng.RunFulfillmentScan()
	move = testdb.RequireOrder(t, db, "hbd-move")
	legs, err := db.ListChildOrders(move.ID)
	testutil.MustNoErr(t, err, "list the dig's legs")
	digRows, err := reservations.ActiveMouthRows(db.DB, sc.Lane.ID)
	testutil.MustNoErr(t, err, "read the lane's mouth rows")
	if len(legs) != 0 || move.Status == protocol.StatusReshuffling || len(digRows) != 0 {
		t.Fatalf("the dig started with %s — the held-bin order's own destination — as its only parking: "+
			"order %s, %d leg(s), %d mouth row(s) on %s. Its retrieve will deliver there, so the release "+
			"can never put the blocker down and the lane stays locked with nothing able to free it",
			dest.Name, move.Status, len(legs), len(digRows), sc.Lane.Name)
	}
	if !protocol.IsAcquiring(move.Status) || move.QueueCause != string(dispatch.CauseNoShuffleSlot) {
		t.Errorf("the held-bin order is %q under cause %q, want an acquiring order waiting under %q — "+
			"there is no parking the dig could use", move.Status, move.QueueCause, dispatch.CauseNoShuffleSlot)
	}
	if claimed, err := db.ListBinsByClaim(move.ID); err != nil || len(claimed) != 0 {
		t.Errorf("the waiting order hard-claims %d bin(s) (err %v) — it waits on its soft holds only",
			len(claimed), err)
	}

	// ── The releaser: another slot in the group frees ─────────────────────
	testutil.MustNoErr(t, db.MoveBinClearingStaging(occupant.ID, sc.LineNode.ID, false), "free the other shuffle slot")
	eng.RunFulfillmentScan()
	move = testdb.RequireOrder(t, db, "hbd-move")
	legs, err = db.ListChildOrders(move.ID)
	testutil.MustNoErr(t, err, "list the dig's legs after a slot freed")
	if move.Status != protocol.StatusReshuffling || len(legs) < 2 {
		t.Fatalf("%s freed and the dig still did not start: order %s under %q, %d leg(s)",
			other.Name, move.Status, move.QueueCause, len(legs))
	}
	if retrieve := legs[len(legs)-1]; retrieve.DeliveryNode != dest.Name {
		t.Errorf("the dig's retrieve delivers to %q, want the order's destination %s", retrieve.DeliveryNode, dest.Name)
	}

	// ── And the blocker goes to the slot that freed ────────────────────────
	unbury := legs[0]
	eng.Dispatcher().EvaluateWaitLaneForStagedOrder(unbury.ID)
	unbury, err = db.GetOrder(unbury.ID)
	testutil.MustNoErr(t, err, "reload the dig's first leg")
	if unbury.DeliveryNode != other.Name {
		t.Fatalf("the dig's first leg was released to %q (cause %q), want the slot that freed, %s",
			unbury.DeliveryNode, unbury.QueueCause, other.Name)
	}
}
