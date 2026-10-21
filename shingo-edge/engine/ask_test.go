package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/service"
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
		msgs, lerr := e.db.ListPendingOutbox(10)
		if lerr != nil {
			t.Fatalf("list outbox: %v", lerr)
		}
		if len(msgs) > 0 {
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

// TestAskOrRefuse_ACoreWithoutTheKindIsRefused: a kind the last ack did not
// name is refused before anything is sent (ErrCoreDoesNotAnswer, naming the
// kind), and nothing is put on the outbox.
func TestAskOrRefuse_ACoreWithoutTheKindIsRefused(t *testing.T) {
	t.Parallel()
	e, _ := binsEngine(t, testEngineDB(t))
	got, err := askOrRefuse[BinCountResponse](e, protocol.AskBinCount, protocol.BinCountRequest{})
	if got != nil || !errors.Is(err, ErrCoreDoesNotAnswer) || !strings.Contains(err.Error(), protocol.AskBinCount) {
		t.Errorf("a kind Core does not answer: %+v, %v, want ErrCoreDoesNotAnswer naming %s", got, err, protocol.AskBinCount)
	}
	if _, err := askLateOrRefuse(e, protocol.AskBinClear, protocol.BinClearRequest{}, func(*BinClearResponse, error) {
		t.Error("the follow-up ran for an ask never sent")
	}); !errors.Is(err, ErrCoreDoesNotAnswer) {
		t.Errorf("late form: %v, want ErrCoreDoesNotAnswer", err)
	}
	msgs, lerr := e.db.ListPendingOutbox(10)
	if lerr != nil {
		t.Fatalf("list outbox: %v", lerr)
	}
	if len(msgs) != 0 {
		t.Errorf("an ask went out to a Core that does not answer it: %d rows", len(msgs))
	}
}

// TestAskLateOrRefuse_TheFollowUpRunsOnALateAnswer: past the wait the tap
// hears ErrAskPending and its follow-up does not run; the answer, when it
// comes, runs it with the kind's shapes (the CLEAR's afterClear is such a
// follow-up).
func TestAskLateOrRefuse_TheFollowUpRunsOnALateAnswer(t *testing.T) {
	e, _ := binsEngine(t, testEngineDB(t))
	e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Asks: []string{protocol.AskBinClear}})
	var got *BinClearResponse
	var gotErr error
	runs := 0
	followUp := func(c *BinClearResponse, err error) { runs++; got, gotErr = c, err }
	// AskWait is 2 s; a tap that is answered after it pends.
	done := make(chan error, 1)
	go func() {
		_, err := askLateOrRefuse(e, protocol.AskBinClear, protocol.BinClearRequest{NodeName: "N1"}, followUp)
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

// TestPreflightAsker_TheReadAsk: the preflight gate asks Core through the ask
// pair (protocol.AskPreflight, a read-ask). The answer is the door's response:
// a Core that sends absent is told apart from one too old to; a refusal and an
// answer still pending past the wait are errors, never "all available" — the
// gate's "cannot check now" (the changeover start refuses on it, the composer
// shows unchecked). A Core that does not answer the kind is not available.
func TestPreflightAsker_TheReadAsk(t *testing.T) {
	t.Parallel()
	e, _ := binsEngine(t, testEngineDB(t))
	p := preflightAsker{e}
	if p.Available() {
		t.Fatal("a Core that does not answer preflight reads available")
	}
	e.setAskKinds([]string{protocol.AskPreflight})
	if !p.Available() {
		t.Fatal("a Core that answers preflight reads unavailable")
	}
	ask := func(answer func(env *protocol.Envelope)) (*service.PreflightCoreResult, error) {
		type out struct {
			r   *service.PreflightCoreResult
			err error
		}
		done := make(chan out, 1)
		go func() {
			r, err := p.PreflightInventory("st", []string{"PART-A", "PART-B"})
			done <- out{r, err}
		}()
		if answer != nil {
			var env *protocol.Envelope
			for deadline := time.Now().Add(3 * time.Second); env == nil; {
				msgs, lerr := e.db.ListPendingOutbox(10)
				if lerr != nil {
					t.Fatalf("list outbox: %v", lerr)
				}
				if len(msgs) > 0 {
					env, _ = askSent(t, e)
					_ = e.db.AckOutbox(msgs[len(msgs)-1].ID)
				} else if time.Now().After(deadline) {
					t.Fatal("the ask never reached the outbox")
				}
			}
			answer(env)
		}
		r := <-done
		return r.r, r.err
	}

	r, err := ask(func(env *protocol.Envelope) {
		e.HandleEdgeAnswer(answerTo(env), &protocol.EdgeAnswer{Kind: protocol.AskPreflight, Status: protocol.AnswerOK,
			Body: json.RawMessage(`{"missing":["PART-B"],"absent":["PART-B"],"available":[{"payload_code":"PART-A","bin_count":3}]}`)})
	})
	if err != nil || len(r.Missing) != 1 || r.Missing[0] != "PART-B" || !r.AbsentKnown || len(r.Absent) != 1 ||
		len(r.Available) != 1 || r.Available[0].BinCount != 3 {
		t.Errorf("answered: %+v, %v", r, err)
	}
	r, err = ask(func(env *protocol.Envelope) {
		e.HandleEdgeAnswer(answerTo(env), &protocol.EdgeAnswer{Kind: protocol.AskPreflight, Status: protocol.AnswerOK,
			Body: json.RawMessage(`{"missing":[],"available":[]}`)})
	})
	if err != nil || r.AbsentKnown {
		t.Errorf("a Core that sends no absent: %+v, %v, want absent not known", r, err)
	}
	if _, err := ask(func(env *protocol.Envelope) {
		e.HandleEdgeAnswer(answerTo(env), &protocol.EdgeAnswer{Kind: protocol.AskPreflight, Status: protocol.AnswerBad, Sentence: "no"})
	}); err == nil {
		t.Error("a refusal read as an answer")
	}
	if _, err := ask(nil); !errors.Is(err, ErrAskPending) {
		t.Errorf("no answer within the wait: err = %v, want ErrAskPending", err)
	}
}

// TestHeldPartials_TheReadAsk: a part's held partials come over the ask pair
// (protocol.AskHeldPartials). The answer is the door's list; a Core that does
// not answer the kind, a refusal and an answer still pending past the wait are
// errors — the finish-later picker's "cannot check now", the REQUEST going on
// without it.
func TestHeldPartials_TheReadAsk(t *testing.T) {
	t.Parallel()
	e, _ := binsEngine(t, testEngineDB(t))
	if _, err := e.HeldPartials("PART-H"); err == nil {
		t.Fatal("a Core that does not answer held_partials gave a list")
	}
	e.setAskKinds([]string{protocol.AskHeldPartials})
	ask := func(answer func(env *protocol.Envelope)) ([]protocol.HeldPartial, error) {
		type out struct {
			list []protocol.HeldPartial
			err  error
		}
		done := make(chan out, 1)
		go func() {
			l, err := e.HeldPartials("PART-H")
			done <- out{l, err}
		}()
		if answer != nil {
			var env *protocol.Envelope
			for deadline := time.Now().Add(3 * time.Second); env == nil; {
				msgs, lerr := e.db.ListPendingOutbox(10)
				if lerr != nil {
					t.Fatalf("list outbox: %v", lerr)
				}
				if len(msgs) > 0 {
					var a *protocol.EdgeAsk
					env, a = askSent(t, e)
					_ = e.db.AckOutbox(msgs[len(msgs)-1].ID)
					var req protocol.HeldPartialsRequest
					if err := json.Unmarshal(a.Body, &req); err != nil || a.Kind != protocol.AskHeldPartials || req.PayloadCode != "PART-H" {
						t.Errorf("ask = %s %s (%v)", a.Kind, a.Body, err)
					}
				} else if time.Now().After(deadline) {
					t.Fatal("the ask never reached the outbox")
				}
			}
			answer(env)
		}
		o := <-done
		return o.list, o.err
	}
	list, err := ask(func(env *protocol.Envelope) {
		e.HandleEdgeAnswer(answerTo(env), &protocol.EdgeAnswer{Kind: protocol.AskHeldPartials, Status: protocol.AnswerOK,
			Body: json.RawMessage(`[{"bin_id":41,"bin_label":"B-41"}]`)})
	})
	if err != nil || len(list) != 1 || list[0].BinID != 41 {
		t.Errorf("answered: %+v, %v", list, err)
	}
	if _, err := ask(func(env *protocol.Envelope) {
		e.HandleEdgeAnswer(answerTo(env), &protocol.EdgeAnswer{Kind: protocol.AskHeldPartials, Status: protocol.AnswerBad, Sentence: "payload is required"})
	}); err == nil {
		t.Error("a refusal read as a list")
	}
	if _, err := ask(nil); !errors.Is(err, ErrAskPending) {
		t.Errorf("no answer within the wait: err = %v, want ErrAskPending", err)
	}
}
