package outbox

import (
	"testing"
	"time"
)

// compressWakeSettle shrinks the settle window for the duration of a test.
func compressWakeSettle(t *testing.T, d time.Duration) {
	t.Helper()
	prev := wakeSettle
	wakeSettle = d
	t.Cleanup(func() { wakeSettle = prev })
}

func (m *mockStore) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listCalls
}

func (m *mockStore) didPurge() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.purged
}

// TestDrainer_NotifyDrainsBeforeTick is the regression guard for the doorbell.
// The loop used to be a bare ticker with nothing to tell it work had arrived,
// so a message waited a uniform 0-interval for its first send attempt —
// measured at Springfield on a 5s interval: mean 2.839s, p95 5.009s, with zero
// rows ever backlogged. A core->edge->core round trip paid it twice.
//
// The interval here is long enough that a tick cannot be what drains it.
func TestDrainer_NotifyDrainsBeforeTick(t *testing.T) {
	compressWakeSettle(t, 5*time.Millisecond)

	store := &mockStore{pending: []Message{{ID: 1, Topic: "orders", Payload: []byte("x")}}}
	pub := &mockPublisher{connected: true}

	d := NewDrainer(store, pub, "orders", time.Hour, 50)
	d.Start()
	defer d.Stop()

	d.Notify()

	deadline := time.After(2 * time.Second)
	for {
		pub.mu.Lock()
		n := len(pub.published)
		pub.mu.Unlock()
		if n > 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("Notify did not trigger a drain — with an hour-long interval the " +
				"only thing that can publish is the doorbell")
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// TestDrainer_NotifyBurstCoalesces pins the bound on drains per burst. A drain
// retries every pending row, so one drain per enqueue would burn the retry
// budget far faster than the tick ever did.
//
// The work here is done by the wake channel's capacity of 1, not by the settle
// window — measured, this burst yields a single drain even with wakeSettle at
// zero. So what this guards is that capacity: widening the channel, or
// spawning a drain per notification, would fail it.
func TestDrainer_NotifyBurstCoalesces(t *testing.T) {
	compressWakeSettle(t, 40*time.Millisecond)

	store := &mockStore{}
	pub := &mockPublisher{connected: true}

	d := NewDrainer(store, pub, "orders", time.Hour, 50)
	d.Start()
	defer d.Stop()

	for range 200 {
		d.Notify()
	}
	time.Sleep(150 * time.Millisecond)

	// An instantaneous burst yields the drain that answers the first wake plus
	// at most a couple for signals raised while it settled — never one each.
	if got := store.calls(); got > 5 {
		t.Fatalf("200 notifications produced %d drains, want a handful — the "+
			"settle window is not coalescing the burst", got)
	}
	if store.calls() == 0 {
		t.Fatal("burst produced no drain at all")
	}
}

// TestDrainer_WakeDoesNotAdvancePurgeCadence guards the coupling that made
// lowering the interval unsafe in the first place. Purge runs every
// PurgeCycleInterval CYCLES, not on a clock, so counting wake-driven drains
// would tie housekeeping frequency to message rate — a busy plant would purge
// constantly and an idle one never.
//
// The interval is long enough that no tick fires, so any purge here came from
// a wake.
func TestDrainer_WakeDoesNotAdvancePurgeCadence(t *testing.T) {
	compressWakeSettle(t, time.Millisecond)

	store := &mockStore{}
	pub := &mockPublisher{connected: true}

	d := NewDrainer(store, pub, "orders", time.Hour, 50)
	d.Start()
	defer d.Stop()

	// Well past PurgeCycleInterval drains.
	deadline := time.After(3 * time.Second)
	for store.calls() < PurgeCycleInterval*2 {
		d.Notify()
		select {
		case <-deadline:
			t.Fatalf("only reached %d drains, wanted %d — test could not establish "+
				"the condition", store.calls(), PurgeCycleInterval*2)
		case <-time.After(time.Millisecond):
		}
	}

	if store.didPurge() {
		t.Fatalf("purge ran after %d wake-driven drains — purge must ride ticker "+
			"cycles only, or its cadence becomes a function of message rate",
			store.calls())
	}
}
