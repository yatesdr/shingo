package engine

import (
	"testing"
	"time"

	"shingocore/store/demands"
)

func newTestMonitor() *ThresholdMonitor {
	return &ThresholdMonitor{
		eng:               nil,
		debounce:          make(map[string]time.Time),
		warmUp:            make(map[string]int),
		negativeLogged:    make(map[string]time.Time),
		duplicateLogged:   make(map[string]time.Time),
		swapContradiction: make(map[string]time.Time),
	}
}

// The Snapshot read model is pinned against a real database in
// threshold_readthrough_test.go (TestReadThrough_SnapshotReadsTheRegistry): it
// reads demand_registry on every call and has nothing to show without one.

func TestThresholdMonitor_DebounceWindow(t *testing.T) {
	t.Parallel()
	tm := newTestMonitor()

	key := placeKey("MS-LOADER", "WIDGET-A")
	if !tm.allow(key) {
		t.Fatal("first allow should pass")
	}
	if tm.allow(key) {
		t.Error("second allow within debounce window should block")
	}

	tm.mu.Lock()
	tm.debounce[key] = time.Now().Add(-thresholdDebounceWindow - time.Second)
	tm.mu.Unlock()

	if !tm.allow(key) {
		t.Error("allow after debounce expires should pass")
	}
}

func TestThresholdMonitor_OnThresholdChanges(t *testing.T) {
	t.Parallel()
	tm := newTestMonitor()

	key := placeKey("MS-LOADER", "WIDGET-A")
	tm.allow(key)

	if tm.allow(key) {
		t.Fatal("debounce should block before reset")
	}

	tm.OnThresholdChanges([]demands.RegistryChange{{
		StationID:    "station-1",
		CoreNodeName: "MS-LOADER",
		PayloadCode:  "WIDGET-A",
		OldThreshold: 5,
		NewThreshold: 10,
	}})

	if !tm.allow(key) {
		t.Error("allow after OnThresholdChanges reset should pass")
	}
}

func TestThresholdMonitor_WarmUpOverridesDebounce(t *testing.T) {
	t.Parallel()
	tm := newTestMonitor()
	key := placeKey("MS-LOADER", "WIDGET-A")
	tm.warmUp[key] = 2

	if !tm.allow(key) {
		t.Fatal("first allow during warm-up should pass")
	}
	if !tm.allow(key) {
		t.Error("second allow during warm-up should also pass")
	}
	if tm.allow(key) {
		t.Error("third allow after warm-up exhausted should block")
	}
}

// The incremental-cache application test was deleted with the private tally:
// OnBinUOPDelta no longer moves an in-memory total, it re-reads the
// authoritative DB sum. The read-authoritative behavior is pinned end-to-end
// against a real engine in threshold_monitor_registry_pg_test.go
// (TestThresholdMonitor_ReadsAuthoritativeSum_NotAStaleCache and the negative-
// total tests), which cannot be expressed against the nil-eng unit harness.

func TestThresholdMonitor_OnBinUOPDelta_SkipsEmptyPayload(t *testing.T) {
	t.Parallel()
	tm := newTestMonitor()
	tm.OnBinUOPDelta("", -5) // should not panic
}

func TestThresholdMonitor_OnBinUOPDelta_NoBindings(t *testing.T) {
	t.Parallel()
	tm := newTestMonitor()
	tm.OnBinUOPDelta("UNMONITORED", -10) // no bindings, should not panic
}

func TestThresholdMonitor_OnBucketApplied_SkipsEmptyPayload(t *testing.T) {
	t.Parallel()
	tm := newTestMonitor()
	tm.eng = &Engine{Events: NewEventBus()}
	tm.OnBucketApplied("") // should not panic
}

// The fire decision needs a database now — every edge reads the place's open
// episode from demand_origins, and a fire without one is refused — so the
// checkBindings cases that used to run against the nil-engine harness live in
// threshold_readthrough_test.go: the negative total still fires
// (TestReadThrough_FireGateBelowAtAbove/negative), and its log throttle does
// not gate ordering (TestReadThrough_NegativeLogThrottleDoesNotGateOrdering).

// TestThresholdMonitor_NegativeLogThrottle pins the log-volume control on the
// broken-ledger refusal.
//
// The floor is evaluated on every incoming delta — every consume tick — so an
// unthrottled refusal line buries the plant log in exactly the situation an
// operator needs to read it. The throttle must NOT be implemented by borrowing
// the debounce stamp: debounce is signal-eligibility budget, and spending it on
// a garbage total would delay the first real signal once the ledger is fixed.
func TestThresholdMonitor_NegativeLogThrottle(t *testing.T) {
	t.Parallel()
	tm := newTestMonitor()
	key := placeKey("LOADER", "WIDGET-A")

	if !tm.shouldLogNegative(key) {
		t.Fatal("first refusal must log")
	}
	if tm.shouldLogNegative(key) {
		t.Error("second refusal within the window must be suppressed")
	}

	// Age the stamp past the window — the condition is still true, so it must
	// be reported again rather than staying silent forever.
	tm.mu.Lock()
	tm.negativeLogged[key] = time.Now().Add(-negativeLogWindow - time.Second)
	tm.mu.Unlock()
	if !tm.shouldLogNegative(key) {
		t.Error("refusal must log again once the window expires")
	}

	// A different binding has its own budget.
	if !tm.shouldLogNegative(placeKey("LOADER", "WIDGET-B")) {
		t.Error("throttle must be per binding, not global")
	}
}

// THE SIM DEFECT. Every interval this monitor measures used to call bare
// time.Now() while everything it gates moves in SIM time: sim startup installs
// a fast-forward clock globally (clock.BuildSimClock → clock.SetDefault,
// cmd/shingocore/sim_enabled.go:47,61) and the rest of the engine reads
// clock.Now().
//
// So at 15× the 15-second debounce covered fifteen times more SIMULATED
// activity than the same debounce covers at a plant — and the hysteresis
// margins for the demand-episode work get tuned on that sim. Same for the
// 60-second negative-log window and the 15-minute contradiction window.
//
// This drives a clock forward the way the sim does and asserts each window
// closes on ITS clock. Against the pre-fix code every case fails, because
// nothing the test does to the clock is visible to the monitor.
func TestThresholdMonitor_WindowsRunOnTheInjectedClock(t *testing.T) {
	t.Parallel()

	// A monitor whose clock is ours, moved by hand — exactly what a sim clock
	// does to it, just deterministically.
	simNow := time.Date(2026, 7, 25, 6, 0, 0, 0, time.UTC)
	tm := newTestMonitor()
	tm.now = func() time.Time { return simNow }

	t.Run("debounce", func(t *testing.T) {
		key := placeKey("MS-LOADER", "WIDGET-A")
		if !tm.allow(key) {
			t.Fatal("first allow should pass")
		}
		// Wall time has not moved and never will in this test. If the monitor
		// were still reading it, the window would never close.
		simNow = simNow.Add(thresholdDebounceWindow + time.Second)
		if !tm.allow(key) {
			t.Error("the debounce window must close on the monitor's clock, not the wall clock")
		}
	})

	t.Run("negative log", func(t *testing.T) {
		key := placeKey("MS-LOADER", "WIDGET-B")
		if !tm.shouldLogNegative(key) {
			t.Fatal("first negative log should fire")
		}
		if tm.shouldLogNegative(key) {
			t.Error("a second within the window must be throttled")
		}
		simNow = simNow.Add(negativeLogWindow + time.Second)
		if !tm.shouldLogNegative(key) {
			t.Error("the negative-log window must close on the monitor's clock")
		}
	})

	t.Run("swap contradiction", func(t *testing.T) {
		if !tm.recordSwapContradiction("WIDGET-C") {
			t.Fatal("first contradiction should record")
		}
		if tm.recordSwapContradiction("WIDGET-C") {
			t.Error("a second within the window must be throttled")
		}
		simNow = simNow.Add(swapContradictionWindow + time.Second)
		if !tm.recordSwapContradiction("WIDGET-C") {
			t.Error("the contradiction window must close on the monitor's clock")
		}
	})
}

// A monitor built as a struct literal — which the pure unit harness does — has
// a nil clock, and must fall back rather than panic.
func TestThresholdMonitor_ZeroValueClockFallsBack(t *testing.T) {
	t.Parallel()
	tm := newTestMonitor() // deliberately leaves tm.now nil
	if tm.now != nil {
		t.Fatal("fixture changed — this test is about the nil case")
	}
	key := placeKey("MS-LOADER", "WIDGET-Z")
	if !tm.allow(key) {
		t.Fatal("a monitor with no clock must still work")
	}
	if tm.allow(key) {
		t.Error("and must still debounce")
	}
}
