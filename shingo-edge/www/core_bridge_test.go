package www

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
	"shingoedge/engine"
	"shingoedge/internal/askbridge"
)

// core_bridge_test.go — a www test's stub Core, reached the way the Edge
// reaches Core: by the ask pair (engine package core_bridge_test.go is the
// same bridge for the engine's tests).
//
// The stub answers the machine doors' paths; Core answers those acts as asks
// only, each through its door's one body, so the stub's answer to a path is
// Core's answer to the ask. bridgeAsks makes the last ack name those kinds and
// carries each such ask on eng's outbox to the stub, its reply handed back as
// the answer.

var bridgedPaths = askbridge.Paths

func bridgeAsks(t *testing.T, eng *engine.Engine, url string) {
	t.Helper()
	kinds := make([]string, 0, len(bridgedPaths))
	for k := range bridgedPaths {
		kinds = append(kinds, k)
	}
	eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Asks: kinds})
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
			if eng.PendingAsks() == 0 { // only while an ask is out
				continue
			}
			msgs, err := eng.DB().ListPendingOutbox(100)
			if err != nil {
				continue
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
				path, ok := bridgedPaths[ask.Kind]
				if !ok || eng.DB().AckOutbox(m.ID) != nil {
					continue
				}
				resp, err := client.Post(strings.TrimRight(url, "/")+path, "application/json", bytes.NewReader(ask.Body))
				if err != nil {
					continue
				}
				raw, rerr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if rerr != nil {
					continue
				}
				eng.HandleEdgeAnswer(&protocol.Envelope{Type: protocol.TypeData, CorID: env.ID}, stubAnswer(ask.Kind, resp.StatusCode, raw))
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-done })
}

// stubAnswer is the stub's reply as Core's answer (engine's bridgedAnswer).
func stubAnswer(kind string, status int, raw []byte) *protocol.EdgeAnswer {
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
	default:
		a.Sentence = fmt.Sprintf("core returned %d", status)
	}
	a.Rows = shape.Rows
	return a
}
