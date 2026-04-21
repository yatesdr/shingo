//go:build docker

package messaging

import (
	"testing"
	"time"

	"shingo/protocol/outbox"
	"shingocore/config"
)

// newOutboxTestClient returns an unconnected *Client. NewOutboxDrainer only
// wires the client's DebugLog and uses it later as a Publisher — actual
// Kafka I/O is not exercised by these tests.
func newOutboxTestClient() *Client {
	return NewClient(&config.MessagingConfig{
		Kafka:         config.KafkaConfig{Brokers: []string{"localhost:9092"}},
		OrdersTopic:   "shingo.orders",
		DispatchTopic: "shingo.dispatch",
	})
}

// TestNewOutboxDrainer_ReturnsNonNilWithDebugLog verifies that
// NewOutboxDrainer wires up an adapter + the client's DebugLog into the
// returned *outbox.Drainer. A nil DebugLog on the client must still
// produce a usable drainer (DebugLogFunc is nil-safe).
func TestNewOutboxDrainer_ReturnsNonNilWithDebugLog(t *testing.T) {
	db := testDB(t)
	client := newOutboxTestClient()

	logged := 0
	client.DebugLog = func(format string, args ...any) { logged++ }

	drainer := NewOutboxDrainer(db, client, 10*time.Millisecond)
	if drainer == nil {
		t.Fatal("NewOutboxDrainer returned nil")
	}
	if drainer.DebugLog == nil {
		t.Fatal("drainer.DebugLog should be wired from client.DebugLog")
	}

	// The wrapped DebugLog should forward to the client's callback.
	drainer.DebugLog.Log("hello %s", "world")
	if logged != 1 {
		t.Errorf("client.DebugLog calls = %d, want 1", logged)
	}
}

// TestNewOutboxDrainer_NilClientDebugLogIsSafe ensures that even when the
// client has no DebugLog set, the drainer's DebugLog does not panic when
// invoked (DebugLogFunc.Log is nil-safe by contract).
func TestNewOutboxDrainer_NilClientDebugLogIsSafe(t *testing.T) {
	db := testDB(t)
	client := newOutboxTestClient() // DebugLog is nil

	drainer := NewOutboxDrainer(db, client, 10*time.Millisecond)
	if drainer == nil {
		t.Fatal("NewOutboxDrainer returned nil")
	}
	// Must not panic.
	drainer.DebugLog.Log("no-op %d", 42)
}

// TestCoreOutboxStore_ListPendingOutbox_ConvertsFields confirms the
// adapter copies every field (ID, Topic, Payload, MsgType, Retries) from
// store.OutboxMessage onto outbox.Message. A regression in the field
// mapping would silently drop payloads at the drainer boundary.
func TestCoreOutboxStore_ListPendingOutbox_ConvertsFields(t *testing.T) {
	db := testDB(t)
	adapter := &coreOutboxStore{db: db}

	if err := db.EnqueueOutbox("shingo.dispatch", []byte(`{"a":1}`), "order.ack", "line-1"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := db.EnqueueOutbox("other.topic", []byte(`{"b":2}`), "order.update", "line-2"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	msgs, err := adapter.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2", len(msgs))
	}

	// Assert the adapter returns []outbox.Message (compile-time check) and
	// that each row's fields survive the conversion.
	var _ []outbox.Message = msgs

	if msgs[0].Topic != "shingo.dispatch" {
		t.Errorf("msgs[0].Topic = %q, want %q", msgs[0].Topic, "shingo.dispatch")
	}
	if msgs[0].MsgType != "order.ack" {
		t.Errorf("msgs[0].MsgType = %q, want %q", msgs[0].MsgType, "order.ack")
	}
	if string(msgs[0].Payload) != `{"a":1}` {
		t.Errorf("msgs[0].Payload = %q, want %q", msgs[0].Payload, `{"a":1}`)
	}
	if msgs[0].Retries != 0 {
		t.Errorf("msgs[0].Retries = %d, want 0", msgs[0].Retries)
	}
	if msgs[0].ID <= 0 {
		t.Errorf("msgs[0].ID = %d, want > 0", msgs[0].ID)
	}

	if msgs[1].Topic != "other.topic" {
		t.Errorf("msgs[1].Topic = %q, want %q", msgs[1].Topic, "other.topic")
	}
	if string(msgs[1].Payload) != `{"b":2}` {
		t.Errorf("msgs[1].Payload = %q, want %q", msgs[1].Payload, `{"b":2}`)
	}
}

// TestCoreOutboxStore_ListPendingOutbox_EmptyTable covers the empty-table
// edge: the adapter must return a non-nil zero-length slice and no error.
func TestCoreOutboxStore_ListPendingOutbox_EmptyTable(t *testing.T) {
	db := testDB(t)
	adapter := &coreOutboxStore{db: db}

	msgs, err := adapter.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("len = %d, want 0", len(msgs))
	}
	// The adapter uses make(..., len(dbMsgs)), so the result is a
	// zero-length slice rather than nil. Assert that shape.
	if msgs == nil {
		t.Error("msgs = nil, want zero-length slice")
	}
}

// TestCoreOutboxStore_ListPendingOutbox_HonoursLimit makes sure the limit
// is forwarded through to the underlying store query.
func TestCoreOutboxStore_ListPendingOutbox_HonoursLimit(t *testing.T) {
	db := testDB(t)
	adapter := &coreOutboxStore{db: db}

	for i := 0; i < 5; i++ {
		if err := db.EnqueueOutbox("shingo.dispatch", []byte(`{}`), "order.ack", "line-1"); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	msgs, err := adapter.ListPendingOutbox(3)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("len = %d, want 3 (limit)", len(msgs))
	}
}

// TestCoreOutboxStore_AckOutbox_RemovesFromPending confirms ack marks the
// message as sent so it no longer shows up in ListPendingOutbox.
func TestCoreOutboxStore_AckOutbox_RemovesFromPending(t *testing.T) {
	db := testDB(t)
	adapter := &coreOutboxStore{db: db}

	if err := db.EnqueueOutbox("shingo.dispatch", []byte(`{}`), "order.ack", "line-1"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := db.EnqueueOutbox("shingo.dispatch", []byte(`{}`), "order.ack", "line-1"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	msgs, err := adapter.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("pre-ack pending = %d, want 2", len(msgs))
	}

	if err := adapter.AckOutbox(msgs[0].ID); err != nil {
		t.Fatalf("ack: %v", err)
	}

	after, err := adapter.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list pending after ack: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("post-ack pending = %d, want 1", len(after))
	}
	if after[0].ID == msgs[0].ID {
		t.Errorf("acked ID %d still appears in pending", msgs[0].ID)
	}
}

// TestCoreOutboxStore_IncrementOutboxRetries_BumpsRetries verifies retries
// increments by exactly one per call (not reset, not doubled).
func TestCoreOutboxStore_IncrementOutboxRetries_BumpsRetries(t *testing.T) {
	db := testDB(t)
	adapter := &coreOutboxStore{db: db}

	if err := db.EnqueueOutbox("shingo.dispatch", []byte(`{}`), "order.ack", "line-1"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	initial, err := adapter.ListPendingOutbox(1)
	if err != nil || len(initial) != 1 {
		t.Fatalf("list pending: len=%d err=%v", len(initial), err)
	}

	if err := adapter.IncrementOutboxRetries(initial[0].ID); err != nil {
		t.Fatalf("increment 1: %v", err)
	}
	if err := adapter.IncrementOutboxRetries(initial[0].ID); err != nil {
		t.Fatalf("increment 2: %v", err)
	}
	if err := adapter.IncrementOutboxRetries(initial[0].ID); err != nil {
		t.Fatalf("increment 3: %v", err)
	}

	after, err := adapter.ListPendingOutbox(1)
	if err != nil || len(after) != 1 {
		t.Fatalf("list pending after: len=%d err=%v", len(after), err)
	}
	if after[0].Retries != 3 {
		t.Errorf("retries = %d, want 3", after[0].Retries)
	}
	if after[0].ID != initial[0].ID {
		t.Errorf("ID changed: %d -> %d", initial[0].ID, after[0].ID)
	}
}

// TestCoreOutboxStore_PurgeOldOutbox_RemovesSentMessages exercises the purge
// adapter and checks the returned int count matches what the underlying
// store purged. The int64->int cast in the adapter is load-bearing — a
// regression would silently truncate the count.
func TestCoreOutboxStore_PurgeOldOutbox_RemovesSentMessages(t *testing.T) {
	db := testDB(t)
	adapter := &coreOutboxStore{db: db}

	// Two messages: one acked, one pending.
	if err := db.EnqueueOutbox("shingo.dispatch", []byte(`{}`), "order.ack", "line-1"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := db.EnqueueOutbox("shingo.dispatch", []byte(`{}`), "order.ack", "line-1"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	pending, err := adapter.ListPendingOutbox(10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("list pending: len=%d err=%v", len(pending), err)
	}
	if err := adapter.AckOutbox(pending[0].ID); err != nil {
		t.Fatalf("ack: %v", err)
	}

	// The underlying store formats the cutoff to second precision, so
	// using a negative duration (cutoff = now + margin) is the most
	// reliable way to ensure sent_at < cutoff without a >1s sleep.
	n, err := adapter.PurgeOldOutbox(-1 * time.Minute)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Errorf("purged count = %d, want 1", n)
	}

	// The remaining (unacked) message should still be pending.
	remaining, err := adapter.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list pending after purge: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("remaining pending = %d, want 1", len(remaining))
	}
	if remaining[0].ID != pending[1].ID {
		t.Errorf("wrong message purged: remaining ID = %d, want %d", remaining[0].ID, pending[1].ID)
	}
}

// TestCoreOutboxStore_PurgeOldOutbox_PreservesRecentSends verifies that a
// large retention window leaves recent sends untouched. This is the
// complement to the "purge removes" test above: the int conversion path
// must also handle n=0.
func TestCoreOutboxStore_PurgeOldOutbox_PreservesRecentSends(t *testing.T) {
	db := testDB(t)
	adapter := &coreOutboxStore{db: db}

	if err := db.EnqueueOutbox("shingo.dispatch", []byte(`{}`), "order.ack", "line-1"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	pending, err := adapter.ListPendingOutbox(10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("list pending: len=%d err=%v", len(pending), err)
	}
	if err := adapter.AckOutbox(pending[0].ID); err != nil {
		t.Fatalf("ack: %v", err)
	}

	n, err := adapter.PurgeOldOutbox(24 * time.Hour)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 0 {
		t.Errorf("purged count = %d, want 0 (retention window covers send)", n)
	}
}

// TestCoreOutboxStore_SatisfiesOutboxStoreInterface is a compile-time check
// that coreOutboxStore implements outbox.Store. If outbox.Store grows a new
// method or renames one, this test will fail to compile — drawing attention
// to the adapter instead of silently falling back on implicit conformance.
func TestCoreOutboxStore_SatisfiesOutboxStoreInterface(t *testing.T) {
	var _ outbox.Store = (*coreOutboxStore)(nil)
}

// TestNewOutboxDrainer_DrainIsNoOpWhenPublisherNotConnected confirms the
// drainer does not crash and does not ack anything when the Client has
// never been connected. This is the cold-start / degraded-Kafka path —
// the whole point of the outbox is to survive exactly this state.
func TestNewOutboxDrainer_DrainIsNoOpWhenPublisherNotConnected(t *testing.T) {
	db := testDB(t)
	client := newOutboxTestClient() // never Connect()'d
	adapter := &coreOutboxStore{db: db}

	if err := db.EnqueueOutbox("shingo.dispatch", []byte(`{}`), "order.ack", "line-1"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Start the drainer briefly. Because client.IsConnected() is false,
	// drain() should short-circuit without touching the table.
	drainer := NewOutboxDrainer(db, client, 2*time.Millisecond)
	drainer.Start()
	time.Sleep(20 * time.Millisecond)
	drainer.Stop()

	// Pending row should still be there, untouched.
	pending, err := adapter.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1 (drain should not run while disconnected)", len(pending))
	}
	if pending[0].Retries != 0 {
		t.Errorf("retries = %d, want 0 (drain should not run while disconnected)", pending[0].Retries)
	}
}
