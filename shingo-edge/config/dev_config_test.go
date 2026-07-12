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
	// 6 sim processes (PRESS-1/2, WELD-1/2/3/4). The SYN_MARKET combine collapsed the
	// separate per-type markets into one mixed-fill market, which retired the extra
	// component lines (and their PRESS-3/4 + WELD-5). Manual_swap loaders/unloaders
	// don't tick.
	if len(cfg.Sim.Processes) != 6 {
		t.Fatalf("sim.processes = %d, want 6", len(cfg.Sim.Processes))
	}
	p0 := cfg.Sim.Processes[0]
	if p0.PLCName != "PRESS-1" || p0.TagName != "PRESS-1_COUNTER" {
		t.Errorf("process[0] = %s/%s, want PRESS-1/PRESS-1_COUNTER", p0.PLCName, p0.TagName)
	}
	// Every process now ticks at the flat 10s line rate (6/min) — the rate retune that
	// came with the market combine. Verify via `make dev-rates`.
	if p0.TickInterval != 10*time.Second || p0.UOPPerTick != 1 {
		t.Errorf("process[0] timing = %v/%d, want 10s/1", p0.TickInterval, p0.UOPPerTick)
	}
	if !cfg.Sim.Operators.Enabled || cfg.Sim.Operators.LoaderAutoLoad != 5*time.Second {
		t.Errorf("operators = %+v, want enabled + 5s load", cfg.Sim.Operators)
	}
}
