//go:build docker

package engine

import (
	"sync"
	"testing"
	"time"

	"shingocore/countgroup"
	"shingocore/fleet"
	"shingocore/store/orders"
)

// adapters_test.go — coverage tests for adapters.go.
//
// adapters.go holds three EventBus-bridging emitter structs
// (dispatchEmitter, pollerEmitter, countGroupEventEmitter) and one
// fleet.OrderIDResolver adapter (orderResolver). Each emitter has no
// state of its own — it just repackages its arguments into an Event
// and forwards to the bus. These tests assert every emitter method:
//
//   - fires an Event with the expected EventType,
//   - wraps a payload of the expected struct type,
//   - copies every input argument through to the payload unchanged.
//
// The orderResolver test exercises both the happy path (found) and
// the error path (unknown vendor ID) using a real store.DB.

// captureBus returns a fresh EventBus wired to a subscriber that
// records every event keyed by EventType. The last emit for a given
// type wins, which is fine: every emitter test fires each event once.
func captureBus() (*EventBus, *sync.Mutex, map[EventType]Event) {
	bus := NewEventBus()
	mu := &sync.Mutex{}
	received := map[EventType]Event{}
	bus.Subscribe(func(evt Event) {
		mu.Lock()
		defer mu.Unlock()
		received[evt.Type] = evt
	})
	return bus, mu, received
}

// ── dispatchEmitter ─────────────────────────────────────────────────

func TestDispatchEmitter_EmitOrderReceived(t *testing.T) {
	bus, mu, got := captureBus()
	em := &dispatchEmitter{bus: bus}
	em.EmitOrderReceived(42, "edge-uuid", "line-1", "retrieve", "PART-A", "LINE1-IN")

	mu.Lock()
	defer mu.Unlock()
	evt, ok := got[EventOrderReceived]
	if !ok {
		t.Fatal("EventOrderReceived not emitted")
	}
	p, ok := evt.Payload.(OrderReceivedEvent)
	if !ok {
		t.Fatalf("payload type = %T, want OrderReceivedEvent", evt.Payload)
	}
	if p.OrderID != 42 || p.EdgeUUID != "edge-uuid" || p.StationID != "line-1" ||
		p.OrderType != "retrieve" || p.PayloadCode != "PART-A" || p.DeliveryNode != "LINE1-IN" {
		t.Errorf("OrderReceivedEvent fields = %+v", p)
	}
}

func TestDispatchEmitter_EmitOrderDispatched(t *testing.T) {
	bus, mu, got := captureBus()
	em := &dispatchEmitter{bus: bus}
	em.EmitOrderDispatched(7, "V-100", "STORAGE-A1", "LINE1-IN")

	mu.Lock()
	defer mu.Unlock()
	evt, ok := got[EventOrderDispatched]
	if !ok {
		t.Fatal("EventOrderDispatched not emitted")
	}
	p := evt.Payload.(OrderDispatchedEvent)
	if p.OrderID != 7 || p.VendorOrderID != "V-100" ||
		p.SourceNode != "STORAGE-A1" || p.DestNode != "LINE1-IN" {
		t.Errorf("OrderDispatchedEvent fields = %+v", p)
	}
}

func TestDispatchEmitter_EmitOrderFailed(t *testing.T) {
	bus, mu, got := captureBus()
	em := &dispatchEmitter{bus: bus}
	em.EmitOrderFailed(11, "euid", "st", "ERR_TIMEOUT", "vendor timed out")

	mu.Lock()
	defer mu.Unlock()
	evt, ok := got[EventOrderFailed]
	if !ok {
		t.Fatal("EventOrderFailed not emitted")
	}
	p := evt.Payload.(OrderFailedEvent)
	if p.OrderID != 11 || p.EdgeUUID != "euid" || p.StationID != "st" ||
		p.ErrorCode != "ERR_TIMEOUT" || p.Detail != "vendor timed out" {
		t.Errorf("OrderFailedEvent fields = %+v", p)
	}
}

func TestDispatchEmitter_EmitOrderCancelled(t *testing.T) {
	bus, mu, got := captureBus()
	em := &dispatchEmitter{bus: bus}
	em.EmitOrderCancelled(12, "euid", "st", "user_request", "dispatched")

	mu.Lock()
	defer mu.Unlock()
	evt, ok := got[EventOrderCancelled]
	if !ok {
		t.Fatal("EventOrderCancelled not emitted")
	}
	p := evt.Payload.(OrderCancelledEvent)
	if p.OrderID != 12 || p.Reason != "user_request" || p.PreviousStatus != "dispatched" {
		t.Errorf("OrderCancelledEvent fields = %+v", p)
	}
}

func TestDispatchEmitter_EmitOrderCompleted(t *testing.T) {
	bus, mu, got := captureBus()
	em := &dispatchEmitter{bus: bus}
	em.EmitOrderCompleted(13, "euid-c", "st-c")

	mu.Lock()
	defer mu.Unlock()
	evt, ok := got[EventOrderCompleted]
	if !ok {
		t.Fatal("EventOrderCompleted not emitted")
	}
	p := evt.Payload.(OrderCompletedEvent)
	if p.OrderID != 13 || p.EdgeUUID != "euid-c" || p.StationID != "st-c" {
		t.Errorf("OrderCompletedEvent fields = %+v", p)
	}
}

func TestDispatchEmitter_EmitOrderQueued(t *testing.T) {
	bus, mu, got := captureBus()
	em := &dispatchEmitter{bus: bus}
	em.EmitOrderQueued(14, "euid-q", "st-q", "PART-A")

	mu.Lock()
	defer mu.Unlock()
	evt, ok := got[EventOrderQueued]
	if !ok {
		t.Fatal("EventOrderQueued not emitted")
	}
	p := evt.Payload.(OrderQueuedEvent)
	if p.OrderID != 14 || p.EdgeUUID != "euid-q" || p.PayloadCode != "PART-A" {
		t.Errorf("OrderQueuedEvent fields = %+v", p)
	}
}

// TestDispatchEmitter_AllMethodsCovered is a belt-and-suspenders check
// that emitting every method of the dispatchEmitter fires exactly one
// matching event type and doesn't leak crosstalk.
func TestDispatchEmitter_AllMethodsCovered(t *testing.T) {
	bus, mu, got := captureBus()
	em := &dispatchEmitter{bus: bus}
	em.EmitOrderReceived(1, "", "", "", "", "")
	em.EmitOrderDispatched(1, "", "", "")
	em.EmitOrderFailed(1, "", "", "", "")
	em.EmitOrderCancelled(1, "", "", "", "")
	em.EmitOrderCompleted(1, "", "")
	em.EmitOrderQueued(1, "", "", "")

	mu.Lock()
	defer mu.Unlock()
	want := []EventType{
		EventOrderReceived, EventOrderDispatched, EventOrderFailed,
		EventOrderCancelled, EventOrderCompleted, EventOrderQueued,
	}
	for _, tp := range want {
		if _, ok := got[tp]; !ok {
			t.Errorf("missing event type %v", tp)
		}
	}
	if len(got) != len(want) {
		t.Errorf("received %d event types, want %d", len(got), len(want))
	}
}

// ── pollerEmitter ───────────────────────────────────────────────────

func TestPollerEmitter_EmitOrderStatusChanged_WithSnapshot(t *testing.T) {
	bus, mu, got := captureBus()
	em := &pollerEmitter{bus: bus}

	snap := &fleet.OrderSnapshot{
		VendorOrderID: "V-999",
		State:         "RUNNING",
		Vehicle:       "AMR-7",
	}
	em.EmitOrderStatusChanged(55, "V-999", "dispatched", "in_transit", "AMR-7", "moving", snap)

	mu.Lock()
	defer mu.Unlock()
	evt, ok := got[EventOrderStatusChanged]
	if !ok {
		t.Fatal("EventOrderStatusChanged not emitted")
	}
	p, ok := evt.Payload.(OrderStatusChangedEvent)
	if !ok {
		t.Fatalf("payload type = %T", evt.Payload)
	}
	if p.OrderID != 55 || p.VendorOrderID != "V-999" ||
		p.OldStatus != "dispatched" || p.NewStatus != "in_transit" ||
		p.RobotID != "AMR-7" || p.Detail != "moving" {
		t.Errorf("payload fields = %+v", p)
	}
	if p.Snapshot == nil || p.Snapshot.Vehicle != "AMR-7" {
		t.Errorf("snapshot = %+v", p.Snapshot)
	}
}

func TestPollerEmitter_EmitOrderStatusChanged_NilSnapshot(t *testing.T) {
	// Nil-snapshot branch — the emitter must forward nil without panicking.
	bus, mu, got := captureBus()
	em := &pollerEmitter{bus: bus}
	em.EmitOrderStatusChanged(56, "V-000", "pending", "dispatched", "", "", nil)

	mu.Lock()
	defer mu.Unlock()
	p := got[EventOrderStatusChanged].Payload.(OrderStatusChangedEvent)
	if p.Snapshot != nil {
		t.Errorf("Snapshot should be nil, got %+v", p.Snapshot)
	}
	if p.OrderID != 56 || p.NewStatus != "dispatched" {
		t.Errorf("payload fields = %+v", p)
	}
}

// ── countGroupEventEmitter ──────────────────────────────────────────

func TestCountGroupEventEmitter_Emit(t *testing.T) {
	bus, mu, got := captureBus()
	em := &countGroupEventEmitter{bus: bus}
	now := time.Now()

	tr := countgroup.Transition{
		Group:             "Crosswalk1",
		Desired:           "on",
		Robots:            []string{"AMR-1", "AMR-2"},
		FailSafeTriggered: false,
		Timestamp:         now,
	}
	em.Emit(tr)

	mu.Lock()
	defer mu.Unlock()
	evt, ok := got[EventCountGroupTransition]
	if !ok {
		t.Fatal("EventCountGroupTransition not emitted")
	}
	p, ok := evt.Payload.(CountGroupTransitionEvent)
	if !ok {
		t.Fatalf("payload type = %T", evt.Payload)
	}
	if p.Group != "Crosswalk1" || p.Desired != "on" {
		t.Errorf("group/desired = %+v", p)
	}
	if len(p.Robots) != 2 || p.Robots[0] != "AMR-1" || p.Robots[1] != "AMR-2" {
		t.Errorf("Robots = %v", p.Robots)
	}
	if p.FailSafeTriggered {
		t.Error("FailSafeTriggered should be false")
	}
	if !p.Timestamp.Equal(now) {
		t.Errorf("Timestamp = %v, want %v", p.Timestamp, now)
	}
}

func TestCountGroupEventEmitter_Emit_FailSafe(t *testing.T) {
	// The fail-safe branch fires when RDS is down — Robots is typically nil.
	bus, mu, got := captureBus()
	em := &countGroupEventEmitter{bus: bus}
	em.Emit(countgroup.Transition{
		Group:             "Crosswalk2",
		Desired:           "on",
		Robots:            nil,
		FailSafeTriggered: true,
		Timestamp:         time.Now(),
	})

	mu.Lock()
	defer mu.Unlock()
	p := got[EventCountGroupTransition].Payload.(CountGroupTransitionEvent)
	if !p.FailSafeTriggered {
		t.Error("FailSafeTriggered should be true for fail-safe branch")
	}
	if p.Robots != nil {
		t.Errorf("Robots should be nil, got %v", p.Robots)
	}
}

// ── orderResolver ───────────────────────────────────────────────────

func TestOrderResolver_ResolveVendorOrderID_Found(t *testing.T) {
	db := testDB(t)
	setupTestData(t, db)

	// Seed an order carrying a known vendor_order_id.
	order := &orders.Order{
		EdgeUUID:     "resolver-uuid",
		StationID:    "line-1",
		OrderType:    "retrieve",
		Status:       "dispatched",
		SourceNode:   "STORAGE-A1",
		DeliveryNode: "LINE1-IN",
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	// CreateOrder doesn't set vendor_order_id — use UpdateOrderVendor.
	if err := db.UpdateOrderVendor(order.ID, "V-FOUND-1", "RUNNING", "AMR-5"); err != nil {
		t.Fatalf("update vendor: %v", err)
	}

	r := &orderResolver{db: db}
	got, err := r.ResolveVendorOrderID("V-FOUND-1")
	if err != nil {
		t.Fatalf("ResolveVendorOrderID: %v", err)
	}
	if got != order.ID {
		t.Errorf("got id = %d, want %d", got, order.ID)
	}
}

func TestOrderResolver_ResolveVendorOrderID_NotFound(t *testing.T) {
	db := testDB(t)
	setupTestData(t, db)

	r := &orderResolver{db: db}
	id, err := r.ResolveVendorOrderID("V-DOES-NOT-EXIST")
	// Contract: on lookup failure the resolver returns 0 and a non-nil error.
	if err == nil {
		t.Fatal("expected error for unknown vendor id")
	}
	if id != 0 {
		t.Errorf("id = %d on error, want 0", id)
	}
}

// TestOrderResolver_ImplementsInterface asserts the adapter satisfies
// fleet.OrderIDResolver — a compile-time check. If the interface moves
// or gains methods, this test stops compiling, which is the point.
func TestOrderResolver_ImplementsInterface(t *testing.T) {
	db := testDB(t)
	var _ fleet.OrderIDResolver = &orderResolver{db: db}
}
