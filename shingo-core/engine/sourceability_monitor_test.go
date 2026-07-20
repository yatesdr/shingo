package engine

import (
	"sync"
	"testing"
	"time"

	"shingocore/store/plantclaims"
)

// No DB: exercises the debounce/dirty-set batching with an injected recompute
// callback, so it runs everywhere (not gated behind the docker tag).

func newDebounceTestMonitor(window time.Duration) (*SourceabilityMonitor, func() (int, []plantclaims.ProcessKey)) {
	var (
		mu       sync.Mutex
		calls    int
		lastKeys []plantclaims.ProcessKey
	)
	m := &SourceabilityMonitor{
		debounceWindow: window,
		dirty:          map[plantclaims.ProcessKey]struct{}{},
		index: map[string][]plantclaims.ProcessKey{
			"BIN-A": {{ProcessID: "SNF2", StyleID: "A"}},
			"BIN-B": {{ProcessID: "SNF2", StyleID: "B"}},
		},
	}
	m.recomputeFn = func(keys []plantclaims.ProcessKey) {
		mu.Lock()
		calls++
		lastKeys = keys
		mu.Unlock()
	}
	observe := func() (int, []plantclaims.ProcessKey) {
		mu.Lock()
		defer mu.Unlock()
		return calls, lastKeys
	}
	return m, observe
}

// TestSourceabilityMonitor_DebounceBatchesRecomputes: a burst of change events
// inside the debounce window collapses to ONE recompute covering the distinct
// dirty styles — no per-event recompute storm.
func TestSourceabilityMonitor_DebounceBatchesRecomputes(t *testing.T) {
	m, observe := newDebounceTestMonitor(40 * time.Millisecond)

	for range 20 {
		m.onPayloadChanged("BIN-A")
		m.onPayloadChanged("BIN-B")
	}

	// Nothing fires until the window elapses.
	if calls, _ := observe(); calls != 0 {
		t.Fatalf("recomputes before window = %d, want 0", calls)
	}

	time.Sleep(150 * time.Millisecond)

	calls, keys := observe()
	if calls != 1 {
		t.Fatalf("recomputes = %d, want 1 (40 events → one batched recompute)", calls)
	}
	if len(keys) != 2 {
		t.Errorf("recomputed keys = %v, want the 2 distinct styles", keys)
	}
}

// TestSourceabilityMonitor_UnclaimedPayloadIgnored: an event for a payload no
// style requires marks nothing dirty and triggers no recompute.
func TestSourceabilityMonitor_UnclaimedPayloadIgnored(t *testing.T) {
	m, observe := newDebounceTestMonitor(40 * time.Millisecond)

	m.onPayloadChanged("BIN-NOBODY-WANTS")

	time.Sleep(150 * time.Millisecond)
	if calls, _ := observe(); calls != 0 {
		t.Errorf("recomputes = %d, want 0 (irrelevant payload)", calls)
	}
}

// TestSourceabilityMonitor_SecondBurstRecomputesAgain: after a window fires, a
// later change starts a fresh window and recomputes again.
func TestSourceabilityMonitor_SecondBurstRecomputesAgain(t *testing.T) {
	m, observe := newDebounceTestMonitor(40 * time.Millisecond)

	m.onPayloadChanged("BIN-A")
	time.Sleep(150 * time.Millisecond)
	if calls, _ := observe(); calls != 1 {
		t.Fatalf("after first burst recomputes = %d, want 1", calls)
	}

	m.onPayloadChanged("BIN-B")
	time.Sleep(150 * time.Millisecond)
	if calls, _ := observe(); calls != 2 {
		t.Errorf("after second burst recomputes = %d, want 2", calls)
	}
}
