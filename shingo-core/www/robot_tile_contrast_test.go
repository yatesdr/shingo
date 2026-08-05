package www

import (
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"shingo/shared"
)

// robot_tile_contrast_test.go — the robots page measured, not eyeballed.
//
// WHY THIS FILE EXISTS. shared/chip_contrast_test.go measures every health
// chip against `--surface` and `--bg`, and passes. It cannot see this page,
// because a robot tile is a THIRD kind of backdrop it was never told about: an
// opaque, saturated fill that changes with robot state. Dropping a translucent
// chip onto one let the tile colour bleed through the wash and destroyed the
// ink contrast — 26 of 30 theme x state x band combinations below AA, worst
// 2.92:1 — while every existing guard stayed green.
//
// That is the invisible-chip incident arriving through an unwatched door, and
// the lesson the style guide already draws from it applies here verbatim: a
// check named for contrast has to compute contrast, on the surface the pixel
// actually lands on. So this measures the composite as the browser paints it.
//
// The exhaustiveness assertions matter as much as the ratios: an unmeasured
// tile state is the whole failure mode, so a new `.robot-<state>` rule with no
// entry in the table below fails here rather than shipping unmeasured.

// robotTileSpec is one tile state's painted colours, per theme.
type robotTileSpec struct {
	state string
	light tilePaint
	dark  tilePaint
}

type tilePaint struct {
	fill   string // resolved hex, or a var(--token) reference
	ink    string
	border string
}

// The five states fleet.RobotStatus.State() can return. Keep in step with the
// CSS; the exhaustiveness check below fails if they diverge.
var robotTileSpecs = []robotTileSpec{
	{"ready", tilePaint{"#d1e7dd", "#0f5132", "#198754"}, tilePaint{"#0d3321", "#3fb950", "#3fb950"}},
	{"busy", tilePaint{"#cfe2ff", "#084298", "#0d6efd"}, tilePaint{"#0c2d6b", "#58a6ff", "#58a6ff"}},
	{"paused", tilePaint{"#fff3cd", "#664d03", "#ffc107"}, tilePaint{"#3d2e00", "#d29922", "#d29922"}},
	{"error", tilePaint{"#f8d7da", "#842029", "#dc3545"}, tilePaint{"#4a1219", "#f85149", "#f85149"}},
	// offline is tokenised — see the note in style.css. Resolved from tokens.css.
	{"offline", tilePaint{"#e2e3e5", "#41464b", "#6c757d"},
		tilePaint{"var(--bg-dark)", "var(--text-muted)", "var(--border)"}},
}

// The confidence chip's three bands. Ink tokens are the measured --chip-ink-*
// values; the pill is painted OPAQUE on a tile (style.css: .robot-tile
// .robot-confidence), so the backdrop is --elev-surface rather than a wash
// over whatever colour the tile happens to be.
var confidenceBands = []string{"--chip-ink-ok", "--chip-ink-near", "--chip-ink-below"}

// AA for normal text. The tile name is 0.8rem and the chip 0.74rem — both
// under the large-text cut-off, so 4.5 applies, not 3.0.
const robotTextFloor = 4.5

var (
	robotRuleRe     = regexp.MustCompile(`(?m)^\.robot-(ready|busy|paused|error|offline)\s*\{`)
	darkRobotRuleRe = regexp.MustCompile(`(?m)^\[data-theme="dark"\]\s*\.robot-(ready|busy|paused|error|offline)\s*\{`)
	tokenLineRe     = regexp.MustCompile(`(?m)^\s*(--[a-z0-9-]+)\s*:\s*([^;]+);`)
)

// themeTokens parses tokens.css into light and dark tables. Core's www tests
// have no resolver of their own and shared's is package-private, so this is a
// small local one: split on the dark selector, take declarations either side.
func themeTokens(t *testing.T) (light, dark map[string]string) {
	t.Helper()
	b, err := fs.ReadFile(shared.Files, "tokens.css")
	if err != nil {
		t.Fatalf("read shared tokens.css: %v", err)
	}
	src := string(b)

	idx := strings.Index(src, `[data-theme="dark"]`)
	if idx < 0 {
		t.Fatal(`tokens.css has no [data-theme="dark"] block — the parser has drifted and this test is a no-op`)
	}
	light, dark = map[string]string{}, map[string]string{}
	for _, m := range tokenLineRe.FindAllStringSubmatch(src[:idx], -1) {
		light[m[1]] = strings.TrimSpace(m[2])
	}
	// Dark inherits light and overrides it, which is how the cascade works.
	for k, v := range light {
		dark[k] = v
	}
	for _, m := range tokenLineRe.FindAllStringSubmatch(src[idx:], -1) {
		dark[m[1]] = strings.TrimSpace(m[2])
	}
	if len(light) == 0 || len(dark) == 0 {
		t.Fatal("parsed zero tokens; the recogniser has drifted")
	}
	return light, dark
}

// resolve follows var(--x) chains to a literal hex.
func resolve(t *testing.T, tbl map[string]string, v string) shared.RGB {
	t.Helper()
	for i := 0; i < 8; i++ {
		v = strings.TrimSpace(v)
		if !strings.HasPrefix(v, "var(") {
			break
		}
		name := strings.TrimSuffix(strings.TrimPrefix(v, "var("), ")")
		name = strings.TrimSpace(strings.SplitN(name, ",", 2)[0])
		next, ok := tbl[name]
		if !ok {
			t.Fatalf("token %s is referenced but not defined", name)
		}
		v = next
	}
	c, err := shared.ParseHexColor(v)
	if err != nil {
		t.Fatalf("cannot measure %q: %v", v, err)
	}
	return c
}

func readStyleCSS(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(staticFS, "static/style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	return string(b)
}

// Every robot state that has a rule must have a spec, and vice versa. An
// unmeasured tile is the failure this file exists for.
func TestRobotTileSpecsAreExhaustive(t *testing.T) {
	src := readStyleCSS(t)

	declared := map[string]bool{}
	for _, m := range robotRuleRe.FindAllStringSubmatch(src, -1) {
		declared[m[1]] = true
	}
	if len(declared) == 0 {
		t.Fatal("no .robot-<state> rules found in style.css — the recogniser has drifted and this test is a no-op")
	}
	inTable := map[string]bool{}
	for _, s := range robotTileSpecs {
		inTable[s.state] = true
	}
	for state := range declared {
		if !inTable[state] {
			t.Errorf(".robot-%s has a rule in style.css but no entry in robotTileSpecs, so nothing measures it.\n"+
				"  Add it: fill, ink and border for each theme.", state)
		}
	}
	for state := range inTable {
		if !declared[state] {
			t.Errorf("robotTileSpecs carries .robot-%s but style.css no longer declares it — delete the stale entry", state)
		}
	}
	// Every state also needs a dark rule, or it inherits a light fill under a
	// dark theme, which is the loudest possible way to fail.
	darkDeclared := map[string]bool{}
	for _, m := range darkRobotRuleRe.FindAllStringSubmatch(src, -1) {
		darkDeclared[m[1]] = true
	}
	for state := range declared {
		if !darkDeclared[state] {
			t.Errorf(".robot-%s has no [data-theme=\"dark\"] rule — it would wear its light fill in dark mode", state)
		}
	}
}

// The tile's own label against its own fill.
func TestRobotTileTextContrast(t *testing.T) {
	light, dark := themeTokens(t)

	for _, s := range robotTileSpecs {
		for _, tc := range []struct {
			theme string
			tbl   map[string]string
			paint tilePaint
		}{
			{"light", light, s.light},
			{"dark", dark, s.dark},
		} {
			fill := resolve(t, tc.tbl, tc.paint.fill)
			ink := resolve(t, tc.tbl, tc.paint.ink)
			if got := shared.ContrastRatio(ink, fill); got < robotTextFloor {
				t.Errorf("%s .robot-%s: label %s on fill %s = %.2f:1, want >= %.1f",
					tc.theme, s.state, ink.Hex(), fill.Hex(), got, robotTextFloor)
			}
		}
	}
}

// THE ONE THAT WOULD HAVE CAUGHT THE BUG. The confidence chip sits on a tile,
// so it is measured on a tile — every band against every state, both themes.
//
// Because the pill is painted opaque the backdrop is constant, so all thirty
// combinations reduce to six distinct ratios. That reduction IS the fix: it is
// what stops a robot changing state from changing whether its own confidence
// figure is readable.
func TestConfidenceChipContrastOnEveryTile(t *testing.T) {
	light, dark := themeTokens(t)

	src := readStyleCSS(t)
	if !strings.Contains(src, ".robot-tile .robot-confidence") {
		t.Fatal("the opaque-backdrop rule for .robot-confidence is gone — the chip is translucent on a coloured tile again")
	}

	worst := 99.0
	var worstAt string
	for _, tc := range []struct {
		theme string
		tbl   map[string]string
	}{
		{"light", light},
		{"dark", dark},
	} {
		// The pill's own opaque background, per style.css.
		back := resolve(t, tc.tbl, "var(--elev-surface)")
		for _, s := range robotTileSpecs {
			for _, inkTok := range confidenceBands {
				ink := resolve(t, tc.tbl, "var("+inkTok+")")
				got := shared.ContrastRatio(ink, back)
				if got < worst {
					worst, worstAt = got, fmt.Sprintf("%s %s on .robot-%s", tc.theme, inkTok, s.state)
				}
				if got < robotTextFloor {
					t.Errorf("%s: %s on an opaque chip over .robot-%s = %.2f:1, want >= %.1f",
						tc.theme, inkTok, s.state, got, robotTextFloor)
				}
			}
		}
	}
	t.Logf("worst confidence-chip contrast: %.2f:1 at %s", worst, worstAt)
}

// The absent marker — the em-dash a disconnected robot shows instead of a
// number — carries .robot-confidence too, so it rides the SAME opaque backdrop
// as the value chip and is measured against that, not against the tile.
//
// The first version of this test measured it against the tile fill and failed
// on seven of ten combinations. That was the test being wrong rather than the
// CSS, but it is worth keeping the memory: the reason it was plausible is that
// this element started life as plain muted text directly on the tile, which
// WOULD have failed. The assertion below pins the backdrop, so if the opaque
// rule is ever dropped the ratios stop being meaningful and this catches it.
//
// This marker matters more than its size suggests: it is the one reading on
// the page whose entire job is to say "no measurement exists". If it is hard
// to read it gets mistaken for a value, and the value it most resembles is
// zero — which means a robot with no signal reading as a robot that is lost.
func TestConfidenceAbsentMarkerContrast(t *testing.T) {
	light, dark := themeTokens(t)

	src := readStyleCSS(t)
	if !strings.Contains(src, ".robot-confidence-none") {
		t.Fatal(".robot-confidence-none has no rule — the absent marker is unstyled")
	}

	for _, tc := range []struct {
		theme string
		tbl   map[string]string
	}{
		{"light", light},
		{"dark", dark},
	} {
		back := resolve(t, tc.tbl, "var(--elev-surface)")
		ink := resolve(t, tc.tbl, "var(--chip-ink-muted)")
		if got := shared.ContrastRatio(ink, back); got < robotTextFloor {
			t.Errorf("%s: absent marker %s on %s = %.2f:1, want >= %.1f",
				tc.theme, ink.Hex(), back.Hex(), got, robotTextFloor)
		}
	}
}
