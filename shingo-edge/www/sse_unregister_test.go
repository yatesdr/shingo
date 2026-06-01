package www

import "testing"

// R69-1: unregister can fire twice for the same client — run() evicts a stuck
// client (closing its channel) and then HandleSSE's deferred unregister also
// runs when its read sees the closed channel. close() is not idempotent, so the
// second call must be a no-op rather than panicking with "close of closed
// channel".
func TestUnregister_Idempotent_NoDoubleClosePanic(t *testing.T) {
	h := NewEventHub()
	c := &sseClient{events: make(chan SSEEvent, 1)}
	h.register(c)

	h.unregister(c)
	h.unregister(c) // second unregister must not panic on the already-closed channel
}
