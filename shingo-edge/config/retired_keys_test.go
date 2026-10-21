package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_ARetiredKeyStillLoads: the config file is read non-strict, so a
// deployed file that still carries a retired key (loaders_multi_window, gone
// with its plant-wide funnel brake) loads, and the keys around it still read.
func TestLoad_ARetiredKeyStillLoads(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "shingoedge.yaml")
	body := "namespace: plant1\nline_id: line9\nloaders_multi_window: false\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("a config carrying loaders_multi_window did not load: %v", err)
	}
	if cfg.Namespace != "plant1" || cfg.LineID != "line9" {
		t.Errorf("station = %s.%s, want plant1.line9", cfg.Namespace, cfg.LineID)
	}
}
