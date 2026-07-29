package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// The hysteresis margin is a KNOB with a conservative default, not a constant.
// The design says "close above threshold + margin" and names no value, so
// nothing here should read as a derived number — these pin the shape (scales
// with the reorder point, never zero, configurable) rather than blessing 10%.
func TestHysteresisMargin(t *testing.T) {
	var cfg Config

	// Default: 10% of the reorder point.
	if got := cfg.HysteresisMargin(50); got != 5 {
		t.Errorf("default margin for reorder_point=50 = %d, want 5", got)
	}
	// Floored at 1. A reorder point of 5 would otherwise get no hysteresis at
	// all — and a small reorder point is exactly what ordinary tick noise
	// crosses back and forth.
	if got := cfg.HysteresisMargin(5); got != MinHysteresisUOP {
		t.Errorf("margin for reorder_point=5 = %d, want the floor %d", got, MinHysteresisUOP)
	}
	// A claim with no reorder point still gets a margin rather than zero.
	if got := cfg.HysteresisMargin(0); got != MinHysteresisUOP {
		t.Errorf("margin for reorder_point=0 = %d, want the floor %d", got, MinHysteresisUOP)
	}

	// Configured: a plant that reports flapping raises it, without a rebuild.
	pct := 40.0
	cfg.Demand.HysteresisPercent = &pct
	if got := cfg.HysteresisMargin(50); got != 20 {
		t.Errorf("configured 40%% margin for reorder_point=50 = %d, want 20", got)
	}

	// A nonsense value falls back to the floor rather than producing a negative
	// margin, which would make the close condition looser than the open one and
	// leave episodes that can never close.
	neg := -5.0
	cfg.Demand.HysteresisPercent = &neg
	if got := cfg.HysteresisMargin(50); got < MinHysteresisUOP {
		t.Errorf("negative configuration produced margin %d — an episode must never be unclosable", got)
	}
}

// TestDefaultsSnapshotIsCurrent is Phase 1's staleness test applied to
// BEHAVIOUR instead of schema, and the parallel is exact.
//
// The shipped defaults are the schema of how this system behaves when nobody
// has said otherwise. There was no file you could open to see them, they are
// scattered across Defaults() and accessor fallbacks, and retuning one is a
// one-character edit that is invisible in review. Nobody catches a default
// drifting by reading code; they catch it by seeing a line change colour.
//
// This pins the SHIPPED default, not a plant's yaml. A plant overriding
// hysteresis_percent locally does not touch this file — which is right, because
// a local tuning decision is local. Changing what EVERYONE gets is what should
// require acknowledgement.
func TestDefaultsSnapshotIsCurrent(t *testing.T) {
	// DefaultsSnapshotPath is module-root relative; the test runs in config/.
	path := filepath.Join("..", filepath.FromSlash(DefaultsSnapshotPath))
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v\n\nrun `%s` and commit the result", DefaultsSnapshotPath, err, DefaultsRegenCommand)
	}
	want := strings.ReplaceAll(string(committed), "\r\n", "\n")
	got := RenderDefaults()
	if got == want {
		return
	}
	t.Fatalf("the shipped defaults have changed and the snapshot has not — run `%s` and commit it.\n\n"+
		"If this is deliberate, the diff is the point: somebody should be able to ask what depended on the old value.\n\n%s",
		DefaultsRegenCommand, firstDefaultsDiff(want, got))
}

// The rendering must be identical run to run, or the staleness test fails
// forever on a tree nobody has touched. Defaults() GENERATES a random session
// secret, which is exactly the trap — secrets are redacted, both for this and
// because a snapshot that renders them is a snapshot that commits them.
func TestRenderDefaults_IsDeterministicAndRedactsSecrets(t *testing.T) {
	first, second := RenderDefaults(), RenderDefaults()
	if first != second {
		t.Fatalf("RenderDefaults is not deterministic:\n%s", firstDefaultsDiff(first, second))
	}
	// Defaults() generates a session secret; it must never reach the file.
	cfg := Defaults()
	if cfg.Web.SessionSecret == "" {
		t.Skip("no generated session secret in this build — nothing to leak")
	}
	if strings.Contains(first, cfg.Web.SessionSecret) {
		t.Error("the generated session secret was rendered into the snapshot — it would be committed to git")
	}
}

func firstDefaultsDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) && i < len(bl); i++ {
		if al[i] != bl[i] {
			return "- committed: " + al[i] + "\n+ built:     " + bl[i]
		}
	}
	if len(al) != len(bl) {
		return "(the two renderings have different lengths: " +
			strconv.Itoa(len(al)) + " vs " + strconv.Itoa(len(bl)) + " lines)"
	}
	return "(identical)"
}

// THE GOLDEN-FILE GENERATOR IS ITSELF CODE WHOSE FAILURE MODE IS SILENT, so it
// needs tests more than most. Both ways it has already broken were invisible:
// a non-deterministic value fails the staleness test forever on a tree nobody
// touched, and an unredacted secret is a credential in git that nobody notices.
//
// Rendering twice and comparing catches random values, timestamps and
// map-iteration order all at once — whatever the source, without having to
// anticipate which.
func TestRenderDefaults_Deterministic(t *testing.T) {
	first, second := RenderDefaults(), RenderDefaults()
	if first != second {
		t.Fatalf("RenderDefaults is not deterministic — the staleness test would fail forever:\n%s",
			firstDefaultsDiff(first, second))
	}
}

// The output scan is the backstop behind the struct tag and the name
// heuristic. It is deliberately dumb: it does not know what a secret is, only
// that a long opaque blob has no business in a defaults file.
func TestRenderDefaults_NoUnredactedSecrets(t *testing.T) {
	if bad := ScanForUnredactedSecrets(RenderDefaults()); len(bad) > 0 {
		t.Errorf("credential-shaped values reached the snapshot — these would be committed to git:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// And the scan has to be able to FAIL, or it is decoration. A generated secret
// rendered verbatim is exactly the thing it exists to catch.
func TestScanForUnredactedSecrets_CatchesALeak(t *testing.T) {
	leaked := "web.session_secret = d9c7c94ef0d19db0f5be0f88f44494d3456162d74abcafc507d38e2c5d8db10e"
	if bad := ScanForUnredactedSecrets(leaked); len(bad) == 0 {
		t.Error("the scan did not catch a rendered 64-char secret — it would pass anything")
	}
	// And it must not cry wolf on ordinary config.
	ordinary := "backup.s3.region = us-east-1\nmessaging.orders_topic = shingo.orders\nposll_rate = 1s"
	if bad := ScanForUnredactedSecrets(ordinary); len(bad) > 0 {
		t.Errorf("the scan flagged ordinary values, which would make it noise: %v", bad)
	}
}
