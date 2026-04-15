package countgroup

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/config"
)

// fakePLC is a minimal in-memory TagReadWriter implementation for tests.
// It records all read/write calls and returns programmed values.
type fakePLC struct {
	mu sync.Mutex

	tags   map[string]map[string]interface{}
	writes []writeCall

	// Optional error injection.
	readErr  error
	writeErr error
}

type writeCall struct {
	PLC, Tag string
	Value    interface{}
}

func newFakePLC() *fakePLC {
	return &fakePLC{tags: make(map[string]map[string]interface{})}
}

func (f *fakePLC) setTag(plcName, tag string, v interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tags[plcName] == nil {
		f.tags[plcName] = make(map[string]interface{})
	}
	f.tags[plcName][tag] = v
}

func (f *fakePLC) writesFor(plcName, tag string) []writeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []writeCall
	for _, w := range f.writes {
		if w.PLC == plcName && w.Tag == tag {
			out = append(out, w)
		}
	}
	return out
}

func (f *fakePLC) ReadTagValue(ctx context.Context, plcName, tagName string) (interface{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return nil, f.readErr
	}
	if m, ok := f.tags[plcName]; ok {
		return m[tagName], nil
	}
	return nil, nil
}

func (f *fakePLC) WriteTagValue(ctx context.Context, plcName, tagName string, value interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, writeCall{PLC: plcName, Tag: tagName, Value: value})
	if f.writeErr != nil {
		return f.writeErr
	}
	if f.tags[plcName] == nil {
		f.tags[plcName] = make(map[string]interface{})
	}
	f.tags[plcName][tagName] = value
	return nil
}

// recordingAckSender returns an AckSender func that captures every ack
// for later inspection.
func recordingAckSender() (AckSender, func() []*protocol.CountGroupAck) {
	var mu sync.Mutex
	var acks []*protocol.CountGroupAck
	send := func(ack *protocol.CountGroupAck) error {
		mu.Lock()
		acks = append(acks, ack)
		mu.Unlock()
		return nil
	}
	snapshot := func() []*protocol.CountGroupAck {
		mu.Lock()
		defer mu.Unlock()
		return append([]*protocol.CountGroupAck(nil), acks...)
	}
	return send, snapshot
}

// helper to build a configured handler with one binding.
func newTestHandler(p *fakePLC, sender AckSender) (*Handler, config.CountGroupsConfig) {
	cfg := config.CountGroupsConfig{
		HeartbeatInterval: 20 * time.Millisecond,
		HeartbeatTag:      "Heartbeat",
		HeartbeatPLC:      "PLC1",
		AckWarn:           100 * time.Millisecond,
		AckDead:           300 * time.Millisecond,
		Codes:             map[string]int{"on": 1, "off": 2},
		Bindings: map[string]config.Binding{
			"Z1": {PLC: "PLC1", RequestTag: "Z1_REQ"},
		},
	}
	h := New(cfg, p, sender, nil)
	return h, cfg
}

func TestHandlerBootstrapClearsStaleTag(t *testing.T) {
	p := newFakePLC()
	p.setTag("PLC1", "Z1_REQ", 5) // stale non-zero value left by prior run

	h, _ := newTestHandler(p, nil)
	h.OnCommand(protocol.CountGroupCommand{
		CorrelationID: "c1",
		Group:         "Z1",
		Desired:       "on",
		Timestamp:     time.Now(),
	})

	writes := p.writesFor("PLC1", "Z1_REQ")
	if len(writes) < 2 {
		t.Fatalf("expected bootstrap clear + action write, got %d writes", len(writes))
	}
	if writes[0].Value != 0 {
		t.Fatalf("first write should be the bootstrap zero, got %v", writes[0].Value)
	}
	if writes[1].Value != 1 {
		t.Fatalf("second write should be the on action code (1), got %v", writes[1].Value)
	}
}

func TestHandlerUnboundGroupLogsWarnAndReturns(t *testing.T) {
	p := newFakePLC()
	h, _ := newTestHandler(p, nil)
	h.OnCommand(protocol.CountGroupCommand{
		CorrelationID: "c1",
		Group:         "NotInConfig",
		Desired:       "on",
	})
	if n := len(p.writes); n != 0 {
		t.Fatalf("unbound group should not write any tags, got %d writes", n)
	}
}

func TestHandlerUnknownDesiredStateReturns(t *testing.T) {
	p := newFakePLC()
	h, _ := newTestHandler(p, nil)
	h.OnCommand(protocol.CountGroupCommand{
		CorrelationID: "c1",
		Group:         "Z1",
		Desired:       "sparkle", // not in codes map
	})
	if n := len(p.writes); n != 0 {
		t.Fatalf("unknown desired should not write, got %d writes", n)
	}
}

func TestHandlerWarlinkWriteErrorSendsErrorAck(t *testing.T) {
	p := newFakePLC()
	p.writeErr = errors.New("warlink down")
	send, acks := recordingAckSender()
	h, _ := newTestHandler(p, send)
	h.OnCommand(protocol.CountGroupCommand{
		CorrelationID: "c1",
		Group:         "Z1",
		Desired:       "on",
	})

	got := acks()
	if len(got) != 1 {
		t.Fatalf("expected 1 error ack, got %d", len(got))
	}
	if got[0].Outcome != protocol.AckOutcomeWarlinkErr {
		t.Fatalf("expected outcome=%s, got %s", protocol.AckOutcomeWarlinkErr, got[0].Outcome)
	}
}

func TestStartedGuardSuppressesHeartbeat(t *testing.T) {
	p := newFakePLC()
	h, _ := newTestHandler(p, nil)

	hb := NewHeartbeatWriter(h, nil)
	hb.Start()
	defer hb.Stop()

	// Before MarkStarted: heartbeat writer should NOT write to the tag.
	time.Sleep(80 * time.Millisecond)
	writes := p.writesFor("PLC1", "Heartbeat")
	if len(writes) != 0 {
		t.Fatalf("heartbeat wrote %d times before MarkStarted; started guard broken", len(writes))
	}

	h.MarkStarted()
	time.Sleep(80 * time.Millisecond)
	writes = p.writesFor("PLC1", "Heartbeat")
	if len(writes) == 0 {
		t.Fatalf("heartbeat wrote 0 times after MarkStarted; expected monotonic writes")
	}
}

func TestAckTimeoutSendsTimeoutAckAndAbandons(t *testing.T) {
	p := newFakePLC()
	// PLC never clears the tag — ack never arrives. OnCommand writes 1,
	// fake PLC holds it, ack-poll times out after AckDead (300ms).
	send, acks := recordingAckSender()
	h, _ := newTestHandler(p, send)
	h.MarkStarted() // allow ack-polling

	hb := NewHeartbeatWriter(h, nil)
	hb.Start()
	defer hb.Stop()

	h.OnCommand(protocol.CountGroupCommand{
		CorrelationID: "c1",
		Group:         "Z1",
		Desired:       "on",
		Timestamp:     time.Now(),
	})

	// Wait past AckDead (300ms). Heartbeat ticker is 20ms so it should
	// cycle checkAcks many times.
	deadline := time.Now().Add(800 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, a := range acks() {
			if a.Outcome == protocol.AckOutcomeTimeout {
				// Verify abandoned: no longer in inFlight.
				h.inFlightMu.Lock()
				_, stillTracked := h.inFlight["Z1"]
				h.inFlightMu.Unlock()
				if stillTracked {
					t.Fatalf("group Z1 still in-flight after timeout ack")
				}
				return // success
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("did not receive timeout ack within deadline; got %+v", acks())
}
