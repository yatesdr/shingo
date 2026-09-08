package main

import (
	"os"
	"path/filepath"
	"testing"

	"shingoedge/config"
)

// adoptFixture returns a config backed by a real file, because the whole point
// of adoption is that the value reaches DISK — an in-memory-only change would
// vanish on the restart it asks for.
func adoptFixture(t *testing.T, existing string) (*config.Config, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shingoedge.yaml")
	cfg := &config.Config{Timezone: existing}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	return cfg, path
}

func savedTimezone(t *testing.T, path string) string {
	t.Helper()
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	return reloaded.Timezone
}

// TestAdoptPlantTimezone_FillsABlank is the feature: a site is configured once
// on Core and every edge inherits it, instead of the zone being typed into each
// box's yaml.
func TestAdoptPlantTimezone_FillsABlank(t *testing.T) {
	cfg, path := adoptFixture(t, "")

	adoptPlantTimezone(cfg, path, "America/Chicago")

	if got := cfg.Timezone; got != "America/Chicago" {
		t.Errorf("in-memory timezone = %q, want America/Chicago", got)
	}
	if got := savedTimezone(t, path); got != "America/Chicago" {
		t.Errorf("persisted timezone = %q, want America/Chicago — an adoption that does not "+
			"reach disk is undone by the restart it asks for", got)
	}
}

// TestAdoptPlantTimezone_NeverOverridesALocalValue is the load-bearing rule.
// Config arriving over a network must not overwrite a zone somebody typed into
// this box — the wire distributes a default, it does not override a decision.
func TestAdoptPlantTimezone_NeverOverridesALocalValue(t *testing.T) {
	cfg, path := adoptFixture(t, "America/Denver")

	adoptPlantTimezone(cfg, path, "America/Chicago")

	if got := cfg.Timezone; got != "America/Denver" {
		t.Errorf("timezone = %q, want America/Denver — Core overwrote a local setting", got)
	}
	if got := savedTimezone(t, path); got != "America/Denver" {
		t.Errorf("persisted timezone = %q, want America/Denver", got)
	}
}

// TestAdoptPlantTimezone_EmptyOfferChangesNothing pins the explicit-only
// contract from the other end. Core sends its CONFIGURED zone, so an empty
// offer means "Core was not told either" — not "Core says UTC". Adopting
// anything here would clear the blank-zone warnings across the fleet and make a
// value nobody chose look chosen.
func TestAdoptPlantTimezone_EmptyOfferChangesNothing(t *testing.T) {
	cfg, path := adoptFixture(t, "")

	adoptPlantTimezone(cfg, path, "")

	if got := cfg.Timezone; got != "" {
		t.Errorf("timezone = %q, want empty — an unconfigured Core must propagate nothing", got)
	}
	if got := savedTimezone(t, path); got != "" {
		t.Errorf("persisted timezone = %q, want empty", got)
	}
}

// TestAdoptPlantTimezone_RejectsAZoneThatDoesNotParse keeps a bad value out of
// the yaml. Written unvalidated it would sit there and only surface at the next
// boot, as a logged fallback on a headless box nobody reads until the clock is
// wrong.
func TestAdoptPlantTimezone_RejectsAZoneThatDoesNotParse(t *testing.T) {
	cfg, path := adoptFixture(t, "")

	adoptPlantTimezone(cfg, path, "Mars/Olympus_Mons")

	if got := cfg.Timezone; got != "" {
		t.Errorf("timezone = %q, want empty — an unparseable zone was accepted", got)
	}
	if got := savedTimezone(t, path); got != "" {
		t.Errorf("persisted timezone = %q, want empty", got)
	}
}

// TestAdoptPlantTimezone_UnwritableConfigLeavesNoPhantom covers the failure
// path. If the value cannot be persisted it must not stay in memory either:
// this process would otherwise report a zone to Core's /edges table that is not
// on disk and would vanish on restart. Reverting means the next heartbeat
// simply offers it again.
func TestAdoptPlantTimezone_UnwritableConfigLeavesNoPhantom(t *testing.T) {
	cfg := &config.Config{}
	// A path inside a file (not a directory) cannot be created on any OS.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	unwritable := filepath.Join(blocker, "shingoedge.yaml")

	adoptPlantTimezone(cfg, unwritable, "America/Chicago")

	if got := cfg.Timezone; got != "" {
		t.Errorf("timezone = %q, want empty — a zone that could not be saved was left in "+
			"memory, so this edge would report a setting that does not survive a restart", got)
	}
}
