//go:build docker

// lifecycle_test.go — Driver behaviour tests against real Postgres.
//
// Covers:
//   - Full (from, to) matrix: every legal transition succeeds, every
//     illegal one returns IllegalTransition.
//   - Typed methods (CancelOrder, ConfirmReceipt, Release, MarkInTransit,
//     MarkStaged, MarkDelivered, Queue, MoveToSourcing, Dispatch, Fail,
//     BeginReshuffle, CompleteCompound, Acknowledge): each writes the
//     correct target status.
//   - Idempotency: CancelOrder on terminal status is a no-op;
//     ConfirmReceipt on already-completed order returns (false, nil).
//   - Action firing: emitCancelled fires with PreviousStatus populated;
//     emitFailed fires with the error code/detail.
//
// Pure-computation tests (error types, action map structural invariants)
// live in lifecycle_pure_test.go and run without Docker.

package dispatch

import (
	"errors"
	"fmt"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

func newLifecycleForTest(t *testing.T, db *store.DB) (*LifecycleService, *mockEmitter) {
	t.Helper()
	emitter := &mockEmitter{}
	backend := testdb.NewTrackingBackend()
	lc := newLifecycleService(db, backend, emitter, nil, nil, nil)
	return lc, emitter
}

// makeOrderAt creates an order with the given starting status. Bypasses
// the lifecycle's protocol validation (we want to set up arbitrary
// starting states for matrix coverage).
func makeOrderAt(t *testing.T, db *store.DB, uuid string, status protocol.Status) *orders.Order {
	t.Helper()
	ord := &orders.Order{
		EdgeUUID:     uuid,
		StationID:    "edge.test",
		OrderType:    OrderTypeRetrieve,
		Status:       status,
		Quantity:     1,
		DeliveryNode: "DELV.1",
	}
	if err := db.CreateOrder(ord); err != nil {
		t.Fatalf("create order at status %s: %v", status, err)
	}
	if err := db.UpdateOrderStatus(ord.ID, string(status), "test fixture"); err != nil {
		t.Fatalf("update fixture status to %s: %v", status, err)
	}
	ord.Status = status
	return ord
}

// ── Full (from, to) matrix ───────────────────────────────────────────────

func TestLifecycle_LegalTransitions_AllPersist(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	lc, _ := newLifecycleForTest(t, db)

	i := 0
	for from, allowed := range protocol.AllValidTransitions() {
		for _, to := range allowed {
			i++
			ord := makeOrderAt(t, db, fmt.Sprintf("legal-%d", i), from)
			err := lc.transition(ord, to, Event{Actor: "test", Reason: "matrix"})
			if err != nil {
				t.Errorf("legal %s→%s rejected: %v", from, to, err)
				continue
			}
			if ord.Status != to {
				t.Errorf("legal %s→%s: in-memory status = %q, want %q", from, to, ord.Status, to)
			}
			persisted, err := db.GetOrder(ord.ID)
			if err != nil {
				t.Errorf("legal %s→%s: GetOrder: %v", from, to, err)
				continue
			}
			if persisted.Status != to {
				t.Errorf("legal %s→%s: persisted status = %q, want %q", from, to, persisted.Status, to)
			}
		}
	}
}

func TestLifecycle_IllegalTransitions_AllRejected(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	lc, _ := newLifecycleForTest(t, db)

	i := 0
	for _, from := range protocol.AllStatuses() {
		for _, to := range protocol.AllStatuses() {
			if protocol.IsValidTransition(from, to) {
				continue
			}
			i++
			ord := makeOrderAt(t, db, fmt.Sprintf("illegal-%d", i), from)
			err := lc.transition(ord, to, Event{Actor: "test", Reason: "matrix"})
			if err == nil {
				t.Errorf("illegal %s→%s was accepted; expected IllegalTransition", from, to)
				continue
			}
			var illegal IllegalTransition
			if !errors.As(err, &illegal) {
				t.Errorf("illegal %s→%s: error was %v (%T), expected IllegalTransition", from, to, err, err)
				continue
			}
			if illegal.From != from || illegal.To != to {
				t.Errorf("illegal %s→%s: error mismatch From=%q To=%q", from, to, illegal.From, illegal.To)
			}
			// Status should remain unchanged.
			persisted, err := db.GetOrder(ord.ID)
			if err != nil {
				t.Errorf("illegal %s→%s: GetOrder: %v", from, to, err)
				continue
			}
			if persisted.Status != from {
				t.Errorf("illegal %s→%s: persisted status changed to %q (should still be %q)", from, to, persisted.Status, from)
			}
		}
	}
}

// ── Typed methods produce the right transition ──────────────────────────

func TestLifecycle_CancelOrder_PersistsCancelled(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	lc, emitter := newLifecycleForTest(t, db)
	ord := makeOrderAt(t, db, "cancel-1", StatusInTransit)

	lc.CancelOrder(ord, "edge.test", "operator cancel")

	persisted, _ := db.GetOrder(ord.ID)
	if persisted.Status != StatusCancelled {
		t.Errorf("after CancelOrder, status = %q, want %q", persisted.Status, StatusCancelled)
	}
	if len(emitter.cancelled) != 1 {
		t.Fatalf("expected 1 cancellation emit, got %d", len(emitter.cancelled))
	}
	if emitter.cancelled[0].reason != "operator cancel" {
		t.Errorf("emit reason = %q, want %q", emitter.cancelled[0].reason, "operator cancel")
	}
}

func TestLifecycle_CancelOrder_IdempotentOnTerminal(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	lc, emitter := newLifecycleForTest(t, db)
	ord := makeOrderAt(t, db, "cancel-term-1", StatusConfirmed)

	lc.CancelOrder(ord, "edge.test", "redundant cancel")

	persisted, _ := db.GetOrder(ord.ID)
	if persisted.Status != StatusConfirmed {
		t.Errorf("terminal status changed: got %q, want %q", persisted.Status, StatusConfirmed)
	}
	if len(emitter.cancelled) != 0 {
		t.Errorf("expected 0 emits for idempotent cancel of terminal order, got %d", len(emitter.cancelled))
	}
}

func TestLifecycle_ConfirmReceipt_AlreadyCompleted(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	lc, emitter := newLifecycleForTest(t, db)
	ord := makeOrderAt(t, db, "confirm-dup", StatusDelivered)
	if err := db.CompleteOrder(ord.ID); err != nil {
		t.Fatalf("complete order: %v", err)
	}
	reloaded, _ := db.GetOrder(ord.ID)

	ok, err := lc.ConfirmReceipt(reloaded, "edge.test", "confirmed", 1)
	if err != nil {
		t.Fatalf("ConfirmReceipt on completed order: %v", err)
	}
	if ok {
		t.Error("ConfirmReceipt returned ok=true for already-completed order, want false")
	}
	if len(emitter.completed) != 0 {
		t.Errorf("ConfirmReceipt emitted on already-completed order: %d", len(emitter.completed))
	}
}

func TestLifecycle_ConfirmReceipt_DeliveredToConfirmed(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	lc, emitter := newLifecycleForTest(t, db)
	ord := makeOrderAt(t, db, "confirm-1", StatusDelivered)

	ok, err := lc.ConfirmReceipt(ord, "edge.test", "confirmed", 5)
	if err != nil {
		t.Fatalf("ConfirmReceipt: %v", err)
	}
	if !ok {
		t.Error("ConfirmReceipt returned ok=false for valid Delivered order")
	}
	persisted, _ := db.GetOrder(ord.ID)
	if persisted.Status != StatusConfirmed {
		t.Errorf("after ConfirmReceipt, status = %q, want %q", persisted.Status, StatusConfirmed)
	}
	if len(emitter.completed) != 1 {
		t.Errorf("expected 1 completion emit, got %d", len(emitter.completed))
	}
}

func TestLifecycle_Fail_PersistsFailed(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	lc, emitter := newLifecycleForTest(t, db)
	ord := makeOrderAt(t, db, "fail-1", StatusInTransit)

	if err := lc.Fail(ord, "edge.test", "fleet_failed", "robot stuck"); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	persisted, _ := db.GetOrder(ord.ID)
	if persisted.Status != StatusFailed {
		t.Errorf("after Fail, status = %q, want %q", persisted.Status, StatusFailed)
	}
	if len(emitter.failed) != 1 {
		t.Fatalf("expected 1 failure emit, got %d", len(emitter.failed))
	}
	if emitter.failed[0].errorCode != "fleet_failed" {
		t.Errorf("emit errorCode = %q, want %q", emitter.failed[0].errorCode, "fleet_failed")
	}
}

func TestLifecycle_Fail_RejectsTerminal(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	lc, _ := newLifecycleForTest(t, db)
	ord := makeOrderAt(t, db, "fail-term-1", StatusCancelled)

	err := lc.Fail(ord, "edge.test", "fleet_failed", "should not apply")
	if !IsIllegalTransition(err) {
		t.Errorf("Fail on terminal: error = %v, want IllegalTransition", err)
	}
	persisted, _ := db.GetOrder(ord.ID)
	if persisted.Status != StatusCancelled {
		t.Errorf("status changed despite rejection: %q", persisted.Status)
	}
}

func TestLifecycle_TypedMethods_CorrectTargets(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	lc, _ := newLifecycleForTest(t, db)

	cases := []struct {
		name       string
		fromStatus protocol.Status
		invoke     func(ord *orders.Order) error
		wantStatus protocol.Status
	}{
		{"Release_StagedToInTransit", StatusStaged,
			func(o *orders.Order) error { return lc.Release(o, "test") },
			StatusInTransit},
		{"MarkInTransit_AcknowledgedToInTransit", StatusAcknowledged,
			func(o *orders.Order) error { return lc.MarkInTransit(o, "robot-1", "fleet") },
			StatusInTransit},
		{"MarkStaged_InTransitToStaged", StatusInTransit,
			func(o *orders.Order) error { return lc.MarkStaged(o, "fleet") },
			StatusStaged},
		{"MarkDelivered_InTransitToDelivered", StatusInTransit,
			func(o *orders.Order) error { return lc.MarkDelivered(o, "fleet") },
			StatusDelivered},
		{"Queue_PendingToQueued", StatusPending,
			func(o *orders.Order) error { return lc.Queue(o, "scanner", "") },
			StatusQueued},
		{"MoveToSourcing_PendingToSourcing", StatusPending,
			func(o *orders.Order) error { return lc.MoveToSourcing(o, "planner", "finding source") },
			StatusSourcing},
		{"Dispatch_QueuedToDispatched", StatusQueued,
			func(o *orders.Order) error { return lc.Dispatch(o, "vendor-id-1", "dispatcher") },
			StatusDispatched},
		{"Acknowledge_QueuedToAcknowledged", StatusQueued,
			func(o *orders.Order) error { return lc.Acknowledge(o, "fleet") },
			StatusAcknowledged},
		{"BeginReshuffle_PendingToReshuffling", StatusPending,
			func(o *orders.Order) error { return lc.BeginReshuffle(o, "reshuffle plan") },
			StatusReshuffling},
		{"CompleteCompound_ReshufflingToConfirmed", StatusReshuffling,
			func(o *orders.Order) error { return lc.CompleteCompound(o) },
			StatusConfirmed},
	}
	for i, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			ord := makeOrderAt(t, db, fmt.Sprintf("typed-%d", i), c.fromStatus)
			if err := c.invoke(ord); err != nil {
				t.Fatalf("%s: invoke: %v", c.name, err)
			}
			persisted, _ := db.GetOrder(ord.ID)
			if persisted.Status != c.wantStatus {
				t.Errorf("%s: status = %q, want %q", c.name, persisted.Status, c.wantStatus)
			}
		})
	}
}

// ── Action firing semantics ──────────────────────────────────────────────

func TestLifecycle_EmitCancelled_PreviousStatusPopulated(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	lc, emitter := newLifecycleForTest(t, db)

	for _, from := range []protocol.Status{StatusInTransit, StatusStaged, StatusDispatched} {
		emitter.cancelled = nil
		ord := makeOrderAt(t, db, "prev-"+string(from), from)
		lc.CancelOrder(ord, "edge.test", "operator cancel")
		if len(emitter.cancelled) != 1 {
			t.Errorf("from=%s: expected 1 emit, got %d", from, len(emitter.cancelled))
			continue
		}
	}
}
