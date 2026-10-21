package engine

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"shingo/protocol"
)

// askSent is the ask the engine last put on its outbox, decoded.
func askSent(t *testing.T, e *Engine) (*protocol.Envelope, *protocol.EdgeAsk) {
	t.Helper()
	msgs, err := e.db.ListPendingOutbox(100)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].MsgType != protocol.SubjectEdgeAsk {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal(msgs[i].Payload, &env); err != nil {
			t.Fatalf("decode ask envelope: %v", err)
		}
		var data protocol.Data
		if err := json.Unmarshal(env.Payload, &data); err != nil {
			t.Fatalf("decode ask data: %v", err)
		}
		var ask protocol.EdgeAsk
		if err := json.Unmarshal(data.Body, &ask); err != nil {
			t.Fatalf("decode ask: %v", err)
		}
		return &env, &ask
	}
	t.Fatal("no ask on the outbox")
	return nil, nil
}

// answerTo is Core's reply to the ask in env.
func answerTo(env *protocol.Envelope) *protocol.Envelope {
	return &protocol.Envelope{Type: protocol.TypeData, CorID: env.ID}
}

// TestAsk_AnsweredInTime: an ask rides the outbox as an edge.ask with its kind
// and body, expiring after AskTTL; Core's answer, matched by CorID, comes back
// to the tap already applied (its rows written into the bins copy), and the
// ask is no longer pending.
func TestAsk_AnsweredInTime(t *testing.T) {
	t.Parallel()
	e, _ := binsEngine(t, testEngineDB(t))
	holdGroupRow(t, e, "GRP", "GRP.S1")
	type out struct {
		a   *protocol.EdgeAnswer
		err error
	}
	done := make(chan out, 1)
	go func() {
		a, err := e.Ask(protocol.AskBinCount, protocol.BinCountRequest{NodeName: "GRP.S1", ActualUOP: 7}, 5*time.Second)
		done <- out{a, err}
	}()
	var env *protocol.Envelope
	var ask *protocol.EdgeAsk
	for deadline := time.Now().Add(3 * time.Second); env == nil; {
		if msgs, _ := e.db.ListPendingOutbox(10); len(msgs) > 0 {
			env, ask = askSent(t, e)
		} else if time.Now().After(deadline) {
			t.Fatal("the ask never reached the outbox")
		}
	}
	var body protocol.BinCountRequest
	if err := json.Unmarshal(ask.Body, &body); err != nil || ask.Kind != protocol.AskBinCount ||
		body.NodeName != "GRP.S1" || body.ActualUOP != 7 {
		t.Errorf("ask = %s %s (%v)", ask.Kind, ask.Body, err)
	}
	if got := env.ExpiresAt.Sub(env.Timestamp); got != protocol.AskTTL {
		t.Errorf("ask lifetime = %v, want %v", got, protocol.AskTTL)
	}
	e.HandleEdgeAnswer(answerTo(env), &protocol.EdgeAnswer{Kind: protocol.AskBinCount, Status: protocol.AnswerRefused,
		Sentence: "no", Rows: []protocol.NodeBinsRow{{Node: "GRP.S1", State: protocol.BinsStateOccupied, Occupied: true, BinID: 9}}})
	r := <-done
	if r.err != nil || r.a == nil || r.a.Sentence != "no" {
		t.Fatalf("Ask = %+v, %v", r.a, r.err)
	}
	if got := e.boardBinID("GRP.S1"); got != 9 {
		t.Errorf("the answer's row was not written before the tap got it: bin %d, want 9", got)
	}
	if n := e.PendingAsks(); n != 0 {
		t.Errorf("pending asks = %d after the answer, want 0", n)
	}
}

// TestAsk_AnswerAfterTheWaitIsStillApplied: past the wait the tap hears
// ErrAskPending; the late answer is applied all the same (rows and the kind's
// applier), as is an answer to an ask this Edge never held (sent before a
// restart).
func TestAsk_AnswerAfterTheWaitIsStillApplied(t *testing.T) {
	t.Parallel()
	e, _ := binsEngine(t, testEngineDB(t))
	holdGroupRow(t, e, "GRP", "GRP.S1")
	applied := 0
	e.askAppliers = map[string]askApplier{protocol.AskBinClear: func(*Engine, *protocol.EdgeAnswer) { applied++ }}

	if _, err := e.Ask(protocol.AskBinClear, protocol.BinClearRequest{NodeName: "GRP.S1"}, 10*time.Millisecond); !errors.Is(err, ErrAskPending) {
		t.Fatalf("Ask past its wait = %v, want ErrAskPending", err)
	}
	env, _ := askSent(t, e)
	e.HandleEdgeAnswer(answerTo(env), &protocol.EdgeAnswer{Kind: protocol.AskBinClear, Status: protocol.AnswerOK,
		Rows: []protocol.NodeBinsRow{{Node: "GRP.S1", State: protocol.BinsStateOccupied, Occupied: true, BinID: 5}}})
	if applied != 1 || e.boardBinID("GRP.S1") != 5 {
		t.Errorf("late answer: applier ran %d, bin %d; want 1 and 5", applied, e.boardBinID("GRP.S1"))
	}
	e.HandleEdgeAnswer(&protocol.Envelope{CorID: "from-before-a-restart"}, &protocol.EdgeAnswer{Kind: protocol.AskBinClear,
		Status: protocol.AnswerOK, Rows: []protocol.NodeBinsRow{{Node: "GRP.S1", State: protocol.BinsStateEmpty}}})
	if applied != 2 || e.boardBinID("GRP.S1") != 0 {
		t.Errorf("an answer to an ask this Edge never held: applier ran %d, bin %d; want 2 and 0", applied, e.boardBinID("GRP.S1"))
	}
}

// TestAsk_OnlyWhatCoreAnswers: the kinds the last ack named are the kinds an
// Edge may ask; an ack without the field (an older Core) names none.
func TestAsk_OnlyWhatCoreAnswers(t *testing.T) {
	t.Parallel()
	e, _ := binsEngine(t, testEngineDB(t))
	if e.CoreAnswers(protocol.AskBinCount) {
		t.Error("before any ack: Core answers bin.count")
	}
	e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Asks: []string{protocol.AskBinCount}})
	if !e.CoreAnswers(protocol.AskBinCount) || e.CoreAnswers(protocol.AskBinLoad) {
		t.Error("after an ack naming bin.count only: wrong kinds")
	}
	e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}})
	if e.CoreAnswers(protocol.AskBinCount) {
		t.Error("after an ack from a Core without the pair: still asks bin.count")
	}
}

// TestAsk_AnUnansweredAskIsForgotten: an ask never answered leaves the pending
// map once its expiry and the slack have passed.
func TestAsk_AnUnansweredAskIsForgotten(t *testing.T) {
	t.Parallel()
	e, _ := binsEngine(t, testEngineDB(t))
	if _, err := e.Ask(protocol.AskBinCount, protocol.BinCountRequest{NodeName: "X"}, time.Millisecond); !errors.Is(err, ErrAskPending) {
		t.Fatalf("Ask = %v", err)
	}
	if n := e.PendingAsks(); n != 1 {
		t.Fatalf("pending = %d, want 1", n)
	}
	e.asks.mu.Lock()
	for _, p := range e.asks.pending {
		p.expires = time.Now().Add(-time.Second)
	}
	e.asks.mu.Unlock()
	if n := e.PendingAsks(); n != 0 {
		t.Errorf("pending = %d past the forget time, want 0", n)
	}
}

// TestAskInto_TheDoorsReturnShapes: an answer becomes what the kind's HTTP door
// returned — the response decoded when Core did the act, a *CoreRefusal with
// the status, sentence, rows and a count's epoch refresh when it did not.
func TestAskInto_TheDoorsReturnShapes(t *testing.T) {
	t.Parallel()
	ok, err := askInto[BinCountResponse](&protocol.EdgeAnswer{Kind: protocol.AskBinCount, Status: protocol.AnswerOK,
		Body: json.RawMessage(`{"status":"ok","bin_id":4,"uop_remaining":12}`)})
	if err != nil || ok.BinID != 4 || ok.UOPRemaining != 12 {
		t.Errorf("accept = %+v, %v", ok, err)
	}
	_, err = askInto[BinCountResponse](&protocol.EdgeAnswer{Kind: protocol.AskBinCount, Status: protocol.AnswerRefused,
		Sentence: "changed", Rows: []protocol.NodeBinsRow{{Node: "N1"}},
		Body: json.RawMessage(`{"error":"changed","epoch_refresh":{"bin_id":4,"core_node_name":"N1","epoch":9}}`)})
	var r *CoreRefusal
	if !errors.As(err, &r) || r.Status != protocol.AnswerRefused || r.Sentence != "changed" || len(r.Rows) != 1 ||
		r.EpochRefresh == nil || r.EpochRefresh.Epoch != 9 {
		t.Errorf("refusal = %+v (%v)", r, err)
	}
}

// TestAskOrDoor_TheDoorForACoreWithoutTheKind: a kind the last ack did not
// name goes through its HTTP door, and nothing is put on the outbox.
func TestAskOrDoor_TheDoorForACoreWithoutTheKind(t *testing.T) {
	t.Parallel()
	e, _ := binsEngine(t, testEngineDB(t))
	called := 0
	got, err := askOrDoor(e, protocol.AskBinCount, protocol.BinCountRequest{}, func() (*BinCountResponse, error) {
		called++
		return &BinCountResponse{BinID: 1}, nil
	})
	if err != nil || called != 1 || got.BinID != 1 {
		t.Errorf("door: called %d, got %+v, %v", called, got, err)
	}
	if msgs, _ := e.db.ListPendingOutbox(10); len(msgs) != 0 {
		t.Errorf("an ask went out to a Core that does not answer it: %d rows", len(msgs))
	}
}

// TestAskOrDoorLate_TheFollowUpRunsOnALateAnswer: past the wait the tap hears
// ErrAskPending and its follow-up does not run; the answer, when it comes, runs
// it with the door's shapes (the CLEAR's afterClear is such a follow-up).
func TestAskOrDoorLate_TheFollowUpRunsOnALateAnswer(t *testing.T) {
	e, _ := binsEngine(t, testEngineDB(t))
	e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Asks: []string{protocol.AskBinClear}})
	var got *BinClearResponse
	var gotErr error
	runs := 0
	followUp := func(c *BinClearResponse, err error) { runs++; got, gotErr = c, err }
	// AskWait is 2 s; a tap that is answered after it pends.
	done := make(chan error, 1)
	go func() {
		_, err := askOrDoorLate(e, protocol.AskBinClear, protocol.BinClearRequest{NodeName: "N1"},
			func() (*BinClearResponse, error) {
				t.Error("the door was used for a kind Core answers")
				return nil, nil
			}, followUp)
		done <- err
	}()
	if err := <-done; !errors.Is(err, ErrAskPending) {
		t.Fatalf("unanswered within the wait: %v, want ErrAskPending", err)
	}
	if runs != 0 {
		t.Fatalf("the follow-up ran before any answer")
	}
	env, _ := askSent(t, e)
	e.HandleEdgeAnswer(answerTo(env), &protocol.EdgeAnswer{Kind: protocol.AskBinClear, Status: protocol.AnswerOK,
		Body: json.RawMessage(`{"bin_id":3,"delta_epoch":8,"status":"ok"}`)})
	if runs != 1 || gotErr != nil || got == nil || got.BinID != 3 || got.DeltaEpoch != 8 {
		t.Errorf("late follow-up: runs %d got %+v err %v", runs, got, gotErr)
	}
}
