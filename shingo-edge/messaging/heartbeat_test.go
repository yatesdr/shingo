package messaging

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"shingoedge/config"
)

// TestHeartbeat_FiresReconcile pins the Item 1 wiring: the heartbeat
// loop fires the per-tick reconcile callback after sendHeartbeat
// returns. Without this, the reconciler stays passive forever — drift
// at any node accumulates indefinitely instead of healing within ~60s.
//
// Tested via fireReconcile directly to avoid coupling the assertion to
// the timer-driven loop. The loop's contract is "every successful tick
// → fireReconcile" (a single line in the loop body); pinning
// fireReconcile's behavior is sufficient.
func TestHeartbeat_FiresReconcile(t *testing.T) {
	hb := &Heartbeater{}

	// No callback set → fireReconcile is a safe no-op (production starts
	// in this state until composition root calls SetReconcileFn).
	hb.fireReconcile()

	called := 0
	hb.SetReconcileFn(func() { called++ })

	hb.fireReconcile()
	if called != 1 {
		t.Errorf("after one fireReconcile: called = %d, want 1", called)
	}
	hb.fireReconcile()
	hb.fireReconcile()
	if called != 3 {
		t.Errorf("after three fireReconciles: called = %d, want 3", called)
	}
}

// TestHeartbeat_FireReconcileSwallowsPanic pins the panic-recovery
// guarantee: a reconciler panic must NOT take down the heartbeat loop
// (heartbeat is the PLC fail-safe; if it dies the deadman trips).
func TestHeartbeat_FireReconcileSwallowsPanic(t *testing.T) {
	hb := &Heartbeater{}
	hb.SetReconcileFn(func() { panic("simulated reconcile panic") })

	// fireReconcile must return normally even when the callback panics.
	hb.fireReconcile()
	hb.fireReconcile()

	// Sanity: a non-panicking callback after the panicking one still runs.
	called := 0
	hb.SetReconcileFn(func() { called++ })
	hb.fireReconcile()
	if called != 1 {
		t.Errorf("post-panic callback ran %d times, want 1", called)
	}
}

// TestReconcileInterval_LoadsFromYAML pins the YAML default + override
// path: the config defaults to 60s, and a YAML file with a custom
// uop.reconcile_interval value loads correctly. Composition root reads
// cfg.UOP.ReconcileInterval and threads it through engine.SetReconcileInterval.
func TestReconcileInterval_LoadsFromYAML(t *testing.T) {
	defaults := config.Defaults()
	if defaults.UOP.ReconcileInterval != 60*time.Second {
		t.Errorf("default ReconcileInterval = %v, want 60s", defaults.UOP.ReconcileInterval)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "shingoedge.yaml")
	body := []byte(`uop:
  reconcile_interval: 90s
`)
	if err := os.WriteFile(path, body, 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UOP.ReconcileInterval != 90*time.Second {
		t.Errorf("loaded ReconcileInterval = %v, want 90s", cfg.UOP.ReconcileInterval)
	}

	// Round-trip: save the loaded config and re-load to make sure the
	// yaml tags survive serialization (a missing tag would silently drop
	// the field on Save).
	roundtripPath := filepath.Join(dir, "roundtrip.yaml")
	if err := cfg.Save(roundtripPath); err != nil {
		t.Fatalf("Save: %v", err)
	}
	cfg2, err := config.Load(roundtripPath)
	if err != nil {
		t.Fatalf("Load roundtrip: %v", err)
	}
	if cfg2.UOP.ReconcileInterval != 90*time.Second {
		t.Errorf("round-tripped ReconcileInterval = %v, want 90s (yaml tag missing?)",
			cfg2.UOP.ReconcileInterval)
	}
}
