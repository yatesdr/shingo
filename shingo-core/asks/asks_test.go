package asks

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"shingo/protocol"
)

// fakeStore is the inbox and outbox the replay reads, in memory.
type fakeStore struct {
	seen     map[string]bool
	replies  map[string]int64
	replayed []int64
	outbox   map[int64]bool // ids still in the outbox
}

func newFakeStore() *fakeStore {
	return &fakeStore{seen: map[string]bool{}, replies: map[string]int64{}, outbox: map[int64]bool{}}
}

func (f *fakeStore) RecordInboundMessage(id, _, _ string) (bool, error) {
	if f.seen[id] {
		return false, nil
	}
	f.seen[id] = true
	return true, nil
}
func (f *fakeStore) RecordInboundReply(id string, outboxID int64) error {
	f.replies[id] = outboxID
	return nil
}
func (f *fakeStore) InboundReply(id string) (int64, bool, error) {
	o, ok := f.replies[id]
	return o, ok, nil
}
func (f *fakeStore) ReplayOutbox(id int64) (bool, error) {
	if !f.outbox[id] {
		return false, nil
	}
	f.replayed = append(f.replayed, id)
	return true, nil
}

// rig is a server whose answers are recorded with the outbox id each got.
type rig struct {
	s       *Server
	db      *fakeStore
	answers []*protocol.EdgeAnswer
	next    int64
}

func newRig() *rig {
	r := &rig{db: newFakeStore()}
	r.s = New(r.db, func(_ *protocol.Envelope, a *protocol.EdgeAnswer) (int64, error) {
		r.next++
		r.answers = append(r.answers, a)
		r.db.outbox[r.next] = true
		return r.next, nil
	})
	return r
}

func askEnv(id string) *protocol.Envelope {
	return &protocol.Envelope{ID: id, Type: protocol.TypeData, Src: protocol.Address{Role: protocol.RoleEdge, Station: "plant-a.line-1"}}
}

// TestServer_RoutesByKindAndAnswers: a registered kind's door answers with its
// status, sentence, body and rows, the asking station in hand; an unregistered
// kind is answered as not answered, and nothing runs.
func TestServer_RoutesByKindAndAnswers(t *testing.T) {
	t.Parallel()
	r := newRig()
	var gotStation string
	var gotBody string
	r.s.Register(protocol.AskBinCount, func(station string, body json.RawMessage) Result {
		gotStation, gotBody = station, string(body)
		return Result{Status: protocol.AnswerRefused, Sentence: "no", Body: map[string]int{"n": 1},
			Rows: []protocol.NodeBinsRow{{Node: "SMN_A1", State: protocol.BinsStateEmpty}}}
	}, false)

	r.s.Handle(askEnv("e1"), &protocol.EdgeAsk{Kind: protocol.AskBinCount, Body: json.RawMessage(`{"node_name":"SMN_A1"}`)})
	if gotStation != "plant-a.line-1" || gotBody != `{"node_name":"SMN_A1"}` {
		t.Errorf("door saw station %q body %q", gotStation, gotBody)
	}
	if len(r.answers) != 1 {
		t.Fatalf("answers = %d, want 1", len(r.answers))
	}
	a := r.answers[0]
	if a.Kind != protocol.AskBinCount || a.Status != protocol.AnswerRefused || a.Sentence != "no" ||
		string(a.Body) != `{"n":1}` || len(a.Rows) != 1 || a.Rows[0].Node != "SMN_A1" {
		t.Errorf("answer = %+v", a)
	}

	r.s.Handle(askEnv("e2"), &protocol.EdgeAsk{Kind: "no.such.kind"})
	if len(r.answers) != 2 || r.answers[1].Status != protocol.AnswerUnknownKind || r.answers[1].Kind != "no.such.kind" {
		t.Errorf("unknown kind answered %+v", r.answers[len(r.answers)-1])
	}
	if got := r.s.Kinds(); len(got) != 1 || got[0] != protocol.AskBinCount {
		t.Errorf("Kinds = %v", got)
	}
}

// TestServer_ReplayAnswersARepeatWithTheFirstAnswer: a replay kind acts once;
// the same envelope again re-sends its first answer and acts on nothing. A
// repeat whose first answer was never recorded is acted on again (the door's
// own checks refuse what it can tell was done), and so is one whose answer row
// is gone.
func TestServer_ReplayAnswersARepeatWithTheFirstAnswer(t *testing.T) {
	t.Parallel()
	r := newRig()
	acts := 0
	r.s.Register(protocol.AskClearHome, func(string, json.RawMessage) Result {
		acts++
		return Result{Status: protocol.AnswerOK}
	}, true)
	ask := &protocol.EdgeAsk{Kind: protocol.AskClearHome}

	r.s.Handle(askEnv("c1"), ask)
	r.s.Handle(askEnv("c1"), ask)
	if acts != 1 || len(r.answers) != 1 || len(r.db.replayed) != 1 || r.db.replayed[0] != 1 {
		t.Fatalf("a repeat: acts %d answers %d replayed %v, want 1 1 [1]", acts, len(r.answers), r.db.replayed)
	}

	r.db.seen["c2"] = true // seen, no answer recorded
	r.s.Handle(askEnv("c2"), ask)
	if acts != 2 {
		t.Errorf("a repeat with no recorded answer: acts %d, want 2", acts)
	}

	r.db.seen["c3"], r.db.replies["c3"] = true, 99 // answer row purged
	r.s.Handle(askEnv("c3"), ask)
	if acts != 3 {
		t.Errorf("a repeat whose answer row is gone: acts %d, want 3", acts)
	}
}

// expiredAsk is an expired edge.ask envelope, as the ingestor hands it over.
func expiredAsk(t *testing.T, id, kind string) *protocol.Envelope {
	t.Helper()
	body, err := json.Marshal(protocol.EdgeAsk{Kind: kind})
	if err != nil {
		t.Fatalf("marshal ask: %v", err)
	}
	payload, err := json.Marshal(protocol.Data{Subject: protocol.SubjectEdgeAsk, Body: body})
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	env := askEnv(id)
	env.Payload, env.ExpiresAt = payload, time.Now().Add(-time.Minute)
	return env
}

// TestServer_AnswerExpired: an ask that arrived expired is answered 408 and
// nothing runs; another subject's expired message is left alone; a late
// repeat of a replay kind already answered gets the first answer again, never
// "nothing was done".
func TestServer_AnswerExpired(t *testing.T) {
	t.Parallel()
	r := newRig()
	acts := 0
	door := func(string, json.RawMessage) Result { acts++; return Result{Status: protocol.AnswerOK} }
	r.s.Register(protocol.AskBinLoad, door, false)
	r.s.Register(protocol.AskClearHome, door, true)

	r.s.AnswerExpired(expiredAsk(t, "x1", protocol.AskBinLoad))
	if acts != 0 || len(r.answers) != 1 || r.answers[0].Status != protocol.AnswerExpired || r.answers[0].Kind != protocol.AskBinLoad {
		t.Fatalf("expired ask: acts %d answers %+v", acts, r.answers)
	}

	other := askEnv("x2")
	payload, err := json.Marshal(protocol.Data{Subject: protocol.SubjectBinUOPDelta, Body: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	other.Payload = payload
	r.s.AnswerExpired(other)
	if len(r.answers) != 1 {
		t.Errorf("an expired delta was answered: %+v", r.answers)
	}

	r.s.Handle(askEnv("x3"), &protocol.EdgeAsk{Kind: protocol.AskClearHome})
	answered := len(r.answers)
	r.s.AnswerExpired(expiredAsk(t, "x3", protocol.AskClearHome))
	if len(r.answers) != answered || len(r.db.replayed) != 1 {
		t.Errorf("a late repeat of an answered clear: answers %d -> %d, replayed %v; want its first answer re-sent",
			answered, len(r.answers), r.db.replayed)
	}
}

// TestServer_AnAnswerThatCannotBeSentRecordsNothing: a failed send is logged and
// leaves no recorded reply, so a repeat is acted on again rather than "replayed"
// into nothing.
func TestServer_AnAnswerThatCannotBeSentRecordsNothing(t *testing.T) {
	t.Parallel()
	db := newFakeStore()
	s := New(db, func(*protocol.Envelope, *protocol.EdgeAnswer) (int64, error) { return 0, errors.New("outbox down") })
	s.Register(protocol.AskClearHome, func(string, json.RawMessage) Result { return Result{Status: protocol.AnswerOK} }, true)
	s.Handle(askEnv("f1"), &protocol.EdgeAsk{Kind: protocol.AskClearHome})
	if _, ok := db.replies["f1"]; ok {
		t.Error("a reply was recorded for an answer that was never enqueued")
	}
}
