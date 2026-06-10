package config

import (
	"path/filepath"
	"testing"
	"time"
)

// TestDevYAMLParses verifies shingoedge.dev.yaml parses with the expected sim
// knobs (and the warlink-disabled-in-sim invariant). Unknown keys are silently
// ignored, so we assert expected VALUES — catches a mistyped key.
func TestDevYAMLParses(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "shingoedge.dev.yaml"))
	if err != nil {
		t.Fatalf("load dev config: %v", err)
	}
	if !cfg.Sim.Enabled {
		t.Error("sim.enabled should be true")
	}
	if cfg.Namespace != "devplant" || cfg.LineID != "line1" {
		t.Errorf("station = %s.%s, want devplant.line1", cfg.Namespace, cfg.LineID)
	}
	if cfg.WarLink.Enabled {
		t.Error("warlink.enabled must be false in sim (sim starts the poller explicitly)")
	}
	if cfg.WarLink.Mode != "poll" {
		t.Errorf("warlink.mode = %q, want poll", cfg.WarLink.Mode)
	}
	// v3 plant: 5 sim processes (PRESS-1, PRESS-2, WELD-1, WELD-2, WELD-3).
	// Manual_swap nodes (LOADER-COMP, UNLOADER-A, UNLOADER-B) don't tick.
	if len(cfg.Sim.Processes) != 5 {
		t.Fatalf("sim.processes = %d, want 5", len(cfg.Sim.Processes))
	}
	p0 := cfg.Sim.Processes[0]
	if p0.PLCName != "PRESS-1" || p0.TagName != "PRESS-1_COUNTER" {
		t.Errorf("process[0] = %s/%s, want PRESS-1/PRESS-1_COUNTER", p0.PLCName, p0.TagName)
	}
	// PRESS-1 ticks 5s (12/min) — 2× the 10s line rate — because it feeds TWO
	// PANEL-LH consumers (WELD-1 + WELD-3). Rate-balance fix; verify via `make dev-rates`.
	if p0.TickInterval != 5*time.Second || p0.UOPPerTick != 1 {
		t.Errorf("process[0] timing = %v/%d, want 5s/1", p0.TickInterval, p0.UOPPerTick)
	}
	if !cfg.Sim.Operators.Enabled || cfg.Sim.Operators.LoaderAutoLoad != 5*time.Second {
		t.Errorf("operators = %+v, want enabled + 5s load", cfg.Sim.Operators)
	}
}
