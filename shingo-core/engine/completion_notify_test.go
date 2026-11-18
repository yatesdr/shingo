//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestRegression_OrderCompletedNotifiesEdge — Core must tell Edge when it
// completes an order, the same way it already does for cancellations
// (EventOrderCancelled) and failures (EventOrderFailed, closed by
// TestRegression_OrderFailedNotifiesEdge in engine_regression_test.go).
//
// Completion was the last terminal state with no path to Edge, and it is the
// one that could never be covered by accident: cancellation and failure are
// usually FLEET-reported, and fleet news has always flowed through
// wiring_vendor_status's OrderUpdate push. Confirmation is paperwork, not
// movement — no robot ever reports it — so when Core decides it alone, silence
// was total.
//
// Springfield 2026-08-03: 115 of 331 swap legs over 14 days were confirmed by
// Core's 5-minute stuck-delivered sweep. Every one left the Edge row at
// `delivered` until the next edge restart. Order 4017 was still `delivered` on
// the Pi 2½ hours after Core completed it — which kept a finished order inside
// ALN_001's active list, where the operator-station modal picked it up as the
// "blocker" during the NEXT changeover and displayed its stale queue_reason.
//
// Asserted on a Core-side confirm's write, not on the sweep. Core confirms
// behind Edge's back from three sites — the stuck-delivered sweep (engine.go),
// the compound-child auto-confirm (wiring_vendor_status.go), and the operator
// force-confirm button (recovery_service.go) — and all three funnel through
// ConfirmReceipt's status write, which queues the station's `confirmed` in its
// own transaction. This drives the force-confirm. It once emitted
// EventOrderCompleted alone, when the event's subscriber sent the notice; the
// write tells now, and the event tells nothing.
func TestRegression_OrderCompletedNotifiesEdge(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "regr-complete-edge-uuid"
		o.StationID = "line-1"
		o.Status = protocol.StatusDelivered
	})
	testutil.MustNoErr(t, eng.ForceConfirmDelivered(o.ID, "regr"), "force-confirm the delivered order")

	var confirmed int
	for _, u := range updatesAfter(t, db, o.EdgeUUID, 0) {
		if u.Status == string(protocol.StatusConfirmed) {
			confirmed++
		}
	}
	if confirmed != 1 {
		t.Errorf("order.update confirmed in outbox for line-1 after Core confirmed the order: %d, want 1 — "+
			"Core completed an order and told Edge nothing, so the Edge row strands at 'delivered'", confirmed)
	}
}

// TestRegression_OrderCompletedSkipsEmptyEdgeUUID — Core-internal orders
// (synthetic reshuffle parents, auto-return legs) have no Edge counterpart, so
// the write's tell must skip them rather than address an envelope to nobody
// (store queueTellTx: no edge uuid, no tell). Mirrors
// TestRegression_OrderFailedSkipsEmptyEdgeUUID. It used to emit the completion
// event by hand, when the event's subscriber sent the notice; the confirm's
// own write tells now, so the confirm is what is driven.
func TestRegression_OrderCompletedSkipsEmptyEdgeUUID(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.StationID, o.Status = "", "line-1", protocol.StatusDelivered // Core-internal order
	})
	if _, err := eng.Dispatcher().Lifecycle().ConfirmReceipt(o, "line-1", "confirmed", 0); err != nil {
		t.Fatalf("confirm the internal order: %v", err)
	}

	var count int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE msg_type = 'order.update'`).Scan(&count), "query outbox")
	if count != 0 {
		t.Errorf("outbox has %d order.update messages, want 0 — empty EdgeUUID should skip Edge notification", count)
	}
}
