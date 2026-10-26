package www

import (
	"io/fs"
	"regexp"
	"strconv"
	"testing"

	"shingo/shared"
)

// rowFlashRe reads the task board's new-row flash at its peak, the first
// keyframe of row-flash in dashboard.css.
var rowFlashRe = regexp.MustCompile(`@keyframes row-flash\s*\{\s*0%\s*\{\s*background:\s*rgba\((\d+),\s*(\d+),\s*(\d+),\s*([0-9.]+)\)`)

// TestRouteStripContrast: the route strip colours one thing, where the robot
// is, and in the plant's accent (--accent, the indigo of the primary buttons
// and the active tab), never a hue of its own. Everything else on the strip is
// the text ramp. Measured in both themes:
//
//   - the accent drawn as a fill (the stop the robot is at, and the arrow it
//     travels between two stops) carries its own ink, at the 4.5:1 floor for
//     normal text;
//   - that fill stands off whatever is behind it at the 3:1 floor for a
//     non-text component: the card, the page, the wall board's background, and
//     the board's new-row flash at its peak composited over that background;
//   - the accent drawn as text (the pop-up timeline's stop, on its card) at
//     4.5:1.
//
// The arrow is a fill rather than an accent glyph because a glyph cannot clear
// 4.5:1 on the flash's peak in either theme.
func TestRouteStripContrast(t *testing.T) {
	light, dark := themeTokens(t)
	css, err := fs.ReadFile(staticFS, "static/dashboard.css")
	if err != nil {
		t.Fatal(err)
	}
	m := rowFlashRe.FindStringSubmatch(string(css))
	if m == nil {
		t.Fatal("dashboard.css has no row-flash keyframe in the form this test reads; it measures nothing")
	}
	num := func(s string) float64 {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			t.Fatalf("row-flash keyframe value %q: %v", s, err)
		}
		return v
	}
	flash := shared.RGB{R: num(m[1]), G: num(m[2]), B: num(m[3])}
	alpha := num(m[4])

	for _, theme := range []struct {
		name string
		tbl  map[string]string
	}{{"light", light}, {"dark", dark}} {
		tok := func(name string) shared.RGB { return resolve(t, theme.tbl, "var("+name+")") }
		if tok("--route-here") != tok("--accent") {
			t.Errorf("%s: --route-here is %s, not the accent %s: the strip has one colour and it is the plant's",
				theme.name, tok("--route-here").Hex(), tok("--accent").Hex())
		}
		for _, retired := range []string{"--route-next", "--route-final"} {
			if _, ok := theme.tbl[retired]; ok {
				t.Errorf("%s: %s is a hue the strip no longer has", theme.name, retired)
			}
		}
		fill, ink := tok("--route-here"), tok("--route-on-here")
		if got := shared.ContrastRatio(ink, fill); got < 4.5 {
			t.Errorf("%s: --route-on-here %s on the accent %s = %.2f:1, want >= 4.5", theme.name, ink.Hex(), fill.Hex(), got)
		}
		wall := tok("--elev-base") // dashboard.css --dash-bg
		behind := map[string]shared.RGB{
			"card": tok("--elev-surface"), "page": tok("--elev-base"), "wall board": wall,
			"new-row flash at its peak": shared.CompositeOver(flash, alpha, wall),
		}
		for where, bg := range behind {
			if got := shared.ContrastRatio(fill, bg); got < 3 {
				t.Errorf("%s: the accent fill %s on the %s %s = %.2f:1, want >= 3", theme.name, fill.Hex(), where, bg.Hex(), got)
			}
		}
		if got := shared.ContrastRatio(fill, tok("--elev-surface")); got < 4.5 {
			t.Errorf("%s: the accent as text on the card = %.2f:1, want >= 4.5", theme.name, got)
		}
		for _, ink := range []string{"--route-passed", "--route-remaining", "--route-chip-ink"} {
			on := map[string]string{"--route-chip-ink": "--route-chip"}[ink]
			if on == "" {
				on = "--elev-surface"
			}
			if got := shared.ContrastRatio(tok(ink), tok(on)); got < 4.5 {
				t.Errorf("%s: %s on %s = %.2f:1, want >= 4.5", theme.name, ink, on, got)
			}
		}
	}
}
