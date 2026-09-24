package outbox

import (
	"errors"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
)

// drainer_count_subject_test.go — how the drainer treats the two count
// subjects (bin_uop_delta, lineside_bucket_delta) when a publish fails.

// scriptedPublisher fails a payload as many times as failures says, panics on
// panicOn, and records every attempt in order with its outcome.
type scriptedPublisher struct {
	mu       sync.Mutex
	failures map[string]int // payload -> failures left to hand out
	attempts []attempt
	panicOn  string
}

type attempt struct {
	payload string
	ok      bool
}

func (p *scriptedPublisher) Publish(_ string, payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.panicOn != "" && string(payload) == p.panicOn {
		panic("poison payload")
	}
	if p.failures[string(payload)] > 0 {
		p.failures[string(payload)]--
		p.attempts = append(p.attempts, attempt{payload: string(payload), ok: false})
		return errors.New("broker unreachable")
	}
	p.attempts = append(p.attempts, attempt{payload: string(payload), ok: true})
	return nil
}

func (p *scriptedPublisher) IsConnected() bool { return true }

// ackingStore is mockStore plus acks that remove the row, so a second pass
// sees only what is still pending — the shape the real stores have.
type ackingStore struct{ mockStore }

func (s *ackingStore) AckOutbox(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acked = append(s.acked, id)
	kept := s.pending[:0]
	for _, m := range s.pending {
		if m.ID != id {
			kept = append(kept, m)
		}
	}
	s.pending = kept
	return nil
}

// TestCountSubjectFailedPublishKeepsItsBudget is S1 (SYNTH-round2 §4). A failed
// publish of either count subject does not spend a retry, so the row can never
// reach MaxRetries and never dead-letters: it retries until the broker takes
// it. Every other subject still spends one.
//
// Inverted pin: at base (TestPin_CountSubjectFailedPublishSpendsARetry) a count
// row's failure incremented its retries like any other, and the tenth
// dead-lettered it.
func TestCountSubjectFailedPublishKeepsItsBudget(t *testing.T) {
	t.Parallel()
	for _, subject := range []string{protocol.SubjectBinUOPDelta, protocol.SubjectLinesideBucketDelta} {
		store := &mockStore{pending: []Message{
			{ID: 1, Payload: []byte("count"), MsgType: subject, Retries: MaxRetries - 1},
		}}
		pub := &mockPublisher{connected: true, publishErr: errors.New("broker unreachable")}
		d := NewDrainer(store, pub, "orders", time.Hour, 50)
		if !d.drain() {
			t.Fatalf("%s: drain reported no failure on a failed publish — muting depends on it", subject)
		}
		if len(store.retried) != 0 {
			t.Errorf("%s: retried = %v, want none — a count row that spends retries dead-letters "+
				"after %d, and the counts it carries are lost", subject, store.retried, MaxRetries)
		}
	}

	// Control: an ordinary subject keeps its budget.
	store := &mockStore{pending: []Message{
		{ID: 2, Payload: []byte("report"), MsgType: protocol.SubjectProductionReport},
	}}
	pub := &mockPublisher{connected: true, publishErr: errors.New("broker unreachable")}
	NewDrainer(store, pub, "orders", time.Hour, 50).drain()
	if len(store.retried) != 1 || store.retried[0] != 2 {
		t.Errorf("ordinary subject: retried = %v, want [2]", store.retried)
	}
}

// TestPin_CountSubjectPanicIsExhausted: the per-message panic boundary forces
// even a count subject into the dead-letter state. A payload that panics the
// publisher every time would otherwise loop forever. Stays green after S1: the
// panic path keeps MarkOutboxExhausted for every subject.
func TestPin_CountSubjectPanicIsExhausted(t *testing.T) {
	t.Parallel()
	store := &mockStore{pending: []Message{
		{ID: 7, Payload: []byte("poison"), MsgType: protocol.SubjectBinUOPDelta},
	}}
	pub := &scriptedPublisher{panicOn: "poison"}
	d := NewDrainer(store, pub, "orders", time.Hour, 50)
	d.drain()
	if len(store.exhausted) != 1 || store.exhausted[0].id != 7 {
		t.Fatalf("exhausted = %+v, want row 7 — a panicking count row must still be stood down", store.exhausted)
	}
	if len(store.retried) != 0 {
		t.Errorf("retried = %v, want none — the panic path marks exhausted, it does not count a retry", store.retried)
	}
}

// TestPin_P0c_PartialFailureReordersAScope pins the cause of V2: drain() keeps
// going past a failed row, so when row 1 of a scope fails once the broker sees
// row 2 before row 1. Publish order: 1 fails, 2 lands, and the next pass lands 1.
//
// This stays green after S1 and S5. It is a characterisation of the cause, not
// a verify-red: the running net (lane N) is what makes the reorder harmless.
func TestPin_P0c_PartialFailureReordersAScope(t *testing.T) {
	t.Parallel()
	store := &ackingStore{mockStore{pending: []Message{
		{ID: 1, Payload: []byte("seq-1"), MsgType: protocol.SubjectBinUOPDelta},
		{ID: 2, Payload: []byte("seq-2"), MsgType: protocol.SubjectBinUOPDelta},
	}}}
	pub := &scriptedPublisher{failures: map[string]int{"seq-1": 1}}
	d := NewDrainer(store, pub, "orders", time.Hour, 50)

	d.drain()
	d.drain()

	want := []attempt{{"seq-1", false}, {"seq-2", true}, {"seq-1", true}}
	if len(pub.attempts) != len(want) {
		t.Fatalf("attempts = %+v, want %+v", pub.attempts, want)
	}
	for i := range want {
		if pub.attempts[i] != want[i] {
			t.Fatalf("attempts = %+v, want %+v — the broker saw seq 2 land before seq 1",
				pub.attempts, want)
		}
	}
}
