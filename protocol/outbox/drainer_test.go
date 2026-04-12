package outbox

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// mockStore implements Store for testing.
type mockStore struct {
	mu       sync.Mutex
	pending  []Message
	acked    []int64
	retried  []int64
	purged   bool
	listErr  error
}

func (m *mockStore) ListPendingOutbox(limit int) ([]Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	n := len(m.pending)
	if n > limit {
		n = limit
	}
	result := make([]Message, n)
	copy(result, m.pending[:n])
	return result, nil
}

func (m *mockStore) AckOutbox(id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acked = append(m.acked, id)
	return nil
}

func (m *mockStore) IncrementOutboxRetries(id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retried = append(m.retried, id)
	return nil
}

func (m *mockStore) PurgeOldOutbox(olderThan time.Duration) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.purged = true
	return 5, nil
}

// mockPublisher implements Publisher for testing.
type mockPublisher struct {
	mu        sync.Mutex
	connected bool
	published []publishedMsg
	publishErr error
}

type publishedMsg struct {
	topic   string
	payload []byte
}

func (m *mockPublisher) Publish(topic string, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.publishErr != nil {
		return m.publishErr
	}
	m.published = append(m.published, publishedMsg{topic: topic, payload: payload})
	return nil
}

func (m *mockPublisher) IsConnected() bool { return m.connected }

func TestDrainer_DrainCycle(t *testing.T) {
	store := &mockStore{
		pending: []Message{
			{ID: 1, Topic: "orders", Payload: []byte("hello"), MsgType: "test", Retries: 0},
			{ID: 2, Topic: "orders", Payload: []byte("world"), MsgType: "test", Retries: 0},
		},
	}
	pub := &mockPublisher{connected: true}

	d := NewDrainer(store, pub, "fallback", 10*time.Millisecond, 50)
	d.drain()

	if len(pub.published) != 2 {
		t.Fatalf("published %d messages, want 2", len(pub.published))
	}
	if len(store.acked) != 2 {
		t.Fatalf("acked %d messages, want 2", len(store.acked))
	}
	if pub.published[0].topic != "orders" {
		t.Errorf("topic = %q, want orders", pub.published[0].topic)
	}
}

func TestDrainer_FallbackTopic(t *testing.T) {
	store := &mockStore{
		pending: []Message{
			{ID: 1, Payload: []byte("hello"), MsgType: "test", Retries: 0},
		},
	}
	pub := &mockPublisher{connected: true}

	d := NewDrainer(store, pub, "default-topic", 10*time.Millisecond, 50)
	d.drain()

	if len(pub.published) != 1 {
		t.Fatal("expected 1 publish")
	}
	if pub.published[0].topic != "default-topic" {
		t.Errorf("topic = %q, want default-topic", pub.published[0].topic)
	}
}

func TestDrainer_SkipsWhenDisconnected(t *testing.T) {
	store := &mockStore{
		pending: []Message{
			{ID: 1, Payload: []byte("hello"), MsgType: "test", Retries: 0},
		},
	}
	pub := &mockPublisher{connected: false}

	d := NewDrainer(store, pub, "orders", 10*time.Millisecond, 50)
	d.drain()

	if len(pub.published) != 0 {
		t.Error("should not publish when disconnected")
	}
	if len(store.acked) != 0 {
		t.Error("should not ack when disconnected")
	}
}

func TestDrainer_RetryOnPublishFail(t *testing.T) {
	store := &mockStore{
		pending: []Message{
			{ID: 1, Payload: []byte("hello"), MsgType: "test", Retries: 3},
		},
	}
	pub := &mockPublisher{connected: true, publishErr: errors.New("kafka down")}

	d := NewDrainer(store, pub, "orders", 10*time.Millisecond, 50)
	d.drain()

	if len(store.retried) != 1 {
		t.Fatal("expected 1 retry")
	}
	if store.retried[0] != 1 {
		t.Errorf("retried msg ID = %d, want 1", store.retried[0])
	}
	if len(store.acked) != 0 {
		t.Error("should not ack failed message")
	}
}

func TestDrainer_DeadLetter(t *testing.T) {
	store := &mockStore{
		pending: []Message{
			{ID: 1, Payload: []byte("hello"), MsgType: "test", Retries: MaxRetries - 1},
		},
	}
	pub := &mockPublisher{connected: true, publishErr: errors.New("kafka down")}

	d := NewDrainer(store, pub, "orders", 10*time.Millisecond, 50)
	d.drain()

	// Should increment retries (which marks it as dead-lettered in DB)
	if len(store.retried) != 1 {
		t.Fatal("expected 1 retry increment")
	}
}

func TestDrainer_ListPendingError(t *testing.T) {
	store := &mockStore{listErr: errors.New("db error")}
	pub := &mockPublisher{connected: true}

	d := NewDrainer(store, pub, "orders", 10*time.Millisecond, 50)
	d.drain() // should not panic

	if len(pub.published) != 0 {
		t.Error("should not publish on list error")
	}
}

func TestDrainer_PurgeOld(t *testing.T) {
	store := &mockStore{}
	pub := &mockPublisher{connected: true}

	d := NewDrainer(store, pub, "orders", 1*time.Millisecond, 50)
	d.Start()
	defer d.Stop()

	// Wait for ~100 cycles to trigger purge (100 * 1ms = ~100ms)
	time.Sleep(200 * time.Millisecond)

	store.mu.Lock()
	defer store.mu.Unlock()
	if !store.purged {
		t.Error("expected purge to be called")
	}
}

func TestDrainer_Stop(t *testing.T) {
	store := &mockStore{}
	pub := &mockPublisher{connected: true}

	d := NewDrainer(store, pub, "orders", 1*time.Millisecond, 50)
	d.Start()

	// Stop should return without hanging
	done := make(chan struct{})
	go func() {
		d.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop hung")
	}
}
