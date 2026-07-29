package engine

import (
	"testing"
	"time"

	"shingocore/store/demands"
)

func newTestMonitor() *ThresholdMonitor {
	return &ThresholdMonitor{
		eng:                 nil,
		debounce:            make(map[string]time.Time),
		warmUp:              make(map[string]int),
		sweepDone:           true,
		thresholdsByPayload: make(map[string][]thresholdEntry),
		negativeLogged:      make(map[string]time.Time),
		swapContradiction:   make(map[string]time.Time),
	}
}

// TestThresholdMonitor_Snapshot pins the monitored-set + binding view the
// Replenishment Health page reads. The snapshot no longer carries a cached UOP
// total (the private tally is gone); it reports only which payloads are
// monitored and the bindings watching each.
func TestThresholdMonitor_Snapshot(t *testing.T) {
	t.Parallel()
	tm := newTestMonitor()
	tm.thresholdsByPayload["WIDGET-A"] = []thresholdEntry{
		{stationID: "st-1", coreNodeName: "MS-A", payloadCode: "WIDGET-A", threshold: 120, loaderID: 7},
		{stationID: "st-1", coreNodeName: "SMN_015", payloadCode: "WIDGET-A", threshold: 96, loaderID: 7},
	}
	tm.thresholdsByPayload["WIDGET-B"] = []thresholdEntry{
		{stationID: "st-2", coreNodeName: "MS-B", payloadCode: "WIDGET-B", threshold: 40, loaderID: 3},
	}

	byCode := map[string]MonitorSnapshotEntry{}
	for _, s := range tm.Snapshot() {
		byCode[s.PayloadCode] = s
	}
	if len(byCode) != 2 {
		t.Fatalf("Snapshot returned %d payloads, want 2", len(byCode))
	}
	a, ok := byCode["WIDGET-A"]
	if !ok {
		t.Fatal("WIDGET-A missing from snapshot")
	}
	if len(a.Bindings) != 2 {
		t.Fatalf("WIDGET-A bindings = %d, want 2", len(a.Bindings))
	}
	maxThresh := 0
	for _, b := range a.Bindings {
		if b.Threshold > maxThresh {
			maxThresh = b.Threshold
		}
		if b.LoaderID != 7 {
			t.Errorf("WIDGET-A binding loader id = %d, want 7", b.LoaderID)
		}
	}
	if maxThresh != 120 {
		t.Errorf("WIDGET-A max binding threshold = %d, want 120", maxThresh)
	}
	if b, ok := byCode["WIDGET-B"]; !ok || len(b.Bindings) != 1 {
		t.Errorf("WIDGET-B should be present with 1 binding, got present=%v bindings=%d", ok, len(b.Bindings))
	}
}

// TestResolveLinesideMode pins the R1 config validation: empty and "edge_reports"
// resolve to the edge_reports default, "ledger" is the revert knob, and any
// unknown value falls back to edge_reports WITH a warning (never silently).
func TestResolveLinesideMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw      string
		want     string
		wantWarn bool
	}{
		{"", linesideModeEdgeReports, false},
		{"edge_reports", linesideModeEdgeReports, false},
		{"ledger", linesideModeLedger, false},
		{"EDGE_REPORTS", linesideModeEdgeReports, true}, // case-sensitive → unknown → warn+default
		{"garbage", linesideModeEdgeReports, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			warned := false
			got := resolveLinesideMode(tc.raw, func(string, ...any) { warned = true })
			if got != tc.want {
				t.Errorf("resolveLinesideMode(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			if warned != tc.wantWarn {
				t.Errorf("resolveLinesideMode(%q) warned=%v, want %v", tc.raw, warned, tc.wantWarn)
			}
		})
	}
}

// TestNewThresholdMonitor_DefaultsToEdgeReports pins that a monitor built with no
// engine (nil cfg) resolves to the edge_reports default rather than an empty/unset
// mode — the R1-live default holds even in the degenerate construction path.
func TestNewThresholdMonitor_DefaultsToEdgeReports(t *testing.T) {
	t.Parallel()
	m := NewThresholdMonitor(nil)
	if m.decisionMode() != linesideModeEdgeReports {
		t.Errorf("nil-engine monitor decision mode = %q, want %q", m.decisionMode(), linesideModeEdgeReports)
	}
}

func TestThresholdMonitor_DebounceWindow(t *testing.T) {
	t.Parallel()
	tm := newTestMonitor()

	key := bindingKey("station-1", "MS-LOADER", "WIDGET-A")
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

	key := bindingKey("station-1", "MS-LOADER", "WIDGET-A")
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
	key := bindingKey("station-1", "MS-LOADER", "WIDGET-A")
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

// TestThresholdMonitor_OnBucketApplied_EmitsEvent pins the one side effect
// OnBucketApplied still owns unconditionally: emitting EventLinesideBucketApplied
// for other subscribers. (The threshold re-evaluation now reads the DB, which
// the nil-inventory unit engine returns 0 for; an UNMONITORED payload is used
// so no fire path is exercised.) The old assertion on an incremental uopCache
// value is gone with the tally.
func TestThresholdMonitor_OnBucketApplied_EmitsEvent(t *testing.T) {
	t.Parallel()
	tm := newTestMonitor()
	tm.eng = &Engine{Events: NewEventBus()}

	var got *LinesideBucketAppliedEvent
	tm.eng.Events.SubscribeTypes(func(ev Event) {
		e := ev.Payload.(LinesideBucketAppliedEvent)
		got = &e
	}, EventLinesideBucketApplied)

	tm.OnBucketApplied("s1", "LOADER", "UNMONITORED-WIDGET", -10, "capture")

	if got == nil {
		t.Fatal("OnBucketApplied did not emit EventLinesideBucketApplied")
	}
	if got.PayloadCode != "UNMONITORED-WIDGET" || got.Delta != -10 || got.CoreNodeName != "LOADER" {
		t.Errorf("emitted event = %+v, want payload=UNMONITORED-WIDGET delta=-10 node=LOADER", *got)
	}
}

func TestThresholdMonitor_OnBucketApplied_SkipsEmptyPayload(t *testing.T) {
	t.Parallel()
	tm := newTestMonitor()
	tm.eng = &Engine{Events: NewEventBus()}
	tm.OnBucketApplied("s1", "LOADER", "", -5, "capture") // should not panic
}

func TestThresholdMonitor_CheckBindings_AboveThreshold_NoFire(t *testing.T) {
	t.Parallel()
	tm := newTestMonitor()
	tm.thresholdsByPayload["WIDGET-A"] = []thresholdEntry{
		{stationID: "s1", coreNodeName: "LOADER", payloadCode: "WIDGET-A", threshold: 50},
	}

	// Above threshold — checkBindings should not attempt to fire.
	// With nil eng, a fire attempt would panic, so this passing proves
	// the threshold check short-circuits correctly.
	tm.checkBindings([]thresholdEntry{
		{stationID: "s1", coreNodeName: "LOADER", payloadCode: "WIDGET-A", threshold: 50},
	}, 100, "below_threshold")
}

// TestThresholdMonitor_CheckBindings_NegativeTotal_StillFires pins the
// REVERSAL of the old validity floor.
//
// A negative total used to suppress replenishment entirely, on the reasoning
// that it is a broken ledger and acting on garbage is worse than doing
// nothing. On a plant floor that is backwards.
//
// A count goes negative for mundane physical reasons — a press overpacked, a
// fork truck delivered parts outside ShinGo, some human intervention it cannot
// see. It means "a person should look at this", not "stop the line". And the
// reading is too LOW, so the honest response to it is to order material, which
// is exactly what the threshold check does. Suppressing paired a number saying
// the line is empty with a system that ordered nothing — the first link in the
// 2026-07-21 chain, logged 1,119 times a day at Springfield.
//
// Over-ordering is recoverable. Starving a line because a count was wrong is
// not. So a negative total is logged loudly for a human and evaluated normally.
func TestThresholdMonitor_CheckBindings_NegativeTotal_StillFires(t *testing.T) {
	t.Parallel()
	tm := newTestMonitor()
	var fired []thresholdEntry
	tm.fireHook = func(b thresholdEntry, total int, reason string) { fired = append(fired, b) }
	tm.thresholdsByPayload["WIDGET-A"] = []thresholdEntry{
		{stationID: "s1", coreNodeName: "LOADER", payloadCode: "WIDGET-A", threshold: 50},
	}

	tm.checkBindings([]thresholdEntry{
		{stationID: "s1", coreNodeName: "LOADER", payloadCode: "WIDGET-A", threshold: 50},
	}, -443, "below_threshold")

	if len(fired) != 1 {
		t.Fatalf("a negative total must still order material, got %d signals", len(fired))
	}
	// And it consumes debounce like any other fire — it IS a fire now.
	if _, ok := tm.debounce[bindingKey("s1", "LOADER", "WIDGET-A")]; !ok {
		t.Error("a fired signal must record its debounce stamp")
	}
}

// The zero boundary — the floor rejects NEGATIVE totals only, and a genuine
// zero-stock payload must still signal — is already pinned end-to-end by
// TestThresholdMonitor_OnThresholdChanges_FiresImmediatelyWhenBelowThreshold
// (threshold_monitor_registry_pg_test.go), which asserts a fired signal with
// CurrentUOP == 0 against a real engine. Re-asserting it here against the
// nil-eng harness cannot be done without either catching a deliberate panic or
// writing a tautology, so it is deliberately left to that test.

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
	key := bindingKey("s1", "LOADER", "WIDGET-A")

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
	if !tm.shouldLogNegative(bindingKey("s1", "LOADER", "WIDGET-B")) {
		t.Error("throttle must be per binding, not global")
	}
}

// TestThresholdMonitor_NegativeCount_LogThrottleDoesNotGateOrdering is the
// property that keeps the throttle honest now that a negative count no longer
// suppresses anything: the LOG is rate-limited so a broken ledger cannot bury
// the plant log, but the ORDERING is not. Every tick still evaluates.
//
// Getting this wrong would reintroduce the old failure quietly — a line going
// unserved because its warning had already been printed.
func TestThresholdMonitor_NegativeCount_LogThrottleDoesNotGateOrdering(t *testing.T) {
	t.Parallel()
	tm := newTestMonitor()
	var fires int
	tm.fireHook = func(thresholdEntry, int, string) { fires++ }
	entry := thresholdEntry{stationID: "s1", coreNodeName: "LOADER", payloadCode: "WIDGET-A", threshold: 50}
	tm.thresholdsByPayload["WIDGET-A"] = []thresholdEntry{entry}
	key := bindingKey("s1", "LOADER", "WIDGET-A")

	// First tick fires. Subsequent ticks are held off by the normal debounce
	// (which applies to every fire, negative or not) — NOT by the negative
	// path, which no longer gates anything.
	for range 25 {
		tm.checkBindings([]thresholdEntry{entry}, -443, "below_threshold")
	}

	if fires == 0 {
		t.Fatal("a negative count must still order material")
	}
	tm.mu.Lock()
	stamps := len(tm.negativeLogged)
	_, debounced := tm.debounce[key]
	tm.mu.Unlock()
	if stamps != 1 {
		t.Errorf("negativeLogged entries = %d, want 1 — the LOG is throttled", stamps)
	}
	if !debounced {
		t.Error("a fired signal must record its debounce stamp")
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
		key := bindingKey("station-1", "MS-LOADER", "WIDGET-A")
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
		key := bindingKey("station-1", "MS-LOADER", "WIDGET-B")
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
		// Snapshot reports per MONITORED payload, and the production caller
		// returns early for an unmonitored one, so the chip is only reachable
		// with a binding present.
		tm.mu.Lock()
		tm.thresholdsByPayload["WIDGET-C"] = []thresholdEntry{
			{stationID: "station-1", coreNodeName: "MS-LOADER", payloadCode: "WIDGET-C", threshold: 100},
		}
		tm.mu.Unlock()

		if !tm.recordSwapContradiction("WIDGET-C") {
			t.Fatal("first contradiction should record")
		}
		if tm.recordSwapContradiction("WIDGET-C") {
			t.Error("a second within the window must be throttled")
		}
		// The chip reads the same stamp against the same clock, so it must
		// still be showing here.
		if !hasContradiction(tm.Snapshot(), "WIDGET-C") {
			t.Error("the contradiction chip must be lit inside its window")
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
	key := bindingKey("station-1", "MS-LOADER", "WIDGET-Z")
	if !tm.allow(key) {
		t.Fatal("a monitor with no clock must still work")
	}
	if tm.allow(key) {
		t.Error("and must still debounce")
	}
}

func hasContradiction(snap []MonitorSnapshotEntry, payload string) bool {
	for _, e := range snap {
		if e.PayloadCode == payload {
			return e.SwapContradiction
		}
	}
	return false
}
