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
		uopCache:            make(map[string]int),
		negativeLogged:      make(map[string]time.Time),
	}
}

func TestThresholdMonitor_Snapshot(t *testing.T) {
	t.Parallel()
	tm := newTestMonitor()
	tm.thresholdsByPayload["WIDGET-A"] = []thresholdEntry{
		{stationID: "st-1", coreNodeName: "MS-A", payloadCode: "WIDGET-A", threshold: 120, loaderID: 7},
		{stationID: "st-1", coreNodeName: "SMN_015", payloadCode: "WIDGET-A", threshold: 96, loaderID: 7},
	}
	tm.uopCache["WIDGET-A"] = 139
	// Monitored payload with no cached delta yet reports 0, not absent.
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
	if a.CachedTotal != 139 {
		t.Errorf("WIDGET-A cached = %d, want 139", a.CachedTotal)
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
	if b := byCode["WIDGET-B"]; b.CachedTotal != 0 {
		t.Errorf("WIDGET-B cached = %d, want 0 (no delta seeded yet)", b.CachedTotal)
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

func TestThresholdMonitor_OnBinUOPDelta_AppliesIncrementally(t *testing.T) {
	t.Parallel()
	tm := newTestMonitor()
	tm.thresholdsByPayload["WIDGET-A"] = []thresholdEntry{
		{stationID: "s1", coreNodeName: "LOADER", payloadCode: "WIDGET-A", threshold: 50},
	}
	tm.uopCache["WIDGET-A"] = 100

	tm.OnBinUOPDelta("WIDGET-A", -5)
	tm.mu.Lock()
	if tm.uopCache["WIDGET-A"] != 95 {
		t.Errorf("uopCache = %d, want 95", tm.uopCache["WIDGET-A"])
	}
	tm.mu.Unlock()

	tm.OnBinUOPDelta("WIDGET-A", -10)
	tm.mu.Lock()
	if tm.uopCache["WIDGET-A"] != 85 {
		t.Errorf("uopCache = %d, want 85", tm.uopCache["WIDGET-A"])
	}
	tm.mu.Unlock()
}

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

func TestThresholdMonitor_OnBucketApplied_AppliesDelta(t *testing.T) {
	t.Parallel()
	tm := newTestMonitor()
	tm.eng = &Engine{Events: NewEventBus()}
	tm.thresholdsByPayload["WIDGET-A"] = []thresholdEntry{
		{stationID: "s1", coreNodeName: "LOADER", payloadCode: "WIDGET-A", threshold: 50},
	}
	tm.uopCache["WIDGET-A"] = 100

	tm.OnBucketApplied("s1", "LOADER", "WIDGET-A", -10, "capture")
	tm.mu.Lock()
	if tm.uopCache["WIDGET-A"] != 90 {
		t.Errorf("uopCache = %d, want 90", tm.uopCache["WIDGET-A"])
	}
	tm.mu.Unlock()
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
	tm.uopCache["WIDGET-A"] = 100

	// Above threshold — checkBindings should not attempt to fire.
	// With nil eng, a fire attempt would panic, so this passing proves
	// the threshold check short-circuits correctly.
	tm.checkBindings([]thresholdEntry{
		{stationID: "s1", coreNodeName: "LOADER", payloadCode: "WIDGET-A", threshold: 50},
	}, 100, "below_threshold")
}

// TestThresholdMonitor_CheckBindings_NegativeTotal_NoFire pins the validity
// floor. A negative in-loop total is never real demand — bins may go negative
// by SME lock (overpack/underpack) but buckets are CHECK (qty >= 0), so a
// negative SUM means the bins ledger for that payload is broken. Springfield
// signalled 74577-6SA0A.06 at −443 on 2026-07-21; below-threshold was
// trivially true and the L1s it produced looked entirely legitimate.
//
// Same no-panic proof as the above-threshold case: with nil eng any fire
// attempt would dereference through fireSignalCached and panic.
func TestThresholdMonitor_CheckBindings_NegativeTotal_NoFire(t *testing.T) {
	t.Parallel()
	tm := newTestMonitor()
	tm.thresholdsByPayload["WIDGET-A"] = []thresholdEntry{
		{stationID: "s1", coreNodeName: "LOADER", payloadCode: "WIDGET-A", threshold: 50},
	}
	// Deeply below threshold — without the floor this fires immediately.
	tm.uopCache["WIDGET-A"] = -443

	tm.checkBindings([]thresholdEntry{
		{stationID: "s1", coreNodeName: "LOADER", payloadCode: "WIDGET-A", threshold: 50},
	}, -443, "below_threshold")

	// The floor must not consume debounce budget either: a refused evaluation
	// is not a fire, so the binding stays eligible for the moment the ledger
	// is corrected.
	if _, fired := tm.debounce[bindingKey("s1", "LOADER", "WIDGET-A")]; fired {
		t.Error("negative-total refusal recorded a debounce stamp; it must not count as a fire")
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

// TestThresholdMonitor_NegativeRefusal_PreservesEligibility is the property that
// makes the throttle safe: however many negative evaluations go by, the binding
// must still be immediately eligible to signal the moment the total is real
// again. Refusing to act on garbage must cost the binding nothing.
func TestThresholdMonitor_NegativeRefusal_PreservesEligibility(t *testing.T) {
	t.Parallel()
	tm := newTestMonitor()
	entry := thresholdEntry{stationID: "s1", coreNodeName: "LOADER", payloadCode: "WIDGET-A", threshold: 50}
	tm.thresholdsByPayload["WIDGET-A"] = []thresholdEntry{entry}
	key := bindingKey("s1", "LOADER", "WIDGET-A")

	// Many ticks against a broken ledger. nil eng: any fire attempt panics.
	for i := 0; i < 25; i++ {
		tm.checkBindings([]thresholdEntry{entry}, -443, "below_threshold")
	}

	// Exactly one log stamp was taken (the rest throttled) ...
	tm.mu.Lock()
	stamps := len(tm.negativeLogged)
	_, debounced := tm.debounce[key]
	tm.mu.Unlock()
	if stamps != 1 {
		t.Errorf("negativeLogged entries = %d, want 1", stamps)
	}
	// ... and NO debounce budget was consumed.
	if debounced {
		t.Error("a refused evaluation consumed debounce budget; the binding is now delayed for a fault that wasn't its own")
	}
	// So the binding is still immediately eligible the moment the ledger is fixed.
	if !tm.allow(key) {
		t.Error("binding is not immediately eligible after the ledger is corrected")
	}
}
