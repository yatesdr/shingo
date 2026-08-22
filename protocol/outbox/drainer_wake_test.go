package outbox

import (
	"errors"
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

// retryCount reports how many times IncrementOutboxRetries fired for one row.
func (m *mockStore) retryCount(id int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, got := range m.retried {
		if got == id {
			n++
		}
	}
	return n
}

// setPublishErr swaps the publisher's failure mode mid-test.
func (m *mockPublisher) setPublishErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publishErr = err
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

// TestDrainer_WakeDoesNotRetryFailedRows is the regression guard for the
// doorbell's effect on the RETRY budget, which is separate from its effect on
// latency and was missed when the doorbell landed.
//
// drain() retries every pending row, so a wake-driven drain is a retry attempt
// for every message already in the backlog. Under a transport failure that
// turns each enqueue into one of the message's MaxRetries attempts, and a
// message that used to survive MaxRetries x interval (~50s at 5s) dies in a
// fraction of that. Measured at Springfield's ~0.27 enqueues/s the cost was
// ~2.5x; at the settle window's limit it is ~0.5s.
//
// The notifications here are spaced past the settle so the wake channel's
// capacity of 1 does NOT coalesce them — that is the whole point. Every one is
// a distinct opportunity to burn a retry, and the interval is an hour so no
// tick can be responsible for anything observed.
func TestDrainer_WakeDoesNotRetryFailedRows(t *testing.T) {
	compressWakeSettle(t, time.Millisecond)

	store := &mockStore{pending: []Message{{ID: 1, Topic: "orders", Payload: []byte("x")}}}
	pub := &mockPublisher{connected: true, publishErr: errors.New("kafka down")}

	d := NewDrainer(store, pub, "orders", time.Hour, 50)
	d.Start()
	defer d.Stop()

	for range 200 {
		d.Notify()
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)

	// One failing drain is expected — the wake that arrives before anything has
	// failed yet. Every wake after it must find the arm muted.
	if got := store.retryCount(1); got > 1 {
		t.Fatalf("row 1 advanced to %d retries on wakes alone, want at most 1 — a "+
			"publish failure must mute the wake arm, or an outage burns MaxRetries "+
			"at the enqueue rate instead of the drain interval", got)
	}
}

// TestDrainer_MuteClearsOnSuccessfulTick pins the other half: muting is not
// permanent. Once a TICKER drain completes with no failure the doorbell is
// armed again, so the latency win returns as soon as the broker does.
//
// Only a ticker drain clears it. A wake-driven success must not, or a broker
// that accepts one publish in ten would let the multiplier re-arm between
// failures — which is the partial-failure case the mute exists for.
func TestDrainer_MuteClearsOnSuccessfulTick(t *testing.T) {
	compressWakeSettle(t, time.Millisecond)

	const interval = 30 * time.Millisecond
	store := &mockStore{pending: []Message{{ID: 1, Topic: "orders", Payload: []byte("x")}}}
	pub := &mockPublisher{connected: true, publishErr: errors.New("kafka down")}

	d := NewDrainer(store, pub, "orders", interval, 50)
	d.Start()
	defer d.Stop()

	// Establish the muted state.
	for range 20 {
		d.Notify()
		time.Sleep(time.Millisecond)
	}

	// Broker recovers; let a ticker drain succeed and clear the mute.
	pub.setPublishErr(nil)
	time.Sleep(3 * interval)

	// The doorbell should be live again: a burst of spaced wakes now drives far
	// more drains than the ticker alone could in the same window.
	before := store.calls()
	deadline := time.After(60 * time.Millisecond)
	for {
		select {
		case <-deadline:
			got := store.calls() - before
			// ~2 ticks fit in 60ms; wakes should add many more than that.
			if got < 5 {
				t.Fatalf("only %d drains in 60ms after recovery, want >=5 — the wake "+
					"arm did not re-arm, so the doorbell stays dead until restart", got)
			}
			return
		case <-time.After(2 * time.Millisecond):
			d.Notify()
		}
	}
}

// TestDrainer_PurgeUsesSeparateRetentions pins that the two cutoffs stay apart.
//
// They shared a 24h window, and it cost evidence twice in one week: an
// investigation found a clean outbox and concluded Springfield had never
// retried a message — it had, the proof had been purged — and two dead-lettered
// production deltas were ~50 minutes from deletion before anyone knew they
// existed. A dead letter is the ONLY surviving record of a destroyed message,
// where a delivered row is just a receipt, so they cannot share a lifetime.
func TestDrainer_PurgeUsesSeparateRetentions(t *testing.T) {
	compressWakeSettle(t, time.Millisecond)

	store := &mockStore{}
	pub := &mockPublisher{connected: true}

	d := NewDrainer(store, pub, "orders", time.Millisecond, 50)
	d.Start()
	defer d.Stop()

	deadline := time.After(3 * time.Second)
	for !store.didPurge() {
		select {
		case <-deadline:
			t.Fatal("no purge observed — the test could not establish its condition")
		case <-time.After(2 * time.Millisecond):
		}
	}

	store.mu.Lock()
	gotDelivered, gotDead := store.purgeDelivered, store.purgeDeadLetter
	store.mu.Unlock()

	if gotDelivered != MessageRetentionPeriod {
		t.Errorf("delivered cutoff = %v, want %v", gotDelivered, MessageRetentionPeriod)
	}
	if gotDead != DeadLetterRetentionPeriod {
		t.Errorf("dead-letter cutoff = %v, want %v", gotDead, DeadLetterRetentionPeriod)
	}
	if gotDead <= gotDelivered {
		t.Errorf("dead letters (%v) are kept no longer than delivered rows (%v) — "+
			"the whole point is that the destroyed message outlives the receipt",
			gotDead, gotDelivered)
	}
}
