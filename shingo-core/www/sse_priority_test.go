package www

import (
	"fmt"
	"testing"
	"time"
)

// floodCoalescing emits n frames on a coalescing topic, pacing often enough
// that the hub's 256-slot intake keeps up and the flood is actually delivered
// to the client rather than shed at Broadcast.
func floodCoalescing(hub *EventHub, n int) {
	for i := range n {
		hub.Broadcast("robot-update", fmt.Sprintf(`{"seq":%d}`, i))
		if i%32 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	time.Sleep(50 * time.Millisecond)
}

// TestSSE_CoalescingFloodDoesNotEvictStateChanges is the regression guard for
// the queue split. Both classes used to share one 64-slot channel, so a client
// that fell behind lost whatever arrived next — and because robot-update is
// ~97% of all frames, what arrived next was overwhelmingly a state change.
// A dropped order-update leaves the orders board silently stale: it refreshes
// on that event (hx-trigger sse:order-update) and never polls as a backstop.
func TestSSE_CoalescingFloodDoesNotEvictStateChanges(t *testing.T) {
	hub := NewEventHub()
	hub.Start()
	defer hub.Stop()

	c := hub.AddClient()
	defer hub.RemoveClient(c)

	// Well past the durable queue's capacity, with nothing reading.
	floodCoalescing(hub, 500)

	hub.Broadcast("order-update", `{"type":"failed","order_id":42}`)

	if !waitForFrame(c, "order-update", `"order_id":42`, 2*time.Second) {
		t.Fatal("order-update was dropped after a coalescing flood — the two " +
			"classes are sharing capacity again, so telemetry can evict the " +
			"frames the operator's board is edge-triggered on")
	}
}

// TestSSE_CoalescingTopicKeepsOnlyNewest pins the other half: a superseded
// snapshot is discarded rather than queued, so a slow client never works
// through a backlog of stale robot positions to reach the current one.
func TestSSE_CoalescingTopicKeepsOnlyNewest(t *testing.T) {
	hub := NewEventHub()
	hub.Start()
	defer hub.Stop()

	c := hub.AddClient()
	defer hub.RemoveClient(c)

	floodCoalescing(hub, 200)

	pending := c.takeCoalesced()
	if len(pending) != 1 {
		t.Fatalf("got %d pending coalesced frames, want 1 — frames on a "+
			"coalescing topic must overwrite rather than accumulate", len(pending))
	}
	if pending[0].Event != "robot-update" {
		t.Fatalf("pending frame is %q, want robot-update", pending[0].Event)
	}
}

// TestSSE_StateChangesAreNotCoalesced guards the classification itself. Two
// order-updates are two distinct facts; collapsing them would lose one.
func TestSSE_StateChangesAreNotCoalesced(t *testing.T) {
	hub := NewEventHub()
	hub.Start()
	defer hub.Stop()

	c := hub.AddClient()
	defer hub.RemoveClient(c)

	hub.Broadcast("order-update", `{"order_id":1}`)
	hub.Broadcast("order-update", `{"order_id":2}`)

	for _, want := range []string{`"order_id":1`, `"order_id":2`} {
		if !waitForFrame(c, "order-update", want, 2*time.Second) {
			t.Fatalf("order-update %s never arrived — a non-snapshot topic was "+
				"coalesced, discarding a distinct state change", want)
		}
	}
}
