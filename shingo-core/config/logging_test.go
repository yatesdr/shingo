package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// The four YAML shapes of logging.stderr_subsystems, pinned because the
// difference between "absent" (defaults) and "null" (mute) is invisible in
// the struct and one of them is the escape hatch someone will reach for
// mid-incident.

func loadYAML(t *testing.T, body string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shingocore.yaml")
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
	cfg := loadYAML(t, "web:\n  port: 8083\n")

	got := cfg.Logging.ResolveStderrSubsystems()
	if !reflect.DeepEqual(got, DefaultStderrSubsystems()) {
		t.Fatalf("absent key should fall back to defaults, got %v", got)
	}
	// countgroup and rds are 75% of the journal between them (334,361 and
	// 125,817 lines/day at Springfield). Both stay off by default.
	for _, muted := range []string{"countgroup", "rds"} {
		if slices.Contains(got, muted) {
			t.Fatalf("%q must be off the default allow-list, got %v", muted, got)
		}
	}
}

func TestStderrSubsystems_ExplicitList(t *testing.T) {
	cfg := loadYAML(t, "logging:\n  stderr_subsystems: [dispatch, rds]\n")

	got := cfg.Logging.ResolveStderrSubsystems()
	if !reflect.DeepEqual(got, []string{"dispatch", "rds"}) {
		t.Fatalf("explicit list not honoured, got %v", got)
	}
}

// "all" is the incident escape hatch: restore the pre-2026-07-25 firehose
// with a config edit and a restart, no rebuild.
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
			cfg := loadYAML(t, body)
			got := cfg.Logging.ResolveStderrSubsystems()
			if got == nil {
				t.Fatal("must not resolve to nil — nil means mirror everything")
			}
			if len(got) != 0 {
				t.Fatalf("expected an empty allow-list, got %v", got)
			}
		})
	}
}
