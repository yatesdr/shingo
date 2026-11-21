package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_ARetiredKeyStillLoads: the config file is read non-strict, so a
// deployed file that still carries a retired key (loaders_multi_window, gone
// with its plant-wide funnel brake; namespace, line_id and
// messaging.station_id, the identity's leftovers) loads, and the keys around
// it still read.
func TestLoad_ARetiredKeyStillLoads(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "shingoedge.yaml")
	body := "station_uid: stn-0123456789abcdef\nnamespace: plant1\nline_id: line9\n" +
		"messaging:\n  station_id: plant1.line9\n  orders_topic: orders.x\nloaders_multi_window: false\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("a config carrying retired keys did not load: %v", err)
	}
	if cfg.StationID() != "stn-0123456789abcdef" || cfg.Messaging.OrdersTopic != "orders.x" {
		t.Errorf("station = %q, orders topic = %q: the keys beside the retired ones did not read",
			cfg.StationID(), cfg.Messaging.OrdersTopic)
	}
}

// TestLoad_TheRetiredStationIDIsNoIdentity: messaging.station_id is read by
// nothing, and nothing copies it into station_uid at load (no shim). A file
// whose only identity it is loads with none, and the boot then mints a new
// one (main.ensureIdentity), so that box would come up as a new station.
func TestLoad_TheRetiredStationIDIsNoIdentity(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "shingoedge.yaml")
	if err := os.WriteFile(path, []byte("messaging:\n  station_id: plant1.line9\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.StationID(); got != "" {
		t.Errorf("StationID() = %q, want none: the retired key stood in for station_uid", got)
	}
}
