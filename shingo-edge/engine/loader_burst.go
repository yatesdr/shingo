package engine

import (
	"sync"
	"time"
)

// Burst-warn observability for the loader/unloader in-bin order paths (PR-0).
// The SLN_002 incident (2026-06-11) put ~39 retrieve_empty orders on one
// delivery node in ~38s; the per-payload log lines were the only reason it was
// diagnosable after the fact. The PR-0 capacity cap makes that burst
// unrepresentable through the legacy/threshold paths, but this tripwire stays
// as defense-in-depth: any future path that bypasses the cap, or a cap
// regression, trips a WARN with the node and count instead of being silent.
const (
	loaderBurstWarnWindow    = 60 * time.Second
	loaderBurstWarnThreshold = 8
)

// loaderBurstTracker records recent in-bin order creations per delivery node
// and reports the rolling count within loaderBurstWarnWindow. The zero value is
// usable; the map is lazily initialized under the mutex. Held by value on
// Engine (never copied — Engine is always used through a pointer).
type loaderBurstTracker struct {
	mu     sync.Mutex
	events map[string][]time.Time
}

// record appends n creation timestamps for coreNodeName, prunes anything older
// than the window, and returns the resulting rolling count.
func (t *loaderBurstTracker) record(coreNodeName string, n int, now time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.events == nil {
		t.events = make(map[string][]time.Time)
	}
	cutoff := now.Add(-loaderBurstWarnWindow)
	prior := t.events[coreNodeName]
	kept := prior[:0] // reuse backing array; prune in place
	for _, ts := range prior {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	for range n {
		kept = append(kept, now)
	}
	t.events[coreNodeName] = kept
	return len(kept)
}

// recordL1Burst notes that n in-bin orders (loader L1 empties or unloader U1
// fulls) were just created at coreNodeName and WARNs if the node has exceeded
// the burst threshold within the window.
func (e *Engine) recordL1Burst(coreNodeName string, n int) {
	if n <= 0 {
		return
	}
	count := e.l1Burst.record(coreNodeName, n, time.Now())
	if count > loaderBurstWarnThreshold {
		e.logFn("WARN burst: %d in-bin orders to delivery node %s within %s — possible loader/unloader misconfig or capacity-cap regression",
			count, coreNodeName, loaderBurstWarnWindow)
	}
}
