package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
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
func bridgeCore(t *testing.T, eng *Engine, url string) {
	t.Helper()
	if url == "" {
		return
	}
	url = strings.TrimRight(url, "/")
	eng.asks.mu.Lock()
	if eng.asks.kinds == nil {
		eng.asks.kinds = map[string]bool{}
	}
	for k := range bridgedKinds {
		eng.asks.kinds[k] = true
	}
	eng.asks.mu.Unlock()
	eng.SetPayloadBinTypes(eng.PayloadBinTypes())
	client := &http.Client{Timeout: 5 * time.Second}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			// Only while an ask is out: a poll that read the outbox on every
			// tick would be counted by the tests that count statements.
			if eng.PendingAsks() > 0 {
				bridgeAsks(t, eng, client, url)
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-done })
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
// each bridged ask stays out.
func coreNeverAnswers(t *testing.T, eng *Engine) {
	t.Helper()
	bridgeCore(t, eng, testCoreURL)
}
