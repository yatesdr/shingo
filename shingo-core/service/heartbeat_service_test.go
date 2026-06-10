package service

import (
	"encoding/json"
	"testing"

	"shingocore/store"
)

// TestDeriveCellConfig pins the Q-034 auto-derive: a catalog cell (PLC +
// process bindings) becomes a display CellConfig — label as id/name, first
// binding primary, the rest satellites — and degrades safely on empty/bad
// bindings (the heartbeat must never error on a malformed catalog row).
func TestDeriveCellConfig(t *testing.T) {
	bindings, _ := json.Marshal([]map[string]any{
		{"process_id": 10, "style_id": 14, "plc_name": "PRESS-1", "tag_name": "T1"},
		{"process_id": 20, "style_id": 14, "plc_name": "PRESS-1", "tag_name": "T2"},
	})
	cfg, ok := deriveCellConfig(store.EdgeCell{Station: "SNF2", CellLabel: "PRESS-1", Bindings: bindings})
	if !ok {
		t.Fatal("expected a derived cell")
	}
	if cfg.CellID != "PRESS-1" || cfg.Station != "SNF2" || cfg.DisplayName != "PRESS-1" {
		t.Errorf("identity wrong: %+v", cfg)
	}
	if cfg.PrimaryProcessID != 10 {
		t.Errorf("primary = %d, want 10 (first binding)", cfg.PrimaryProcessID)
	}
	if len(cfg.SubProcessIDs) != 1 || cfg.SubProcessIDs[0] != 20 {
		t.Errorf("subs = %v, want [20]", cfg.SubProcessIDs)
	}

	// Empty bindings → nothing to pulse → not a cell.
	if _, ok := deriveCellConfig(store.EdgeCell{CellLabel: "X", Bindings: json.RawMessage("[]")}); ok {
		t.Error("empty bindings should derive nothing")
	}
	// Malformed bindings → not a cell, no panic.
	if _, ok := deriveCellConfig(store.EdgeCell{CellLabel: "X", Bindings: json.RawMessage("not json")}); ok {
		t.Error("bad bindings should derive nothing")
	}
}
