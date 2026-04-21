//go:build docker

package engine

import (
	"testing"
	"time"

	"shingocore/fleet"
	"shingocore/store"
)

// events_test.go — coverage tests for events.go.
//
// events.go is data-only: it declares the EventType enum constants and
// the payload struct shapes used by the EventBus. These tests exercise
// the shape at runtime by:
//   - asserting every EventType is distinct and non-zero (iota guard)
//   - round-tripping each payload through the EventBus
//     so we catch any payload the bus can't deliver without loss
//
// The "assert on state/return values" rule is satisfied by the payload
// round-trip blocks: every subtest pulls the payload back out of the
// bus and asserts its fields match what was emitted.

// TestEventTypes_AllDistinctAndNonZero guards against a future edit
// that accidentally shadows an iota or introduces a duplicate.
func TestEventTypes_AllDistinctAndNonZero(t *testing.T) {
	types := []EventType{
		EventOrderReceived,
		EventOrderDispatched,
		EventOrderStatusChanged,
		EventOrderCompleted,
		EventOrderFailed,
		EventOrderCancelled,
		EventOrderQueued,
		EventBinUpdated,
		EventNodeUpdated,
		EventCorrectionApplied,
		EventFleetConnected,
		EventFleetDisconnected,
		EventMessagingConnected,
		EventMessagingDisconnected,
		EventDBConnected,
		EventDBDisconnected,
		EventRobotsUpdated,
		EventCMSTransaction,
		EventCountGroupTransition,
	}
	seen := map[EventType]bool{}
	for _, tp := range types {
		if int(tp) == 0 {
			t.Errorf("event type %v should be non-zero (iota + 1)", tp)
		}
		if seen[tp] {
			t.Errorf("duplicate event type value: %v", tp)
		}
		seen[tp] = true
	}
	// Sanity: iota starts at 1 and ascends contiguously.
	if EventOrderReceived != 1 {
		t.Errorf("EventOrderReceived = %d, want 1", EventOrderReceived)
	}
}

// TestEventPayloads_RoundTripAllShapes emits one of every payload struct
// through the EventBus and asserts the subscriber sees the same fields.
// This is a single table-driven test so it covers every payload type in
// events.go in one pass, with per-type assertions.
func TestEventPayloads_RoundTripAllShapes(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		evtType EventType
		payload any
		check   func(t *testing.T, got any)
	}{
		{
			name:    "OrderReceived",
			evtType: EventOrderReceived,
			payload: OrderReceivedEvent{
				OrderID: 1, EdgeUUID: "u", StationID: "s", OrderType: "retrieve",
				PayloadCode: "PC", DeliveryNode: "N1",
			},
			check: func(t *testing.T, got any) {
				p, ok := got.(OrderReceivedEvent)
				if !ok {
					t.Fatalf("wrong type: %T", got)
				}
				if p.OrderID != 1 || p.EdgeUUID != "u" || p.DeliveryNode != "N1" {
					t.Errorf("payload = %+v", p)
				}
			},
		},
		{
			name:    "OrderDispatched",
			evtType: EventOrderDispatched,
			payload: OrderDispatchedEvent{OrderID: 2, VendorOrderID: "V1", SourceNode: "A", DestNode: "B"},
			check: func(t *testing.T, got any) {
				p := got.(OrderDispatchedEvent)
				if p.OrderID != 2 || p.VendorOrderID != "V1" || p.SourceNode != "A" || p.DestNode != "B" {
					t.Errorf("payload = %+v", p)
				}
			},
		},
		{
			name:    "OrderStatusChanged",
			evtType: EventOrderStatusChanged,
			payload: OrderStatusChangedEvent{
				OrderID: 3, VendorOrderID: "V3",
				OldStatus: "dispatched", NewStatus: "in_transit",
				RobotID: "AMR-1", Detail: "on the way",
				Snapshot: &fleet.OrderSnapshot{VendorOrderID: "V3", State: "RUNNING"},
			},
			check: func(t *testing.T, got any) {
				p := got.(OrderStatusChangedEvent)
				if p.OldStatus != "dispatched" || p.NewStatus != "in_transit" {
					t.Errorf("status fields = %+v", p)
				}
				if p.Snapshot == nil || p.Snapshot.State != "RUNNING" {
					t.Errorf("snapshot = %+v", p.Snapshot)
				}
			},
		},
		{
			name:    "OrderCompleted",
			evtType: EventOrderCompleted,
			payload: OrderCompletedEvent{OrderID: 4, EdgeUUID: "uc", StationID: "sc"},
			check: func(t *testing.T, got any) {
				p := got.(OrderCompletedEvent)
				if p.OrderID != 4 || p.EdgeUUID != "uc" || p.StationID != "sc" {
					t.Errorf("payload = %+v", p)
				}
			},
		},
		{
			name:    "OrderFailed",
			evtType: EventOrderFailed,
			payload: OrderFailedEvent{OrderID: 5, EdgeUUID: "uf", StationID: "sf", ErrorCode: "E01", Detail: "oops"},
			check: func(t *testing.T, got any) {
				p := got.(OrderFailedEvent)
				if p.ErrorCode != "E01" || p.Detail != "oops" {
					t.Errorf("payload = %+v", p)
				}
			},
		},
		{
			name:    "OrderCancelled",
			evtType: EventOrderCancelled,
			payload: OrderCancelledEvent{OrderID: 6, EdgeUUID: "uc6", StationID: "s6", Reason: "user", PreviousStatus: "dispatched"},
			check: func(t *testing.T, got any) {
				p := got.(OrderCancelledEvent)
				if p.Reason != "user" || p.PreviousStatus != "dispatched" {
					t.Errorf("payload = %+v", p)
				}
			},
		},
		{
			name:    "OrderQueued",
			evtType: EventOrderQueued,
			payload: OrderQueuedEvent{OrderID: 7, EdgeUUID: "uq", StationID: "sq", PayloadCode: "PC"},
			check: func(t *testing.T, got any) {
				p := got.(OrderQueuedEvent)
				if p.OrderID != 7 || p.PayloadCode != "PC" {
					t.Errorf("payload = %+v", p)
				}
			},
		},
		{
			name:    "BinUpdated",
			evtType: EventBinUpdated,
			payload: BinUpdatedEvent{
				NodeID: 10, NodeName: "N10", Action: "moved", BinID: 22, PayloadCode: "PC",
				FromNodeID: 10, ToNodeID: 11, Actor: "system", Detail: "auto",
			},
			check: func(t *testing.T, got any) {
				p := got.(BinUpdatedEvent)
				if p.Action != "moved" || p.FromNodeID != 10 || p.ToNodeID != 11 {
					t.Errorf("payload = %+v", p)
				}
			},
		},
		{
			name:    "NodeUpdated",
			evtType: EventNodeUpdated,
			payload: NodeUpdatedEvent{NodeID: 5, NodeName: "N5", Action: "created"},
			check: func(t *testing.T, got any) {
				p := got.(NodeUpdatedEvent)
				if p.NodeName != "N5" || p.Action != "created" {
					t.Errorf("payload = %+v", p)
				}
			},
		},
		{
			name:    "CorrectionApplied",
			evtType: EventCorrectionApplied,
			payload: CorrectionAppliedEvent{CorrectionID: 9, CorrectionType: "bin_move", NodeID: 1, Reason: "drift", Actor: "op1"},
			check: func(t *testing.T, got any) {
				p := got.(CorrectionAppliedEvent)
				if p.CorrectionType != "bin_move" || p.Reason != "drift" {
					t.Errorf("payload = %+v", p)
				}
			},
		},
		{
			name:    "FleetConnected",
			evtType: EventFleetConnected,
			payload: ConnectionEvent{Detail: "fleet up"},
			check: func(t *testing.T, got any) {
				p := got.(ConnectionEvent)
				if p.Detail != "fleet up" {
					t.Errorf("payload = %+v", p)
				}
			},
		},
		{
			name:    "FleetDisconnected",
			evtType: EventFleetDisconnected,
			payload: ConnectionEvent{Detail: "fleet down"},
			check: func(t *testing.T, got any) {
				p := got.(ConnectionEvent)
				if p.Detail != "fleet down" {
					t.Errorf("payload = %+v", p)
				}
			},
		},
		{
			name:    "MessagingConnected",
			evtType: EventMessagingConnected,
			payload: ConnectionEvent{Detail: "msg up"},
			check: func(t *testing.T, got any) {
				p := got.(ConnectionEvent)
				if p.Detail != "msg up" {
					t.Errorf("payload = %+v", p)
				}
			},
		},
		{
			name:    "MessagingDisconnected",
			evtType: EventMessagingDisconnected,
			payload: ConnectionEvent{Detail: "msg down"},
			check: func(t *testing.T, got any) {
				p := got.(ConnectionEvent)
				if p.Detail != "msg down" {
					t.Errorf("payload = %+v", p)
				}
			},
		},
		{
			name:    "DBConnected",
			evtType: EventDBConnected,
			payload: ConnectionEvent{Detail: "db up"},
			check: func(t *testing.T, got any) {
				p := got.(ConnectionEvent)
				if p.Detail != "db up" {
					t.Errorf("payload = %+v", p)
				}
			},
		},
		{
			name:    "DBDisconnected",
			evtType: EventDBDisconnected,
			payload: ConnectionEvent{Detail: "db down"},
			check: func(t *testing.T, got any) {
				p := got.(ConnectionEvent)
				if p.Detail != "db down" {
					t.Errorf("payload = %+v", p)
				}
			},
		},
		{
			name:    "RobotsUpdated",
			evtType: EventRobotsUpdated,
			payload: RobotsUpdatedEvent{Robots: []fleet.RobotStatus{
				{VehicleID: "AMR-1", Connected: true, Available: true},
				{VehicleID: "AMR-2", Connected: false},
			}},
			check: func(t *testing.T, got any) {
				p := got.(RobotsUpdatedEvent)
				if len(p.Robots) != 2 || p.Robots[0].VehicleID != "AMR-1" {
					t.Errorf("payload = %+v", p)
				}
			},
		},
		{
			name:    "CMSTransaction",
			evtType: EventCMSTransaction,
			payload: CMSTransactionEvent{Transactions: []*store.CMSTransaction{
				{}, {}, // two empty entries — we only assert the slice length
			}},
			check: func(t *testing.T, got any) {
				p := got.(CMSTransactionEvent)
				if len(p.Transactions) != 2 {
					t.Errorf("transactions = %d, want 2", len(p.Transactions))
				}
			},
		},
		{
			name:    "CountGroupTransition",
			evtType: EventCountGroupTransition,
			payload: CountGroupTransitionEvent{
				Group: "Cross1", Desired: "on",
				Robots:            []string{"AMR-1"},
				FailSafeTriggered: true,
				Timestamp:         now,
			},
			check: func(t *testing.T, got any) {
				p := got.(CountGroupTransitionEvent)
				if p.Group != "Cross1" || p.Desired != "on" {
					t.Errorf("payload = %+v", p)
				}
				if !p.FailSafeTriggered {
					t.Error("FailSafeTriggered should be true")
				}
				if !p.Timestamp.Equal(now) {
					t.Errorf("timestamp drift: got %v want %v", p.Timestamp, now)
				}
				if len(p.Robots) != 1 || p.Robots[0] != "AMR-1" {
					t.Errorf("robots = %v", p.Robots)
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			bus := NewEventBus()
			var captured Event
			bus.Subscribe(func(evt Event) {
				captured = evt
			})
			bus.Emit(Event{Type: tc.evtType, Payload: tc.payload})
			if captured.Type != tc.evtType {
				t.Errorf("event type = %v, want %v", captured.Type, tc.evtType)
			}
			tc.check(t, captured.Payload)
		})
	}
}

// TestEvent_TimestampAutofill documents the bus's auto-timestamp
// behavior so callers of events.go payloads know their Event.Timestamp
// is populated even when they don't set one. Part of the events.go
// contract because payloads ride inside Event.
func TestEvent_TimestampAutofill(t *testing.T) {
	bus := NewEventBus()
	var got Event
	bus.Subscribe(func(evt Event) { got = evt })
	before := time.Now()
	bus.Emit(Event{Type: EventOrderQueued, Payload: OrderQueuedEvent{OrderID: 99}})
	after := time.Now()
	if got.Timestamp.IsZero() {
		t.Fatal("Event.Timestamp was not auto-filled")
	}
	if got.Timestamp.Before(before) || got.Timestamp.After(after) {
		t.Errorf("Timestamp %v outside [%v, %v]", got.Timestamp, before, after)
	}
	// Payload survives the roundtrip.
	if p := got.Payload.(OrderQueuedEvent); p.OrderID != 99 {
		t.Errorf("payload lost: %+v", p)
	}
}
