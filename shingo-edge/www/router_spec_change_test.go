package www

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

// router_spec_change_test.go — the spec-change coalescer carries process ids.
//
// The plant-claims publish is per process, so the coalescer between the edit
// handlers and the publisher has to remember WHICH processes were edited
// while it was busy, not merely THAT something was. A 1-slot channel, which
// is what it used to be, cannot: a burst of edits across Press 4 and Press 6
// would publish one of them and silently leave the other's Core mirror stale
// until the hourly safety snapshot.

// specChangeRecorder records hook calls. The FIRST per-process call blocks
// inside the hook until release is closed, so a test can pile requests up
// behind a busy loop and see exactly what the drain does with them.
type specChangeRecorder struct {
	mu      sync.Mutex
	calls   []int64 // process ids in hook order; specChangeAllMarker for a whole-plant call
	release chan struct{}
	entered chan struct{} // closed when the first per-process call is inside the hook
	once    sync.Once
}

const specChangeAllMarker int64 = -1

func newSpecChangeHandlers(t *testing.T) (*Handlers, *specChangeRecorder) {
	t.Helper()
	rec := &specChangeRecorder{release: make(chan struct{}), entered: make(chan struct{})}
	h := &Handlers{
		specChangeCh:   make(chan struct{}, 1),
		specChangeStop: make(chan struct{}),
	}
	h.SetPlantSpecChangeHook(
		func(processID int64) {
			rec.mu.Lock()
			rec.calls = append(rec.calls, processID)
			rec.mu.Unlock()
			rec.once.Do(func() {
				close(rec.entered)
				<-rec.release
			})
		},
		func() {
			rec.mu.Lock()
			rec.calls = append(rec.calls, specChangeAllMarker)
			rec.mu.Unlock()
		},
	)
	go h.specChangeLoop()
	t.Cleanup(func() { close(h.specChangeStop) })
	return h, rec
}

// waitForCalls polls until the recorder holds at least len(want) calls, lets
// the loop settle, and then asserts the exact sequence — so an implementation
// that publishes too MANY reports fails as well as one that drops some.
func waitForCalls(t *testing.T, rec *specChangeRecorder, want []int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rec.mu.Lock()
		n := len(rec.calls)
		rec.mu.Unlock()
		if n >= len(want) || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	rec.mu.Lock()
	got := append([]int64(nil), rec.calls...)
	rec.mu.Unlock()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hook calls = %v, want %v (-1 = whole plant)", got, want)
	}
}

func TestSpecChangeCoalescer_BurstAcrossProcessesPublishesEach(t *testing.T) {
	t.Parallel()
	h, rec := newSpecChangeHandlers(t)

	h.requestSpecChangePublish(4)
	<-rec.entered // the loop is inside the hook for 4 and stays there

	// The burst lands while the loop is busy: two more processes, one of
	// them twice, and 4 again.
	h.requestSpecChangePublish(6)
	h.requestSpecChangePublish(4)
	h.requestSpecChangePublish(6)
	h.requestSpecChangePublish(9)
	close(rec.release)

	// 4 (the call that was blocking), then ONE drain of the pending set in
	// sorted order: 4 again (it was edited after its publish started), 6, 9.
	// Nothing collapsed to a single publish and nothing was dropped.
	waitForCalls(t, rec, []int64{4, 4, 6, 9})
}

func TestSpecChangeCoalescer_WholePlantSubsumesPending(t *testing.T) {
	t.Parallel()
	h, rec := newSpecChangeHandlers(t)

	h.requestSpecChangePublish(4)
	<-rec.entered

	h.requestSpecChangePublish(6)
	h.requestSpecChangePublishAll()
	h.requestSpecChangePublish(9)
	close(rec.release)

	// PublishAll carries every process; publishing 6 and 9 again afterwards
	// would only add outbox rows.
	waitForCalls(t, rec, []int64{4, specChangeAllMarker})
}

// Test fixtures build Handlers without NewRouter (no channel, no map). A
// request on such a Handlers must be a no-op, not a nil-map panic: every
// style/claim handler test would otherwise fail on its last line.
func TestSpecChangeCoalescer_UnwiredHandlersAreInert(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	h.requestSpecChangePublish(1)
	h.requestSpecChangePublishAll()
}

// TestSpecChangeCoalescer_ASequentialBurstIsOnePublish is the property a
// preset apply depends on.
//
// The loop used to wake on the first doorbell and publish immediately, so the
// pending set only absorbed edits that OVERLAPPED — and an apply is not
// overlapping work, it is forty saves one after the other. Each one published
// its own report and broadcast its own material-refresh, which put every board
// on the plant into its 500 ms burst cadence for the duration; the polls that
// provoked cost the box about twice what the apply itself did.
//
// The window is what turns the burst into one drain. This Handlers is built
// here, so the window is set on it rather than on a package var the running
// loop reads from another goroutine.
func TestSpecChangeCoalescer_ASequentialBurstIsOnePublish(t *testing.T) {
	var mu sync.Mutex
	var calls []int64
	// The hub is NOT started: Broadcast only enqueues, so the buffered
	// channel is the count. No production seam added for a test.
	h := &Handlers{
		specChangeCh:     make(chan struct{}, 1),
		specChangeStop:   make(chan struct{}),
		eventHub:         NewEventHub(),
		specChangeWindow: 60 * time.Millisecond,
	}
	h.SetPlantSpecChangeHook(func(processID int64) {
		mu.Lock()
		calls = append(calls, processID)
		mu.Unlock()
	}, func() {})
	go h.specChangeLoop()
	t.Cleanup(func() { close(h.specChangeStop) })

	// Forty saves, one after the other, as an apply makes them.
	//
	// NO SLEEP BETWEEN THEM. This used to pace them at 1 ms apiece to make the
	// burst "sequential", which on Windows' ~15 ms timer granularity is forty
	// sleeps of up to 15 ms — 600 ms of wall clock inside a 60 ms window, so
	// the loop legitimately drains several times and the test flakes on its
	// own pacing. Sequential means "one call returns before the next begins",
	// which a plain loop already is; what the loop under test coalesces is
	// doorbells, not delays.
	for i := 0; i < 40; i++ {
		h.requestSpecChangePublishAndRefresh(4)
	}
	time.Sleep(h.specChangeWindow * 4)

	mu.Lock()
	gotCalls := append([]int64(nil), calls...)
	mu.Unlock()
	gotRefreshes := 0
	for draining := true; draining; {
		select {
		case e := <-h.eventHub.broadcast:
			if e.Type == "material-refresh" {
				gotRefreshes++
			}
		default:
			draining = false
		}
	}
	if len(gotCalls) > 2 {
		t.Errorf("forty sequential edits published %d reports (%v); the burst should collapse", len(gotCalls), gotCalls)
	}
	if len(gotCalls) == 0 {
		t.Error("forty edits published nothing")
	}
	if gotRefreshes > 2 {
		t.Errorf("forty sequential edits broadcast %d board refreshes; every board re-polls the "+
			"full view on each one", gotRefreshes)
	}
	if gotRefreshes == 0 {
		t.Error("the boards were never told the flow changed")
	}
}
