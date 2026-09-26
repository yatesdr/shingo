package www

import (
	"regexp"
	"strings"
	"testing"

	"shingo/shared"
)

// node_tile_contrast_test.go — the Nodes page tile names, measured.
//
// A node tile's name sat in white on every state fill by habit. Measured, white
// on --status-staged-dot is 2.5:1 in both themes and white on dark --success is
// 2.5:1, and on the two hatched states no single ink clears 4.5:1 on both
// stripes. Each solid fill now names its own ink token, and a hatched tile puts
// the name on an opaque --elev-surface plate. This file holds both: every
// state's rule must carry the paint listed here, and every pairing must clear
// AA in both themes.

type nodeTileSpec struct {
	rule string // the selector whose block must carry the declarations
	fill string // var(--token) the name is painted on
	ink  string // var(--token) the name is painted in
	want []string
}

var nodeTileSpecs = []nodeTileSpec{
	{".node-tile", "var(--bg-dark)", "var(--chip-ink-muted)",
		[]string{"background: var(--bg-dark)", "color: var(--chip-ink-muted)"}},
	{".tile-has-payload", "var(--success)", "var(--on-success-fill)",
		[]string{"background: var(--success)", "color: var(--on-success-fill)"}},
	{".tile-staged", "var(--status-staged-dot)", "var(--on-staged-fill)",
		[]string{"background: var(--status-staged-dot)", "color: var(--on-staged-fill)"}},
	// The plate: one rule serves both hatched states.
	{".tile-empty-bin > .tile-text, .tile-maintenance > .tile-text", "var(--elev-surface)", "var(--text)",
		[]string{"background: var(--elev-surface)", "color: var(--text)"}},
}

// AA for normal text: the name is 0.75rem bold, under the large-text cut-off.
const nodeTileTextFloor = 4.5

func TestNodeTileStatesCarryTheirMeasuredPaint(t *testing.T) {
	src := readStyleCSS(t)
	for _, s := range nodeTileSpecs {
		re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(s.rule) + `[^{]*\{([^}]*)\}`)
		m := re.FindStringSubmatch(src)
		if m == nil {
			t.Errorf("style.css has no rule starting %q — the tile paint moved and nothing measures it", s.rule)
			continue
		}
		for _, d := range s.want {
			if !strings.Contains(m[1], d) {
				t.Errorf("%s: want %q in its rule, got:%s", s.rule, d, m[1])
			}
		}
	}
	// The hatched fills must not set their own ink: the plate carries it.
	for _, hatch := range []string{".tile-empty-bin", ".tile-maintenance"} {
		re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(hatch) + `\s*\{([^}]*)\}`)
		if m := re.FindStringSubmatch(src); m != nil && strings.Contains(m[1], "color:") {
			t.Errorf("%s sets an ink on the stripes; the name's ink belongs on the plate", hatch)
		}
	}
}

func TestNodeTileNameContrast(t *testing.T) {
	light, dark := themeTokens(t)
	for _, s := range nodeTileSpecs {
		for _, th := range []struct {
			name string
			tbl  map[string]string
		}{{"light", light}, {"dark", dark}} {
			fill := resolve(t, th.tbl, s.fill)
			ink := resolve(t, th.tbl, s.ink)
			if got := shared.ContrastRatio(ink, fill); got < nodeTileTextFloor {
				t.Errorf("%s %s: name %s on %s = %.2f:1, want >= %.1f",
					th.name, s.rule, ink.Hex(), fill.Hex(), got, nodeTileTextFloor)
			}
		}
	}
}
