package messaging

import (
	"path/filepath"
	"testing"

	"shingo/protocol"
	"shingoedge/orders"
	"shingoedge/store"
)

// queue_reason is DERIVED, not latched.
//
// HandleOrderUpdate used to write the reason only when the pushed one was
// non-empty, which made the field write-once: Core pushes a reason when it
// queues an order and clears its OWN copy on dispatch without ever pushing the
// clear, so the Edge's copy survived forever. Springfield 2026-08-03 — Core's
// queue_reason for order 4017 was empty while the Edge still read "Waiting for
// material: 76683-6TA0A.06", and the operator-station modal displayed that
// sentence 2½ hours later, during a changeover to a different style, naming the
// style the line had already left.
//
// Trusting the pushed value instead would have been the same bug pointed the
// other way: an empty string on the wire is indistinguishable from an absent one
// (Go zero value + omitempty), so any unrelated status update would wipe a LIVE
// reason. So the Edge derives it from the status it is already being told — a
// reason explains a wait, and IsAcquiring (queued|sourcing) is exactly when an
// order is waiting. Same predicate Core gates the push on, so the two agree by
// construction rather than by protocol.

func testHandlerDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "handler.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// noopEmitter satisfies orders.EventEmitter. These tests assert on the stored
// row, not on emissions; the emitter exists only because status transitions fan
// out through it.
type noopEmitter struct{}

func (noopEmitter) EmitOrderCreated(int64, string, protocol.OrderType, *int64, *int64) {}
func (noopEmitter) EmitOrderStatusChanged(int64, string, protocol.OrderType, string, string, string, *int64, *int64) {
}
func (noopEmitter) EmitOrderCompleted(int64, string, protocol.OrderType, *int64, *int64) {}
func (noopEmitter) EmitOrderDelivered(int64, string, protocol.OrderType, *int64, *int64, *int, int64, string, string) {
}
func (noopEmitter) EmitOrderDeliveredFallback(int64, *int, int64, string)     {}
func (noopEmitter) EmitOrderFailed(int64, string, protocol.OrderType, string) {}
func (noopEmitter) EmitOrderFaulted(int64, string, string)                    {}

func testHandler(t *testing.T, db *store.DB) *EdgeHandler {
	t.Helper()
	return NewEdgeHandler(orders.NewManager(db, noopEmitter{}, "edge"))
}

// seedQueuedWithReason creates an order parked in an acquiring status carrying
// the reason Core sent, i.e. the state the Edge is legitimately in mid-wait.
func seedQueuedWithReason(t *testing.T, h *EdgeHandler, db *store.DB, uuid string) int64 {
	t.Helper()
	id, err := db.CreateOrder(uuid, protocol.OrderTypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	h.HandleOrderUpdate(nil, &protocol.OrderUpdate{
		OrderUUID:   uuid,
		Status:      string(protocol.StatusQueued),
		QueueReason: "Waiting for material: 76683-6TA0A.06",
		QueueCode:   "waiting_for_material",
	})
	o, _ := db.GetOrder(id)
	if o.QueueReason == "" {
		t.Fatalf("setup: queued push should have stored a reason, got empty")
	}
	return id
}

func TestHandleOrderUpdate_ClearsQueueReasonOnceMoving(t *testing.T) {
	t.Parallel()
	db := testHandlerDB(t)
	h := testHandler(t, db)

	id := seedQueuedWithReason(t, h, db, "uuid-clear")

	// Core dispatches it. No reason on the update — because there is nothing
	// left to explain, and because Core never sends a clear.
	h.HandleOrderUpdate(nil, &protocol.OrderUpdate{
		OrderUUID: "uuid-clear",
		Status:    string(protocol.StatusInTransit),
	})

	o, _ := db.GetOrder(id)
	if o.QueueReason != "" {
		t.Errorf("queue_reason = %q, want empty — a hold reason must not outlive the hold; "+
			"the operator board quotes this field verbatim", o.QueueReason)
	}
	if o.QueueCode != "" {
		t.Errorf("queue_code = %q, want empty — the code must clear with the sentence", o.QueueCode)
	}
}

// The opposite error: an order still genuinely waiting must keep its
// explanation. This is the case that makes "just trust the pushed value"
// unsafe, so it is pinned alongside the clear.
func TestHandleOrderUpdate_KeepsQueueReasonWhileStillWaiting(t *testing.T) {
	t.Parallel()
	db := testHandlerDB(t)
	h := testHandler(t, db)

	id := seedQueuedWithReason(t, h, db, "uuid-keep")

	// Core re-pushes the same acquiring status without restating the reason.
	h.HandleOrderUpdate(nil, &protocol.OrderUpdate{
		OrderUUID: "uuid-keep",
		Status:    string(protocol.StatusQueued),
	})

	o, _ := db.GetOrder(id)
	if o.QueueReason == "" {
		t.Error("queue_reason was cleared while the order is still queued — an order that " +
			"is genuinely waiting must keep its explanation")
	}
}

// sourcing is the other half of IsAcquiring, and the one that regressed at
// Springfield on 2026-07-31 when it was left out of a related predicate.
func TestHandleOrderUpdate_KeepsQueueReasonWhileSourcing(t *testing.T) {
	t.Parallel()
	db := testHandlerDB(t)
	h := testHandler(t, db)

	id := seedQueuedWithReason(t, h, db, "uuid-sourcing")

	h.HandleOrderUpdate(nil, &protocol.OrderUpdate{
		OrderUUID:   "uuid-sourcing",
		Status:      string(protocol.StatusSourcing),
		QueueReason: "Waiting for material: 76683-6TA0A.06",
		QueueCode:   "waiting_for_material",
	})

	o, _ := db.GetOrder(id)
	if o.QueueReason == "" {
		t.Error("queue_reason was cleared on a sourcing push — sourcing is an acquiring " +
			"status and still owes the operator an explanation")
	}
}

// The incident shape end to end: the reason is set while waiting, survives the
// wait, and is gone by the time the order reaches a terminal state — so a LATER
// changeover at the same node cannot inherit it.
func TestHandleOrderUpdate_ReasonGoneByTerminal(t *testing.T) {
	t.Parallel()
	db := testHandlerDB(t)
	h := testHandler(t, db)

	id := seedQueuedWithReason(t, h, db, "uuid-terminal")

	for _, status := range []protocol.Status{
		protocol.StatusInTransit,
		protocol.StatusConfirmed,
	} {
		h.HandleOrderUpdate(nil, &protocol.OrderUpdate{
			OrderUUID: "uuid-terminal",
			Status:    string(status),
		})
	}

	o, _ := db.GetOrder(id)
	if o.QueueReason != "" {
		t.Errorf("queue_reason = %q on a finished order — this is the exact row the "+
			"ALN_001 modal quoted during the next changeover", o.QueueReason)
	}
}
