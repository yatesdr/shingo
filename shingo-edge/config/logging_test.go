package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// The four YAML shapes of logging.stderr_subsystems, with Core's semantics
// (shingo-core/config/logging_test.go): absent is the defaults, [all] is no
// restriction, [] and null mute the mirror.

func loadYAML(t *testing.T, body string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shingoedge.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestStderrSubsystems_AbsentUsesDefaults(t *testing.T) {
	cfg := loadYAML(t, "poll_rate: 1s\n")

	got := cfg.Logging.ResolveStderrSubsystems()
	if !reflect.DeepEqual(got, DefaultStderrSubsystems()) {
		t.Fatalf("absent key should fall back to defaults, got %v", got)
	}
	for _, s := range []string{"outbox", "inventory_delta", "kafka", "reporter"} {
		if slices.Contains(got, s) {
			t.Errorf("%q must be off the default allow-list, got %v", s, got)
		}
	}
}

func TestStderrSubsystems_ExplicitList(t *testing.T) {
	cfg := loadYAML(t, "logging:\n  stderr_subsystems: [plc, outbox]\n")

	if got := cfg.Logging.ResolveStderrSubsystems(); !reflect.DeepEqual(got, []string{"plc", "outbox"}) {
		t.Fatalf("explicit list not honoured, got %v", got)
	}
}

func TestStderrSubsystems_AllMeansNoRestriction(t *testing.T) {
	cfg := loadYAML(t, "logging:\n  stderr_subsystems: [all]\n")

	if got := cfg.Logging.ResolveStderrSubsystems(); got != nil {
		t.Fatalf(`"all" must resolve to nil (no restriction), got %v`, got)
	}
}

func TestStderrSubsystems_EmptyAndNullMuteTheMirror(t *testing.T) {
	for name, body := range map[string]string{
		"empty list": "logging:\n  stderr_subsystems: []\n",
		"null":       "logging:\n  stderr_subsystems:\n",
	} {
		t.Run(name, func(t *testing.T) {
			got := loadYAML(t, body).Logging.ResolveStderrSubsystems()
			if got == nil {
				t.Fatal("must not resolve to nil — nil means mirror everything")
			}
			if len(got) != 0 {
				t.Fatalf("expected an empty allow-list, got %v", got)
			}
		})
	}
}
