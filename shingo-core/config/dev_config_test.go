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
	if cfg.Sim.TransitTime != 5*time.Second {
		t.Errorf("sim.transit_time = %v, want 5s", cfg.Sim.TransitTime)
	}
	if cfg.Sim.JitterPct != 0.2 {
		t.Errorf("sim.jitter_pct = %v, want 0.2", cfg.Sim.JitterPct)
	}
	if cfg.Web.Port != 8083 {
		t.Errorf("web.port = %d, want 8083", cfg.Web.Port)
	}
	if len(cfg.Messaging.Kafka.Brokers) != 1 || cfg.Messaging.Kafka.Brokers[0] != "kafka:9092" {
		t.Errorf("kafka brokers = %v, want [kafka:9092]", cfg.Messaging.Kafka.Brokers)
	}
}
