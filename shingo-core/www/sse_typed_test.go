package www

import (
	"strings"
	"testing"
	"time"

	"shingocore/engine"
)

// TestSetupEngineListeners_TypedBroadcast guards the SubscribeTyped migration in
// sse.go. Each payload-carrying listener now binds its event type to a concrete
// payload type P via eventbus.SubscribeTyped. A wrong event-type↔payload-type
// pairing still COMPILES (both are valid types) but is skipped at runtime with a
// logged "payload mismatch" — so the broadcast silently never fires. Emitting
// each real payload and requiring its SSE frame is the regression guard against
// exactly that mis-binding.
//
// The hub is wired to a bare engine carrying only an event bus: SetupEngineListeners
// just registers subscribers, and the events asserted here never reach the DB /
// robot-cache branches (status is non-"in_transit", RobotID is empty), so no
// engine.Start() or DB is needed.
func TestSetupEngineListeners_TypedBroadcast(t *testing.T) {
	eng := &engine.Engine{Events: engine.NewEventBus()}

	hub := NewEventHub()
	hub.Start()
	defer hub.Stop()
	hub.SetupEngineListeners(eng)

	ch := hub.AddClient()
	defer hub.RemoveClient(ch)

	cases := []struct {
		name      string
		typ       engine.EventType
		payload   any
		wantEvent string
		wantSub   string // substring required in frame Data ("" = match event name only)
	}{
		{"received", engine.EventOrderReceived, engine.OrderReceivedEvent{OrderID: 1}, "order-update", `"type":"received"`},
		{"dispatched", engine.EventOrderDispatched, engine.OrderDispatchedEvent{OrderID: 2, VendorOrderID: "V2"}, "order-update", `"type":"dispatched"`},
		{"status_changed", engine.EventOrderStatusChanged, engine.OrderStatusChangedEvent{OrderID: 3, NewStatus: "queued"}, "order-update", `"type":"status_changed"`},
		{"mission_event", engine.EventOrderStatusChanged, engine.OrderStatusChangedEvent{OrderID: 3, NewStatus: "queued"}, "mission-event", ""},
		{"completed", engine.EventOrderCompleted, engine.OrderCompletedEvent{OrderID: 4}, "order-update", `"type":"completed"`},
		{"failed", engine.EventOrderFailed, engine.OrderFailedEvent{OrderID: 5, Detail: "x"}, "order-update", `"type":"failed"`},
		{"cancelled", engine.EventOrderCancelled, engine.OrderCancelledEvent{OrderID: 6, Reason: "x"}, "order-update", `"type":"cancelled"`},
		{"skipped", engine.EventOrderSkipped, engine.OrderSkippedEvent{OrderID: 7, Detail: "x"}, "order-update", `"type":"skipped"`},
		{"queued", engine.EventOrderQueued, engine.OrderQueuedEvent{OrderID: 8, PayloadCode: "P8"}, "order-update", `"type":"queued"`},
		{"bin", engine.EventBinUpdated, engine.BinUpdatedEvent{NodeID: 9, Action: "added", BinID: 1}, "bin-update", ""},
		{"correction", engine.EventCorrectionApplied, engine.CorrectionAppliedEvent{NodeID: 10, CorrectionType: "cycle_count"}, "inventory-update", ""},
		{"node", engine.EventNodeUpdated, engine.NodeUpdatedEvent{NodeID: 11, Action: "updated"}, "node-update", ""},
		{"cms", engine.EventCMSTransaction, engine.CMSTransactionEvent{}, "cms-transaction", ""},
		{"robots", engine.EventRobotsUpdated, engine.RobotsUpdatedEvent{}, "robot-update", ""},
		{"cell", engine.EventCellTick, engine.CellTickEvent{Station: "S1", ProcessID: 1}, "cell-heartbeat", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			drainFrames(ch)
			eng.Events.Emit(engine.Event{Type: tc.typ, Payload: tc.payload})
			if !waitForFrame(ch, tc.wantEvent, tc.wantSub, 2*time.Second) {
				t.Fatalf("emitting event type %v produced no %q frame (data sub %q) — "+
					"SubscribeTyped most likely skipped it on a payload-type mismatch",
					tc.typ, tc.wantEvent, tc.wantSub)
			}
		})
	}
}

// drainFrames clears any frames left in the channel from a prior case so the
// next assertion only sees frames from its own emit.
func drainFrames(ch chan SSEEvent) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// waitForFrame reads frames until one matches event (and, if sub != "", whose
// Data contains sub), or the timeout elapses. Non-matching frames — e.g. the
// mission-event that also fires on a status change — are skipped.
func waitForFrame(ch chan SSEEvent, event, sub string, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		select {
		case evt := <-ch:
			if evt.Event == event && (sub == "" || strings.Contains(evt.Data, sub)) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}
