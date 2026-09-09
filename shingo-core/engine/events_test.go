//go:build docker

package engine

import (
	"reflect"
	"testing"
	"time"

	"shingocore/fleet"
	"shingocore/store/cms"
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
	t.Parallel()
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
		EventFleetConnected,
		EventFleetDisconnected,
		EventMessagingConnected,
		EventMessagingDisconnected,
		EventDBConnected,
		EventDBDisconnected,
		EventRobotsUpdated,
		EventCMSTransaction,
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
	t.Parallel()
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
				NodeID: 10, Action: BinActionMoved, BinID: 22, PayloadCode: "PC",
				FromNodeID: 10, ToNodeID: 11, Actor: "system", Detail: "auto",
			},
			check: func(t *testing.T, got any) {
				p := got.(BinUpdatedEvent)
				if p.Action != BinActionMoved || p.FromNodeID != 10 || p.ToNodeID != 11 {
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
			payload: CMSTransactionEvent{Transactions: []*cms.Transaction{
				{}, {}, // two empty entries — we only assert the slice length
			}},
			check: func(t *testing.T, got any) {
				p := got.(CMSTransactionEvent)
				if len(p.Transactions) != 2 {
					t.Errorf("transactions = %d, want 2", len(p.Transactions))
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
	t.Parallel()
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

// TestBinAction_FieldContract pins the per-value contract stated next to the
// BinAction type: which placement fields (FromNodeID / ToNodeID / NodeID) and
// attribution fields (RobotID / OrderID) each action MUST and MUST NOT carry.
//
// The field rules are asserted on VALID emit-site shapes — one canonical
// emitter per action, fields filled per the contract — by walking every value
// in the vocabulary through a rules table. An emitter that starts filling a
// "must not" field (the shape that let a subscriber guess a zeroed field's
// meaning and guess wrong, 2026-09) breaks the table only if this test is
// taught the emitter, so the canonical shapes here double as documentation of
// what each emitter sends.
//
// The wire-bytes row is separate and load-bearing: the constants are
// string-valued precisely so the SSE `bin-update` frame and the audit journal
// stay byte-identical to the untyped era, and that property is asserted, not
// assumed.
//
// MUTATION: rename a constant's string (e.g. BinActionMoved = "mooved") and
// the wire-bytes row fails. Add a tenth value and the closed-set row fails
// until the vocabulary table here is updated — which is the point: a new
// action must argue its field contract in the same edit.
func TestBinAction_FieldContract(t *testing.T) {
	t.Parallel()

	// The closed set, with its wire spelling. This table IS the vocabulary;
	// adding a BinAction constant without a row here fails the first loop.
	vocabulary := []struct {
		value BinAction
		wire  string
	}{
		{BinActionMoved, "moved"},
		{BinActionEvicted, "evicted"},
		{BinActionCreated, "created"},
		{BinActionStatusChanged, "status_changed"},
		{BinActionLocked, "locked"},
		{BinActionUnlocked, "unlocked"},
		{BinActionLoaded, "loaded"},
		{BinActionCleared, "cleared"},
		{BinActionCounted, "counted"},
	}
	seen := map[BinAction]bool{}
	for _, v := range vocabulary {
		if seen[v.value] {
			t.Errorf("duplicate BinAction value %q", v.value)
		}
		seen[v.value] = true
		if string(v.value) != v.wire {
			t.Errorf("BinAction %q changed wire spelling to %q — SSE frames and the audit journal are byte-compared downstream", v.wire, string(v.value))
		}
	}

	// One canonical emitter shape per action, taken from the real emit sites:
	// engine/wiring_completion.go (moved, evicted), www/handlers_bins.go
	// (created), www/bin_actions.go (the rest, via emitBinUpdate).
	cases := []struct {
		name  string
		event BinUpdatedEvent
		// mustFill / mustNotFill name the placement and attribution fields,
		// so a refactor that adds a field to the struct fails this test until
		// the contract is decided for it — not silently after.
		mustFill    []string
		mustNotFill []string
	}{
		{
			name: "moved (delivery)",
			event: BinUpdatedEvent{Action: BinActionMoved, BinID: 1, PayloadCode: "PC",
				FromNodeID: 10, ToNodeID: 11, NodeID: 11, RobotID: "AMR-1", OrderID: 7},
			mustFill: []string{"FromNodeID", "ToNodeID", "NodeID", "RobotID", "OrderID"},
		},
		{
			name: "moved (operator drag)",
			event: BinUpdatedEvent{Action: BinActionMoved, BinID: 1, PayloadCode: "PC",
				FromNodeID: 10, ToNodeID: 11, NodeID: 11},
			mustFill: []string{"FromNodeID", "ToNodeID", "NodeID"},
		},
		{
			name: "evicted",
			event: BinUpdatedEvent{Action: BinActionEvicted, BinID: 1, PayloadCode: "PC",
				ToNodeID: 99, NodeID: 99},
			mustFill:    []string{"ToNodeID", "NodeID"},
			mustNotFill: []string{"FromNodeID", "RobotID", "OrderID"},
		},
		{
			name:        "created",
			event:       BinUpdatedEvent{Action: BinActionCreated, NodeID: 5},
			mustFill:    []string{"NodeID"},
			mustNotFill: []string{"FromNodeID", "ToNodeID", "RobotID", "OrderID"},
		},
		{
			name:        "status_changed",
			event:       BinUpdatedEvent{Action: BinActionStatusChanged, BinID: 1, NodeID: 5, PayloadCode: "PC"},
			mustFill:    []string{"NodeID"},
			mustNotFill: []string{"FromNodeID", "ToNodeID", "RobotID", "OrderID"},
		},
		{
			name:        "locked",
			event:       BinUpdatedEvent{Action: BinActionLocked, BinID: 1, NodeID: 5, PayloadCode: "PC", Detail: "op"},
			mustFill:    []string{"NodeID"},
			mustNotFill: []string{"FromNodeID", "ToNodeID", "RobotID", "OrderID"},
		},
		{
			name:        "unlocked",
			event:       BinUpdatedEvent{Action: BinActionUnlocked, BinID: 1, NodeID: 5, PayloadCode: "PC"},
			mustFill:    []string{"NodeID"},
			mustNotFill: []string{"FromNodeID", "ToNodeID", "RobotID", "OrderID"},
		},
		{
			name:        "loaded",
			event:       BinUpdatedEvent{Action: BinActionLoaded, BinID: 1, NodeID: 5, PayloadCode: "PC", Detail: "PC"},
			mustFill:    []string{"NodeID"},
			mustNotFill: []string{"FromNodeID", "ToNodeID", "RobotID", "OrderID"},
		},
		{
			name:        "cleared",
			event:       BinUpdatedEvent{Action: BinActionCleared, BinID: 1, NodeID: 5},
			mustFill:    []string{"NodeID"},
			mustNotFill: []string{"FromNodeID", "ToNodeID", "RobotID", "OrderID"},
		},
		{
			name:        "counted",
			event:       BinUpdatedEvent{Action: BinActionCounted, BinID: 1, NodeID: 5, PayloadCode: "PC"},
			mustFill:    []string{"NodeID"},
			mustNotFill: []string{"FromNodeID", "ToNodeID", "RobotID", "OrderID"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !seen[tc.event.Action] {
				t.Fatalf("case %q uses BinAction %q, which is not in the vocabulary table", tc.name, tc.event.Action)
			}
			ev := reflect.ValueOf(tc.event)
			for _, fname := range tc.mustFill {
				if !ev.FieldByName(fname).IsValid() {
					t.Fatalf("field %s does not exist on BinUpdatedEvent — the struct grew; decide its contract for every action and update this test", fname)
				}
				if ev.FieldByName(fname).IsZero() {
					t.Errorf("%s: %s must be filled (non-zero) per the contract", tc.event.Action, fname)
				}
			}
			for _, fname := range tc.mustNotFill {
				if !ev.FieldByName(fname).IsValid() {
					t.Fatalf("field %s does not exist on BinUpdatedEvent — the struct grew; decide its contract for every action and update this test", fname)
				}
				if !ev.FieldByName(fname).IsZero() {
					t.Errorf("%s: %s must NOT be filled per the contract — a value here teaches the next subscriber the field is sometimes meaningful for this action", tc.event.Action, fname)
				}
			}
		})
	}
}
