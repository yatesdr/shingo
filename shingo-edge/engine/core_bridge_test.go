package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingoedge/internal/askbridge"
)

// core_bridge_test.go — a test's stub Core, reached the way the Edge reaches
// Core: by the ask pair.
//
// Many engine tests stand a stub Core up as an httptest server answering the
// machine doors' paths (bin-load, bin-count, bin-clear, bin-quality-hold,
// clear-home). Core answers those acts as asks only, each through its door's
// one body (shingo-core www.Doors; door_ask_agreement_test pins the ask and
// the body to the bytes), so the stub's answer to a path is Core's answer to
// the ask of that kind. bridgeCore makes eng's Core answer those kinds and
// carries each ask to the stub: the ask's body as the request's, the stub's
// status, sentence, body and rows as the answer's. A stub that cannot be
// reached answers nothing: the ask stays out (ErrAskPending), as against a
// Core that never answered.

// bridgedKinds are the kinds the stubs answer, with the path each is posted to.
var bridgedKinds = askbridge.Paths

// bridgeCore makes eng's Core the stub at url for the test's life: Core has
// been heard from (its catalog arrived, the press-index changeover's gate),
// the last ack names the bridged kinds, and each such ask is answered by the
// stub. An empty url is no Core: nothing is bridged and those kinds are
// refused.
//
// THE ANSWER COMES IN THE ASKING GOROUTINE, NOT FROM A POLL. Engine.ask puts
// the ask's row on the outbox and then takes its wait from the ask book's wait
// clock (EnqueueOutbox, then waitOn.After, in ask.go). The bridge is that
// clock (askBridge): its After carries the outbox's bridged asks to the stub,
// then hands back the wait of the clock it wraps, so the answer is in before
// the tap waits. Until 2026-11-23 each bridge was a goroutine that woke every
// 2 ms for its test's life to look. A clock a test sets with setAskClock goes
// under the bridge, not in its place.
func bridgeCore(t *testing.T, eng *Engine, url string) {
	t.Helper()
	if url == "" {
		return
	}
	installAskBridge(t, eng, strings.TrimRight(url, "/"))
}

// installAskBridge is bridgeCore's work: the bridged kinds answered, and the
// bridge wrapped round the ask book's wait clock. An empty url is a Core that
// takes each bridged ask and answers none (coreNeverAnswers).
func installAskBridge(t *testing.T, eng *Engine, url string) {
	t.Helper()
	eng.asks.mu.Lock()
	if eng.asks.kinds == nil {
		eng.asks.kinds = map[string]bool{}
	}
	for k := range bridgedKinds {
		eng.asks.kinds[k] = true
	}
	b := &askBridge{t: t, eng: eng, client: &http.Client{Timeout: 5 * time.Second}, url: url,
		inner: eng.asks.waitClock}
	eng.asks.waitClock = b
	eng.asks.mu.Unlock()
	// The test's end ends the bridge, as it stopped the poll: an ask after it
	// waits on inner alone.
	t.Cleanup(func() { b.done.Store(true) })
	eng.SetPayloadBinTypes(eng.PayloadBinTypes())
}

// askBridge is the ask book's wait clock while a stub Core is bridged (see
// bridgeCore): After carries the bridged asks on the outbox to the stub, then
// waits on inner, the clock the taps waited on before (nil: the wall clock).
// A second bridge on one engine wraps the first, so the newest answers first.
type askBridge struct {
	t      *testing.T
	eng    *Engine
	client *http.Client
	url    string
	mu     sync.Mutex // guards inner: setAskClock writes it beside a tap's wait
	inner  clock.Clock
	done   atomic.Bool // the test has ended: answer nothing more
}

func (b *askBridge) waits() clock.Clock {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inner == nil {
		return clock.Real()
	}
	return b.inner
}

func (b *askBridge) setInner(c clock.Clock) {
	b.mu.Lock()
	b.inner = c
	b.mu.Unlock()
}

func (b *askBridge) Now() time.Time                         { return b.waits().Now() }
func (b *askBridge) NewTicker(d time.Duration) clock.Ticker { return b.waits().NewTicker(d) }

// After answers the bridged asks on the outbox, then waits d on inner.
func (b *askBridge) After(d time.Duration) <-chan time.Time {
	if !b.done.Load() {
		bridgeAsks(b.t, b.eng, b.client, b.url)
	}
	return b.waits().After(d)
}

// bridgeAsks carries every bridged ask on eng's outbox to the stub once.
func bridgeAsks(t *testing.T, eng *Engine, client *http.Client, url string) {
	msgs, err := eng.db.ListPendingOutbox(100)
	if err != nil {
		return // the test's db is closing
	}
	for _, m := range msgs {
		if m.MsgType != protocol.SubjectEdgeAsk {
			continue
		}
		var env protocol.Envelope
		var data protocol.Data
		var ask protocol.EdgeAsk
		if json.Unmarshal(m.Payload, &env) != nil || json.Unmarshal(env.Payload, &data) != nil ||
			json.Unmarshal(data.Body, &ask) != nil {
			continue
		}
		path, ok := bridgedKinds[ask.Kind]
		if !ok {
			continue
		}
		if err := eng.db.AckOutbox(m.ID); err != nil {
			t.Errorf("take the %s ask off the outbox: %v", ask.Kind, err)
			continue
		}
		if url == "" {
			continue // no Core answers (coreNeverAnswers)
		}
		resp, err := client.Post(url+path, "application/json", bytes.NewReader(ask.Body))
		if err != nil {
			continue // the stub is not there: Core never answers
		}
		raw, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			continue
		}
		eng.HandleEdgeAnswer(answerTo(&env), bridgedAnswer(ask.Kind, resp.StatusCode, raw))
	}
}

// bridgedAnswer is the stub's reply as Core's answer: its status, its body,
// and for a refusal (a non-200, or a 200 whose body says status "error") the
// sentence (detail, else error, else the status) and the rows.
func bridgedAnswer(kind string, status int, raw []byte) *protocol.EdgeAnswer {
	a := &protocol.EdgeAnswer{Kind: kind, Status: status}
	if json.Valid(raw) {
		a.Body = json.RawMessage(bytes.TrimSpace(raw))
	}
	var shape struct {
		Status string                 `json:"status"`
		Detail string                 `json:"detail"`
		Error  string                 `json:"error"`
		Rows   []protocol.NodeBinsRow `json:"rows"`
	}
	_ = json.Unmarshal(raw, &shape) // a body that is no object answers with its status alone
	if status == http.StatusOK && shape.Status == "error" {
		a.Status = protocol.AnswerRefused
	}
	if a.Status == http.StatusOK {
		return a
	}
	switch {
	case shape.Detail != "":
		a.Sentence = shape.Detail
	case shape.Error != "":
		a.Sentence = shape.Error
	case len(raw) > 0 && !json.Valid(raw):
		a.Sentence = strings.TrimSpace(string(raw))
	default:
		a.Sentence = fmt.Sprintf("core returned %d", status)
	}
	a.Rows = shape.Rows
	return a
}

// coreNeverAnswers is a Core this Edge has heard from that answers no ask:
// each bridged ask is taken off the outbox and stays out. It was a bridge to
// http://127.0.0.1:1 (testCoreURL, whose comment said it "refuses at once on
// every OS"): on Windows a refused connect takes about 2 s, which the poll spent
// beside the tap's wait and a bridge in the tap's goroutine would add to it, so
// this bridge posts nothing.
func coreNeverAnswers(t *testing.T, eng *Engine) {
	t.Helper()
	installAskBridge(t, eng, "")
}
