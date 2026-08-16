//go:build docker

package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// lane_unification_redrive_docker_test.go — the parked order's named releaser.
//
// The unification gives plain orders a new way to WAIT (a lane another robot is
// inside). A refusal with no named releaser is a wedge, so the question this
// file answers is not "does it park" — that is pinned in the dispatch package —
// but "what un-parks it, and is that the EVENT or the 60-second sweep".
//
// It must be the event. StartPeriodicSweep runs at 60s (engine_lifecycle.go), so
// a lane that clears at t=0 would otherwise hold every waiting order for up to a
// minute — on the highest-traffic path in the system, for a lane that is already
// free.
//
// The assertion window below is seconds against that 60s backstop, so a pass is
// attributable to the subscription and not to the ticker.

// laneWithTwoSlots builds an NGRP + LANE + two depth-ordered slots, left at the
// DEFAULT enforcement mode (`none`) — what both plants run.
func laneWithTwoSlots(t *testing.T, db *store.DB, name string) (laneID int64, slots []*nodes.Node) {
	t.Helper()
	ngrpType, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	if err != nil {
		t.Fatalf("get NGRP type: %v", err)
	}
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	if err != nil {
		t.Fatalf("get LANE type: %v", err)
	}
	grp := &nodes.Node{Name: name + "-GRP", IsSynthetic: true, Enabled: true, NodeTypeID: &ngrpType.ID}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")
	lane := &nodes.Node{Name: name + "-LANE", IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID, ParentID: &grp.ID}
	testutil.MustNoErr(t, db.CreateNode(lane), "create lane")
	d0, d1 := 0, 1
	s0 := &nodes.Node{Name: name + "-S0", Enabled: true, ParentID: &lane.ID, Depth: &d0}
	testutil.MustNoErr(t, db.CreateNode(s0), "create slot 0")
	s1 := &nodes.Node{Name: name + "-S1", Enabled: true, ParentID: &lane.ID, Depth: &d1}
	testutil.MustNoErr(t, db.CreateNode(s1), "create slot 1")
	return lane.ID, []*nodes.Node{s0, s1}
}

// waitForStatus polls for a status, bounded well under the 60s periodic sweep so
// a pass cannot be the sweep's doing.
func waitForStatus(t *testing.T, db *store.DB, uuid string, want protocol.Status, within time.Duration) *orders.Order {
	t.Helper()
	deadline := time.Now().Add(within)
	var last protocol.Status
	for time.Now().Before(deadline) {
		o, err := db.GetOrderByUUID(uuid)
		if err == nil && o != nil {
			last = o.Status
			if o.Status == want {
				return o
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("order %s did not reach %s within %s (last status %s)", uuid, want, within, last)
	return nil
}

// TestUnification_LaneClearingEventRedrivesAParkedPlainOrder.
//
// A retrieve whose only source bin sits in a lane that another order is inside.
// It parks. Then the occupant goes terminal — which releases its occupancy in
// the SAME transaction as the status write (store/orders.go →
// reservations.ReleaseByOrder) — and the terminal event re-drives the scanner.
//
// MUTATION (verified): remove EventOrderCancelled from the triggerFulfillment
// subscriptions in wiring.go. The order stays in sourcing and this test times
// out — the sweep would eventually take it, 60 seconds later.
func TestUnification_LaneClearingEventRedrivesAParkedPlainOrder(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	laneID, slots := laneWithTwoSlots(t, db, "REDRIVE")

	// The ONLY bin of this payload, in the lane's mouth slot — so the finder has
	// no other source and the order's path must run through this lane.
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, slots[0].ID, "REDRIVE-BIN")

	// Somebody is inside the lane. `dispatched` keeps it out of the scanner's
	// acquiring set — an occupant that the scanner also tries to fulfil would
	// fail on its own account and muddy what this test is watching — while still
	// being terminalizable below.
	occupant := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "redrive-occupant"
		o.Status = dispatch.StatusDispatched
	})
	if err := reservations.AcquireOccupancy(db.DB, occupant.ID, laneID); err != nil {
		t.Fatalf("acquire occupancy: %v", err)
	}

	eng := newTestEngine(t, db, simulator.New())
	d := eng.Dispatcher()

	// Intake runs the scanner synchronously (EventOrderQueued → RunOnce), so by
	// the time this returns the order has already been offered its lane once.
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:    "redrive-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  sd.Payload.Code,
		DeliveryNode: sd.LineNode.Name,
		Quantity:     1,
	})

	// PRECONDITION, and the whole test rests on it: the order must genuinely be
	// parked on the lane. If it dispatched, the release below proves nothing.
	parked, err := db.GetOrderByUUID("redrive-1")
	if err != nil || parked == nil {
		t.Fatalf("reload parked order: %v", err)
	}
	if parked.Status == dispatch.StatusDispatched {
		t.Fatal("the order dispatched into a lane another order occupies — the unification's ask " +
			"is not reaching this path, and the rest of this test is vacuous")
	}
	if parked.QueueCause != string(dispatch.CauseLaneOccupied) {
		t.Fatalf("queue_cause = %q, want %q — the park must say WHY on the row, or an operator "+
			"looking at a stalled order sees a blank", parked.QueueCause, dispatch.CauseLaneOccupied)
	}

	// ── The lane clears ───────────────────────────────────────────────────
	// Terminalizing releases the occupancy row in the same transaction as the
	// status write; the event is what tells the scanner to look again.
	if _, err := db.TerminalizeOrder(occupant.ID, protocol.StatusCancelled, "test: occupant leaves"); err != nil {
		t.Fatalf("terminalize occupant: %v", err)
	}
	if occ, err := reservations.OccupantsOf(db.DB, laneID); err != nil || len(occ) != 0 {
		t.Fatalf("fixture: lane still occupied by %v (err %v) — TerminalizeOrder should have "+
			"released the row in its own transaction", occ, err)
	}
	eng.Events.Emit(Event{Type: EventOrderCancelled, Payload: OrderCancelledEvent{
		OrderID:  occupant.ID,
		EdgeUUID: occupant.EdgeUUID,
	}})

	// Seconds, against a 60s sweep: this is the event's doing.
	waitForStatus(t, db, "redrive-1", dispatch.StatusDispatched, 10*time.Second)
}
