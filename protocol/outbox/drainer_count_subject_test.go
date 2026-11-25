package outbox

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
)

// drainer_count_subject_test.go — how the drainer treats the rows whose
// failed publish spends no retry (neverDeadLetter, the must-arrive table's
// never-dead-letter rows) when a publish fails. The exact list is
// TestNeverDeadLetter_ExactMembership's.

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
// publish of any of the subjects below does not spend a retry, so the row can
// never reach MaxRetries and never dead-letters: it retries until the broker
// takes it. Every other subject still spends one. (It said "either count
// subject" when the list held two; the exact list is
// TestNeverDeadLetter_ExactMembership's.)
//
// Inverted pin: at base (TestPin_CountSubjectFailedPublishSpendsARetry) a count
// row's failure incremented its retries like any other, and the tenth
// dead-lettered it.
func TestCountSubjectFailedPublishKeepsItsBudget(t *testing.T) {
	t.Parallel()
	// The produce ingest is a count too: a full bin's manifest (S1b).
	for _, subject := range []string{protocol.SubjectBinUOPDelta, protocol.SubjectLinesideBucketLevel, protocol.TypeOrderIngest,
		"data." + protocol.SubjectBinPickedUp, "data." + protocol.SubjectBinLanded, protocol.TypeOrderRelease,
		protocol.SubjectHoldDropped} {
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
		{ID: 2, Payload: []byte("report"), MsgType: protocol.SubjectEdgeHeartbeat},
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

// neverDeadLetterWant is the drainer's never-dead-letter list: the base's
// (v0.18.0, d2686a1d) ten keys, then each key the must-arrive table has added
// since, named with its commit's reason. Each key is spelled as the outbox row's msg_type
// is written on the side that sends it: the Edge writes a data subject bare
// and an order message as its envelope type; Core writes a data subject as
// "data."+subject (store/messaging BuildDataEnvelope), a reply as
// "data.reply."+subject (messaging.CoreHandler) and an order message as its
// envelope type. The value names the sender. Literal strings, not the
// constants, so a respelling of either side fails here too.
var neverDeadLetterWant = map[string]string{
	"inventory.bin_uop_delta":         "Edge (uop accumulator)",
	"inventory.lineside_bucket_level": "Edge (uop accumulator)",
	"inventory.hold_dropped":          "Edge (orders manager, with its ingest)",
	"order.ingest":                    "Edge (order sender)",
	"order.release":                   "Edge (orders manager)",
	"order.cancel":                    "Edge (order sender)",
	"data.transit.bin_picked_up":      "Core (bin service, the lift)",
	"data.transit.bin_landed":         "Core (bin service, the landing)",
	"data.reply.edge.answer":          "Core (core handler, the ask's answer)",
	"order.update":                    "Core (the status tell, queueTellTx)",
	// Added by the must-arrive table: a lost refresh leaves the station counting
	// under an ended generation; a late one only moves a stamp forward.
	"data.inventory.bin_epoch_refresh": "Core (uop applier, the epoch repair)",
	// Added by the must-arrive table: a lost episode state is missing at Core until
	// the episode next changes; a late one loses Core's revision compare.
	"demand.origin": "Edge (demand episode)",
	// Added by the must-arrive table once every count Core states is fenced per
	// addressed station (C6): a lost declaration is a count the station never
	// hears; a late one is rebased on its stream.
	"data.inventory.uop_adjustment": "Core (bin service and manifest, the count declarations)",
	// Added by the must-arrive table (final brief 5i): a lost confirm left Core
	// reading delivered until its sweep, and for good on an order the sweep
	// skips; a late one confirms only a delivered order.
	"order.receipt": "Edge (orders manager)",
}

// msgTypeUniverse is every spelling a row's msg_type can take for a protocol
// name, on either side: every subject and envelope type bare, as
// "data."+name and as "data.reply."+name. The drainer is asked about each.
func msgTypeUniverse() []string {
	seen := map[string]bool{}
	var out []string
	names := append(append(append([]string{}, protocol.AllSubjects()...), protocol.AllTypes()...), protocol.EphemeralTypes()...)
	for _, n := range names {
		for _, k := range []string{n, "data." + n, "data.reply." + n} {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

// TestNeverDeadLetter_ExactMembership pins which outbox rows the drainer never
// dead-letters, by asking the drainer: one row per spelling in
// msgTypeUniverse, each one failed publish short of MaxRetries, one pass
// against a broker that refuses everything. A row that spends no retry is on
// the list. The set must be exactly neverDeadLetterWant: a key gained, a
// key lost, or a key respelled (a Core subject written bare, an Edge one with
// "data.", an order type with "data.") fails it.
//
// Pin 12 of the info-flow build's Step 0. The must-arrive table (lane B2)
// derives this list from one table and keeps this membership in its first
// commit; each later commit that changes membership changes the map above in
// the same commit and says why. A key outside msgTypeUniverse is invisible
// here, and also matches no row any sender writes.
func TestNeverDeadLetter_ExactMembership(t *testing.T) {
	t.Parallel()
	universe := msgTypeUniverse()
	for key := range neverDeadLetterWant {
		found := false
		for _, k := range universe {
			found = found || k == key
		}
		if !found {
			t.Fatalf("expected key %q is not a spelling of any protocol subject or type — fix the expectation", key)
		}
	}
	store := &mockStore{}
	byID := map[int64]string{}
	for i, key := range universe {
		id := int64(i + 1)
		byID[id] = key
		store.pending = append(store.pending, Message{ID: id, Payload: []byte(key), MsgType: key, Retries: MaxRetries - 1})
	}
	pub := &mockPublisher{connected: true, publishErr: errors.New("broker unreachable")}
	NewDrainer(store, pub, "orders", time.Hour, len(universe)).drain()

	spent := map[string]bool{}
	for _, id := range store.retried {
		spent[byID[id]] = true
	}
	if len(spent) == 0 {
		t.Fatal("no row spent a retry: the drain asked nothing, so this pin has no input")
	}
	var gained, lost []string
	for _, key := range universe {
		_, want := neverDeadLetterWant[key]
		got := !spent[key]
		switch {
		case got && !want:
			gained = append(gained, key)
		case !got && want:
			lost = append(lost, key)
		}
	}
	sort.Strings(gained)
	sort.Strings(lost)
	if len(gained) > 0 || len(lost) > 0 {
		t.Errorf("the never-dead-letter list moved: now also never dead-lettered %v; now dead-lettered after %d "+
			"failed publishes %v. If the change is meant, change neverDeadLetterWant in the same commit, "+
			"spelled as the sending side writes the row (Edge bare, Core \"data.\"+subject), and say why in the body",
			strings.Join(gained, ", "), MaxRetries, strings.Join(lost, ", "))
	}
}
