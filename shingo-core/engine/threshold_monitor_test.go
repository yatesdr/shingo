package engine

import (
	"testing"
	"time"

	"shingocore/store/demands"
)

// TestThresholdMonitor_DebounceWindow verifies the in-memory debounce
// suppresses repeat fires for the same (loader, payload) within 15s.
// First call passes, second within window blocks, then after the
// window the gate reopens.
func TestThresholdMonitor_DebounceWindow(t *testing.T) {
	t.Parallel()
	tm := &ThresholdMonitor{
		eng:       nil, // not used in allow()
		debounce:  make(map[string]time.Time),
		warmUp:    make(map[string]int),
		sweepDone: true,
	}

	key := bindingKey("station-1", "MS-LOADER", "WIDGET-A")
	if !tm.allow(key) {
		t.Fatal("first allow should pass")
	}
	if tm.allow(key) {
		t.Error("second allow within debounce window should block")
	}

	// Simulate the debounce expiring by aging the recorded timestamp.
	tm.mu.Lock()
	tm.debounce[key] = time.Now().Add(-thresholdDebounceWindow - time.Second)
	tm.mu.Unlock()

	if !tm.allow(key) {
		t.Error("allow after debounce expires should pass")
	}
}

// TestThresholdMonitor_OnRegistryChanges resets debounce + warm-up
// state for changed bindings so a freshly-applied threshold engages
// without waiting out the prior window.
func TestThresholdMonitor_OnRegistryChanges(t *testing.T) {
	t.Parallel()
	tm := &ThresholdMonitor{
		eng:       nil,
		debounce:  make(map[string]time.Time),
		warmUp:    make(map[string]int),
		sweepDone: true,
	}

	key := bindingKey("station-1", "MS-LOADER", "WIDGET-A")
	tm.allow(key) // records a firing time

	if tm.allow(key) {
		t.Fatal("debounce should block before reset")
	}

	tm.OnRegistryChanges([]demands.RegistryChange{{
		StationID:    "station-1",
		CoreNodeName: "MS-LOADER",
		PayloadCode:  "WIDGET-A",
		OldThreshold: 5,
		NewThreshold: 10,
	}})

	if !tm.allow(key) {
		t.Error("allow after OnRegistryChanges reset should pass")
	}
}

// TestThresholdMonitor_WarmUpOverridesDebounce — warm-up counter > 0
// bypasses the debounce window so the startup-sweep cap can fire
// multiple back-to-back signals on a cold-start binding.
func TestThresholdMonitor_WarmUpOverridesDebounce(t *testing.T) {
	t.Parallel()
	tm := &ThresholdMonitor{
		eng:       nil,
		debounce:  make(map[string]time.Time),
		warmUp:    make(map[string]int),
		sweepDone: true,
	}
	key := bindingKey("station-1", "MS-LOADER", "WIDGET-A")
	tm.warmUp[key] = 2

	if !tm.allow(key) {
		t.Fatal("first allow during warm-up should pass")
	}
	if !tm.allow(key) {
		t.Error("second allow during warm-up should also pass (warm-up overrides debounce)")
	}
	// Warm-up exhausted; debounce gates from here.
	if tm.allow(key) {
		t.Error("third allow after warm-up exhausted should block (debounce)")
	}
}
