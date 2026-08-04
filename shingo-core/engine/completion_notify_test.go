//go:build docker

package engine

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
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
// Asserted on the EVENT, not on the sweep. Core confirms behind Edge's back
// from three sites — the stuck-delivered sweep (engine.go), the compound-child
// auto-confirm (wiring_vendor_status.go), and the operator force-confirm button
// (recovery_service.go) — and all three funnel through EventOrderCompleted.
// Pinning the sweep would leave the other two free to regress.
func TestRegression_OrderCompletedNotifiesEdge(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	eng.Events.Emit(Event{Type: EventOrderCompleted, Payload: OrderCompletedEvent{
		OrderID:   4017,
		EdgeUUID:  "regr-complete-edge-uuid",
		StationID: "line-1",
	}})

	var count int
	testutil.MustNoErr(t, db.QueryRow(`
		SELECT COUNT(*) FROM outbox
		WHERE msg_type = 'order.update' AND station_id = $1`, "line-1").Scan(&count), "query outbox")
	if count == 0 {
		t.Errorf("expected order.update in outbox for line-1 after EventOrderCompleted; got 0 rows — " +
			"Core completed an order and told Edge nothing, so the Edge row strands at 'delivered'")
	}
}

// TestRegression_OrderCompletedSkipsEmptyEdgeUUID — Core-internal orders
// (synthetic reshuffle parents, auto-return legs) have no Edge counterpart, so
// the notification gate must skip them rather than address an envelope to
// nobody. Mirrors TestRegression_OrderFailedSkipsEmptyEdgeUUID.
func TestRegression_OrderCompletedSkipsEmptyEdgeUUID(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	eng.Events.Emit(Event{Type: EventOrderCompleted, Payload: OrderCompletedEvent{
		OrderID:   99,
		EdgeUUID:  "", // Core-internal order
		StationID: "line-1",
	}})

	var count int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE msg_type = 'order.update'`).Scan(&count), "query outbox")
	if count != 0 {
		t.Errorf("outbox has %d order.update messages, want 0 — empty EdgeUUID should skip Edge notification", count)
	}
}
