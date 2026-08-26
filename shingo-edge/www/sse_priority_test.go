package www

import (
	"testing"
	"time"
)

// waitForType reads a client's queues until a frame of the named type arrives
// or the timeout elapses.
func waitForType(c *sseClient, typ string, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		select {
		case evt := <-c.events:
			if evt.Type == typ {
				return true
			}
		case evt := <-c.lossy:
			if evt.Type == typ {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// TestSSE_LossyFloodDoesNotEvictStateChanges is the regression guard for the
// queue split. Every topic used to share one 64-slot queue, so a client that
// fell behind lost whatever arrived next. counter-read and debug-log are the
// bulk of the traffic — measured at Springfield, 4,512 of 5,929 drops in 24h —
// so what they evicted was everything else, including order-update.
func TestSSE_LossyFloodDoesNotEvictStateChanges(t *testing.T) {
	h := NewEventHub()
	h.Start()
	defer h.Stop()

	c := &sseClient{
		events: make(chan SSEEvent, 64),
		lossy:  make(chan SSEEvent, 16),
	}
	h.register(c)
	defer h.unregister(c)

	// Well past both queues' capacity, with nothing reading.
	for i := range 500 {
		h.Broadcast(SSEEvent{Type: "debug-log", Data: i})
		h.Broadcast(SSEEvent{Type: "counter-read", Data: i})
		if i%16 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	time.Sleep(50 * time.Millisecond)

	h.Broadcast(SSEEvent{Type: "order-update", Data: "landed"})

	if !waitForType(c, "order-update", 2*time.Second) {
		t.Fatal("order-update was dropped after a diagnostic flood — the two " +
			"classes are sharing capacity again, so logging and PLC polling " +
			"can evict the frames the operator's board is edge-triggered on")
	}
}

// TestSSE_LossyDropsDoNotEvictClient pins the eviction rule. A client behind on
// diagnostics is not stuck, so shedding those frames must not count toward
// MaxConsecutiveDrops — otherwise a debug-log burst disconnects a healthy tab.
func TestSSE_LossyDropsDoNotEvictClient(t *testing.T) {
	h := NewEventHub()
	h.Start()
	defer h.Stop()

	c := &sseClient{
		events: make(chan SSEEvent, 64),
		lossy:  make(chan SSEEvent, 1),
	}
	h.register(c)
	defer h.unregister(c)

	for i := range MaxConsecutiveDrops * 10 {
		h.Broadcast(SSEEvent{Type: "debug-log", Data: i})
	}
	time.Sleep(100 * time.Millisecond)

	h.mu.RLock()
	_, stillRegistered := h.clients[c]
	drops := c.drops
	h.mu.RUnlock()

	if !stillRegistered {
		t.Fatal("client was evicted by a diagnostic flood — lossy drops must " +
			"not count toward the stuck-client threshold")
	}
	if drops != 0 {
		t.Fatalf("drops = %d, want 0 — only durable drops indicate a stuck client", drops)
	}
}

// TestSSE_PLCStatusStaysDurable guards the classification. plc-status is keyed
// by plcName and plc-health-alert is an alarm; neither may be shed as
// diagnostics just because the PLC feed is chatty.
func TestSSE_PLCStatusStaysDurable(t *testing.T) {
	for _, typ := range []string{"plc-status", "plc-health-alert", "counter-update", "order-update"} {
		if lossySSETopics[typ] {
			t.Errorf("%s is classified lossy — it carries state the operator or "+
				"engine acts on and must keep its reserved capacity", typ)
		}
	}
}
