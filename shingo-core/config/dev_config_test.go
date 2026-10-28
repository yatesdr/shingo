package config

import (
	"path/filepath"
	"testing"
	"time"
)

// TestDevYAMLParses verifies shingocore.dev.yaml parses with the expected sim
// knobs. Unknown YAML keys are silently ignored, so we assert expected VALUES
// are present — that catches a mistyped key (it would leave the default/zero).
func TestDevYAMLParses(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "shingocore.dev.yaml"))
	if err != nil {
		t.Fatalf("load dev config: %v", err)
	}
	if !cfg.Sim.Enabled {
		t.Error("sim.enabled should be true in dev config")
	}
	// The keys the file sets, not defaults: transit_time and jitter_pct are
	// unset there (a uniform transit_min..transit_max draw never reads them).
	if cfg.Sim.Speed != 10 {
		t.Errorf("sim.speed = %v, want 10", cfg.Sim.Speed)
	}
	if cfg.Sim.TransitMin != 15*time.Second || cfg.Sim.TransitMax != 20*time.Second {
		t.Errorf("sim.transit_min/max = %v/%v, want 15s/20s", cfg.Sim.TransitMin, cfg.Sim.TransitMax)
	}
	if cfg.Sim.FleetSize != 20 {
		t.Errorf("sim.fleet_size = %d, want 20", cfg.Sim.FleetSize)
	}
	if cfg.Web.Port != 8083 {
		t.Errorf("web.port = %d, want 8083", cfg.Web.Port)
	}
	if len(cfg.Messaging.Kafka.Brokers) != 1 || cfg.Messaging.Kafka.Brokers[0] != "kafka:9092" {
		t.Errorf("kafka brokers = %v, want [kafka:9092]", cfg.Messaging.Kafka.Brokers)
	}
}
