package shared

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"testing"

	"shingo/protocol"
)

// Contrast guards for the shared token surfaces.
//
// The guard this repo already had for the invisible-chip incident —
// shingo-core/www/chip_drift_test.go — asserts that every `.chip-<x>` has a
// rule somewhere. Its own header says the incident it came from was
// `.chip-err` shipping at 1.2:1. A presence check cannot see 1.2:1. The rule
// existed both times; it was the ratio that was wrong, so the guard named for
// the incident could not have caught the incident.
//
// These tests compute the ratio. Every pair below is read out of the
// stylesheet, resolved through the real cascade, and measured; nothing is
// hardcoded except the WCAG floors and the reference values that pin the
// arithmetic.

// TestContrastMathMatchesWCAGReference pins the colour maths against W3C's
// published worked examples before any other test in this file is believed.
//
// This is the load-bearing test of the pair. Every other assertion here is
// "the computed ratio is at least N" — and a luminance function with the
// linearisation knee dropped, or the coefficients transposed, still returns
// plausible numbers in the 1..21 band. It would fail nothing and check
// nothing. Contrast is exactly the kind of quantity where a wrong
// implementation is invisible in the output.
func TestContrastMathMatchesWCAGReference(t *testing.T) {
	cases := []struct {
		fg, bg string
		want   float64
		note   string
	}{
		{"#000000", "#ffffff", 21.0, "the definitional maximum"},
		{"#ffffff", "#ffffff", 1.0, "the definitional minimum"},
		{"#777777", "#ffffff", 4.4781, "W3C's own AA borderline example"},
		{"#0000ff", "#ffffff", 8.5925, "pure blue on white, exercises the 0.0722 weight alone"},
		{"#ff0000", "#ffffff", 3.9985, "pure red on white, exercises the 0.2126 weight alone"},
		{"#008000", "#ffffff", 5.1374, "mid green, lands above the knee on one channel only"},
		{"#0a0a0a", "#000000", 1.0607, "both operands below the 0.03928 knee — the linear branch, which a gamma-only implementation gets wrong"},
	}
	for _, c := range cases {
		fg := mustColor(t, c.fg)
		bg := mustColor(t, c.bg)
		got := ContrastRatio(fg, bg)
		if math.Abs(got-c.want) > 0.0005 {
			t.Errorf("ContrastRatio(%s, %s) = %.4f, want %.4f (%s)\n"+
				"  The WCAG arithmetic in shared/contrast.go is wrong. Every other assertion in this file is measured with it and is therefore meaningless until this passes.",
				c.fg, c.bg, got, c.want, c.note)
		}
		if r := ContrastRatio(bg, fg); math.Abs(r-got) > 1e-12 {
			t.Errorf("ContrastRatio is not symmetric for (%s, %s): %.6f vs %.6f", c.fg, c.bg, got, r)
		}
	}
}

// TestSignalBadgeTextClearsAA measures every badge in shared/status-classes.css
// — text colour against the pill background it is printed on — in both themes,
// and fails below WCAG AA for normal text.
//
// The file's own header already claims this: "All text-on-pill pairs clear
// WCAG AA (>=4.5:1) in both themes." Until now that was a sentence. Two of the
// individual rules go further and quote a specific figure in their comment
// (faulted "5.40:1 on the pill (AA)", dark faulted "7.92:1 (AAA)") — numbers
// somebody computed once, by hand, off to the side, and which nothing has
// re-checked since.
//
// The margin is thinner than the comment suggests. Light `sourcing` sits at
// 4.63:1 — thirteen hundredths above the floor. A half-step darkening of that
// sand background to make it read as "warmer" would put it under, and there is
// no eyeball that catches 4.4 versus 4.6 on a shop-floor LCD.
//
// Statuses are enumerated from protocol.AllStatuses() rather than from the
// stylesheet, so a status added to the protocol whose rule is missing fails
// here as "unresolved", not as a silent skip. A check has to know whether it
// had the input to check.
func TestSignalBadgeTextClearsAA(t *testing.T) {
	css := readShared(t, "status-classes.css")
	light, dark := badgePalette(t, css)

	var measured int
	seen := map[string]bool{}
	for _, theme := range []struct {
		name  string
		decls map[string]map[string]string
	}{{"light", light}, {"dark", dark}} {
		for _, s := range protocol.AllStatuses() {
			class := "badge-" + string(s)
			d, ok := theme.decls[class]
			if !ok {
				t.Errorf("%s theme: .%s has no rule in status-classes.css, so its contrast could not be measured at all.\n"+
					"  Add the rule beside its lifecycle neighbours; TestStatusClassesCoversAllProtocolStatuses covers presence, this test covers whether it is readable.",
					theme.name, class)
				continue
			}
			fgHex, bgHex := d["color"], d["background"]
			if !isHex(fgHex) || !isHex(bgHex) {
				t.Errorf("%s theme: .%s resolves to color=%q background=%q — not both literal hex, so no ratio can be computed.\n"+
					"  Badge colours are deliberately literal (they are a floor-display palette, not themable chrome); keep them that way or teach badgePalette to resolve the new form.",
					theme.name, class, fgHex, bgHex)
				continue
			}
			fg, bg := mustColor(t, fgHex), mustColor(t, bgHex)
			got := ContrastRatio(fg, bg)
			measured++
			seen[theme.name+"|"+bgHex+"|"+fgHex] = true
			if got < AANormalText {
				t.Errorf("Signal contrast FAIL — %s theme, .%s: text %s on background %s is %.2f:1, below the %.2f:1 floor (WCAG 2.2 SC 1.4.3, AA, normal text; the badge label is 0.8rem/600, under the large-text cut-off).\n"+
					"  Darken %s or lighten %s in shared/status-classes.css until the ratio clears %.2f.",
					theme.name, class, fgHex, bgHex, got, AANormalText,
					fgHex, bgHex, AANormalText)
			}
		}
	}

	// The three non-lifecycle chips share the pill and the same read-at-a-
	// glance job, so they are held to the same floor. They are allowlisted
	// out of the status-drift test, which is how a badge ends up guarded by
	// nothing at all.
	for _, theme := range []struct {
		name  string
		decls map[string]map[string]string
	}{{"light", light}, {"dark", dark}} {
		for _, class := range []string{"badge-info", "badge-warn", "badge-muted"} {
			d, ok := theme.decls[class]
			if !ok {
				t.Errorf("%s theme: .%s has no rule — status-classes.go's allowlist names it, so it is expected to exist", theme.name, class)
				continue
			}
			fgHex, bgHex := d["color"], d["background"]
			if !isHex(fgHex) || !isHex(bgHex) {
				t.Errorf("%s theme: .%s is not a literal hex pair (color=%q background=%q)", theme.name, class, fgHex, bgHex)
				continue
			}
			got := ContrastRatio(mustColor(t, fgHex), mustColor(t, bgHex))
			measured++
			if got < AANormalText {
				t.Errorf("Signal contrast FAIL — %s theme, .%s: text %s on background %s is %.2f:1, below the %.2f:1 floor (WCAG 2.2 SC 1.4.3, AA, normal text).\n"+
					"  Darken %s or lighten %s in shared/status-classes.css until the ratio clears %.2f.",
					theme.name, class, fgHex, bgHex, got, AANormalText, fgHex, bgHex, AANormalText)
			}
		}
	}

	if measured == 0 {
		t.Fatal("no badge pairs measured at all — badgePalette has drifted from status-classes.css, which makes this test a no-op")
	}
	if len(seen) < 20 {
		t.Errorf("only %d distinct (background, text) Signal pairs across both themes; the palette declares 13 per theme.\n"+
			"  A collapse this large means the cascade merge is wrong, not that the palette shrank.", len(seen))
	}
}

// vizRole is what a --viz-* token PAINTS. The WCAG floor is a property of the
// role, not of the file the token lives in, so classifying is the whole job —
// holding a heatmap's lightest step to 4.5:1 would forbid the ramp from having
// a light end, which is the ramp's entire encoding.
type vizRole int

const (
	// vizText: chart ink — hero numbers, axis and series labels. Read as
	// text, so SC 1.4.3 applies at the card surface it is painted on.
	vizText vizRole = iota
	// vizMark: a categorical or semantic series colour. A graphical object
	// required to understand the chart, so SC 1.4.11 at 3:1.
	vizMark
	// vizRampEnd: the high-magnitude end of a sequential or diverging ramp.
	// This is the step that MUST read — it is where the outliers land.
	vizRampEnd
	// vizRampInterior: the low and middle steps of a ramp. Deliberately NOT
	// given a contrast floor against the surface: "close to the surface"
	// is how a sequential ramp encodes "close to zero", and SC 1.4.11
	// exempts a presentation that is essential to the information. Ordering
	// is what these are held to instead — see TestVizSequentialRampIsOrdered.
	vizRampInterior
)

// vizRoles classifies every --viz-* token. Missing entries fail the test
// rather than being skipped: an unclassified token is one nobody decided the
// role of, and defaulting it either way is a decision made by accident.
var vizRoles = map[string]vizRole{
	"--viz-primary":   vizText,
	"--viz-secondary": vizText,

	"--viz-accent": vizMark,
	"--viz-indigo": vizMark,
	"--viz-violet": vizMark,
	"--viz-sky":    vizMark,
	"--viz-teal":   vizMark,
	"--viz-amber":  vizMark,
	"--viz-coral":  vizMark,
	"--viz-green":  vizMark,

	"--viz-seq-1": vizRampInterior,
	"--viz-seq-2": vizRampInterior,
	"--viz-seq-3": vizRampInterior,
	"--viz-seq-4": vizRampInterior,
	"--viz-seq-5": vizRampEnd,

	"--viz-div-neg-2": vizRampEnd,
	"--viz-div-neg-1": vizRampInterior,
	"--viz-div-mid":   vizRampInterior,
	"--viz-div-pos-1": vizRampInterior,
	"--viz-div-pos-2": vizRampEnd,
}

// TestVizTokenContrastAgainstSurfaces measures every --viz-* token against
// both surfaces it can be painted on — --surface (cards, where every chart in
// this app currently lives) and --bg (the page behind them) — in both themes.
//
// Two surfaces and not one because the tokens are theme-scoped but not
// container-scoped: nothing stops a sparkline being dropped straight onto the
// page, and `--viz-teal` at 3.74:1 on a card is 3.46:1 on the page. Both are
// above the mark floor today; the point of measuring both is that the second
// number exists at all.
//
// Chart text is held to AA against --surface and only to the non-text floor
// against --bg, because charts are painted inside .card. That is a real
// dependency and worth writing down: --viz-secondary aliases --text-muted,
// which is 4.69:1 on a card and 4.33:1 on the page. Move a chart's labels out
// of a card and they stop meeting AA — not because this token changed, but
// because --text-muted has never met AA on --bg for anything.
func TestVizTokenContrastAgainstSurfaces(t *testing.T) {
	src := readShared(t, "tokens.css")
	themes := tokenThemes(t, src)

	// Exhaustiveness first: an unclassified token is an unchecked token.
	declared := map[string]bool{}
	for name := range themes["light"] {
		if strings.HasPrefix(name, "--viz-") {
			declared[name] = true
		}
	}
	if len(declared) == 0 {
		t.Fatal("no --viz-* tokens found in tokens.css — the parser has drifted and this test is a no-op")
	}
	for name := range declared {
		if _, ok := vizRoles[name]; !ok {
			t.Errorf("--viz token %s has no entry in vizRoles, so nothing decided what floor applies to it.\n"+
				"  Classify it: vizText (chart ink, 4.5:1), vizMark (series colour, 3:1), vizRampEnd (3:1) or vizRampInterior (ordering only).", name)
		}
	}
	for name := range vizRoles {
		if !declared[name] {
			t.Errorf("vizRoles classifies %s but tokens.css no longer declares it — delete the stale entry", name)
		}
	}

	var measured int
	for _, themeName := range []string{"light", "dark"} {
		tbl := themes[themeName]
		for _, surfaceTok := range []string{"--surface", "--bg"} {
			surfHex := resolveToken(tbl, surfaceTok)
			if !isHex(surfHex) {
				t.Fatalf("%s theme: %s resolved to %q, not a hex colour", themeName, surfaceTok, surfHex)
			}
			surf := mustColor(t, surfHex)

			for _, name := range sortedKeys(declared) {
				role, ok := vizRoles[name]
				if !ok {
					continue
				}
				hex := resolveToken(tbl, name)
				if !isHex(hex) {
					t.Errorf("%s theme: %s resolved to %q, not a hex colour — no ratio computable", themeName, name, hex)
					continue
				}
				floor := vizFloor(role, surfaceTok)
				if floor == 0 {
					continue // ramp interior: held by ordering, not by a floor
				}
				got := ContrastRatio(mustColor(t, hex), surf)
				measured++
				if got < floor {
					t.Errorf("Viz contrast FAIL — %s theme: %s (%s) on %s (%s) is %.2f:1, below the %.2f:1 floor (%s).\n"+
						"  Retune %s for the %s theme in shared/tokens.css until it clears %.2f against %s.",
						themeName, name, hex, surfaceTok, surfHex, got, floor, vizFloorCitation(role, surfaceTok),
						name, themeName, floor, surfHex)
				}
			}
		}
	}
	if measured == 0 {
		t.Fatal("no --viz-* / surface pairs measured — the test is a no-op")
	}
}

// vizFloor returns the contrast floor for a role against a given surface
// token, or 0 when the role is deliberately unfloored.
func vizFloor(role vizRole, surfaceTok string) float64 {
	switch role {
	case vizText:
		if surfaceTok == "--surface" {
			return AANormalText
		}
		// See the test's doc comment: chart labels live in cards, so --bg is
		// the weaker "if this ever moves" floor, not the conformance one.
		return AANonText
	case vizMark, vizRampEnd:
		return AANonText
	default:
		return 0
	}
}

func vizFloorCitation(role vizRole, surfaceTok string) string {
	if role == vizText && surfaceTok == "--surface" {
		return "WCAG 2.2 SC 1.4.3, AA, normal text — this token is chart ink"
	}
	if role == vizText {
		return "WCAG 2.2 SC 1.4.11 — chart ink measured against the page it is not normally painted on"
	}
	return "WCAG 2.2 SC 1.4.11 Non-text Contrast — a chart mark is a graphical object required to understand the content"
}

// TestVizSequentialRampIsOrdered holds the sequential ramp to the property
// its own token comment claims — "Steps are ordered by luminance (monotonic
// light->dark) so a heatmap reads correctly in greyscale" — because that
// sentence is the reason the interior steps are exempt from a contrast floor.
// Without it, "exempt" would just mean "unchecked".
//
// Adjacent steps also have to be separable from each other. 1.15:1 is not a
// WCAG number, it is the weakest separation at which two flat fills are still
// told apart side by side; the ramp's real minimum today is 1.37:1, so the
// floor has margin without being decorative.
//
// The DIVERGING ramp is deliberately not held to either property here, and
// that is a finding rather than an oversight: in the light theme its positive
// arm is not monotonic away from the neutral mid (mid L=0.500, pos-1 L=0.591,
// pos-2 L=0.201), and mid against neg-1 is 1.03:1 — the two collapse into one
// grey in greyscale and for anyone reading the chart by lightness. It has no
// consumers today, so it is latent. It gets promoted the moment something
// renders signed data with it.
func TestVizSequentialRampIsOrdered(t *testing.T) {
	src := readShared(t, "tokens.css")
	themes := tokenThemes(t, src)
	const minStepSeparation = 1.15

	for _, themeName := range []string{"light", "dark"} {
		tbl := themes[themeName]
		var lums []float64
		var hexes []string
		for i := 1; i <= 5; i++ {
			name := fmt.Sprintf("--viz-seq-%d", i)
			hex := resolveToken(tbl, name)
			if !isHex(hex) {
				t.Fatalf("%s theme: %s resolved to %q, not a hex colour", themeName, name, hex)
			}
			hexes = append(hexes, hex)
			lums = append(lums, RelativeLuminance(mustColor(t, hex)))
		}
		// Direction is theme-dependent by design: light darkens with
		// magnitude, dark brightens. Only monotonicity is the contract.
		ascending := lums[4] > lums[0]
		for i := 0; i < 4; i++ {
			a, b := lums[i], lums[i+1]
			if (ascending && b <= a) || (!ascending && b >= a) {
				dir := "descending"
				if ascending {
					dir = "ascending"
				}
				t.Errorf("Sequential ramp NOT ordered — %s theme: --viz-seq-%d (%s, L=%.4f) and --viz-seq-%d (%s, L=%.4f) break the %s luminance run.\n"+
					"  A heatmap built on this ramp stops reading correctly in greyscale and for anyone who cannot separate its hue. Re-solve the step's lightness.",
					themeName, i+1, hexes[i], a, i+2, hexes[i+1], b, dir)
			}
			sep := ContrastRatio(mustColor(t, hexes[i]), mustColor(t, hexes[i+1]))
			if sep < minStepSeparation {
				t.Errorf("Sequential ramp steps collapse — %s theme: --viz-seq-%d (%s) against --viz-seq-%d (%s) is %.2f:1, below the %.2f:1 step-separation floor.\n"+
					"  Two adjacent magnitudes render as the same fill. Widen the lightness gap between them.",
					themeName, i+1, hexes[i], i+2, hexes[i+1], sep, minStepSeparation)
			}
		}
	}
}

// TestSubstrateRampContrast holds the --sub-* ramp to the three properties its
// token comment states and nothing has ever checked.
//
// The comment is unusually specific — it lists the derived ratios for both
// themes to two decimals and says "Re-derive the same way if any step moves;
// do not eyeball a light value". That instruction exists because the first
// light pass WAS eyeballed: an HSL lightness flip of the dark ramp put light
// step 4 at 2.33:1, so a tick mark that read in dark would have shipped
// invisible in light. sRGB luminance is not symmetric about mid-grey.
//
// The three properties:
//
//  1. Steps 4 and 5 clear the non-text floor in BOTH themes. These carry
//     meaning — tick marks, structural dots, emphasis structure.
//  2. The whole ramp is strictly increasing in contrast against its own
//     surface. A ramp whose steps cross is not a ramp.
//  3. Light and dark reproduce each other's ratios within 0.05. This is the
//     derivation rule the comment demands, turned into an assertion: it is
//     what makes a light value "solved" rather than "picked".
//
// Steps 1-3 have NO contrast floor and that is deliberate — a hairline rule at
// 3:1 is not a hairline, it is a border.
func TestSubstrateRampContrast(t *testing.T) {
	src := readShared(t, "tokens.css")
	themes := tokenThemes(t, src)
	const parityTolerance = 0.05

	ratios := map[string][5]float64{}
	hexes := map[string][5]string{}

	for _, themeName := range []string{"light", "dark"} {
		tbl := themes[themeName]
		surfHex := resolveToken(tbl, "--surface")
		if !isHex(surfHex) {
			t.Fatalf("%s theme: --surface resolved to %q", themeName, surfHex)
		}
		surf := mustColor(t, surfHex)

		var r [5]float64
		var h [5]string
		for i := 1; i <= 5; i++ {
			name := fmt.Sprintf("--sub-%d", i)
			hex := resolveToken(tbl, name)
			if !isHex(hex) {
				t.Fatalf("%s theme: %s resolved to %q, not a hex colour — the substrate ramp must be literal in both themes", themeName, name, hex)
			}
			h[i-1] = hex
			r[i-1] = ContrastRatio(mustColor(t, hex), surf)
		}
		ratios[themeName], hexes[themeName] = r, h

		// (1) the two load-bearing steps
		for _, i := range []int{4, 5} {
			if r[i-1] < AANonText {
				t.Errorf("Substrate contrast FAIL — %s theme: --sub-%d (%s) on --surface (%s) is %.2f:1, below the %.2f:1 floor (WCAG 2.2 SC 1.4.11 — step %d carries meaning: %s).\n"+
					"  Re-solve --sub-%d's lightness against --surface for this theme; do not copy the other theme's hex.",
					themeName, i, h[i-1], surfHex, r[i-1], AANonText, i,
					map[int]string{4: "tick marks and structural dots", 5: "emphasis structure"}[i], i)
			}
		}
		// (2) strictly increasing
		for i := 0; i < 4; i++ {
			if r[i+1] <= r[i] {
				t.Errorf("Substrate ramp NOT ordered — %s theme: --sub-%d (%s) is %.2f:1 and --sub-%d (%s) is %.2f:1 against --surface; step %d must be strictly stronger than step %d.\n"+
					"  The ramp's steps have crossed, so 'weakest to strongest' no longer describes it.",
					themeName, i+1, h[i], r[i], i+2, h[i+1], r[i+1], i+2, i+1)
			}
		}
	}

	// (3) cross-theme parity
	for i := 0; i < 5; i++ {
		l, d := ratios["light"][i], ratios["dark"][i]
		if math.Abs(l-d) > parityTolerance {
			t.Errorf("Substrate ramp parity FAIL — --sub-%d is %.2f:1 in light (%s on white) but %.2f:1 in dark (%s), a gap of %.2f against the %.2f tolerance.\n"+
				"  Light is derived from dark by solving each step's LIGHTNESS so its contrast reproduces the dark step's, keeping hue and saturation. Re-solve rather than adjusting by eye — an HSL flip is what put light step 4 at 2.33:1 the first time.",
				i+1, l, hexes["light"][i], d, hexes["dark"][i], math.Abs(l-d), parityTolerance)
		}
	}
}

// ─── CSS reading helpers ─────────────────────────────────────────────────

var (
	cssRulePattern    = regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`)
	badgeClassPattern = regexp.MustCompile(`\.(badge(?:-[a-zA-Z0-9_]+)?)\s*$`)
	varRefPattern     = regexp.MustCompile(`^var\(\s*(--[a-zA-Z0-9-]+)\s*\)$`)
	hexPattern        = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)
)

// badgePalette resolves status-classes.css into the declarations that actually
// win for each badge class, per theme.
//
// It models the cascade rather than grepping for the last hex near a class
// name, because the file relies on it: `.badge` sets a background and colour
// for everything, `.badge-<status>` overrides at equal specificity by source
// order, and `[data-theme="dark"] .badge-<status>` overrides both at higher
// specificity. Reading only the dark block would miss any property a dark rule
// does not restate; reading only the literal nearest to the class name would
// silently pick the base .badge values for any status whose rule is missing —
// and the base is readable, so the missed status would pass.
func badgePalette(t *testing.T, css string) (light, dark map[string]map[string]string) {
	t.Helper()
	css = cssCommentPattern.ReplaceAllString(css, " ")

	lightRules := map[string]map[string]string{}
	darkRules := map[string]map[string]string{}

	for _, m := range cssRulePattern.FindAllStringSubmatch(css, -1) {
		decls := map[string]string{}
		for _, d := range strings.Split(m[2], ";") {
			k, v, ok := strings.Cut(d, ":")
			if !ok {
				continue
			}
			decls[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		for _, sel := range strings.Split(m[1], ",") {
			sel = strings.TrimSpace(sel)
			cm := badgeClassPattern.FindStringSubmatch(sel)
			if cm == nil {
				continue
			}
			class := cm[1]
			target := lightRules
			if strings.Contains(sel, `data-theme="dark"`) {
				target = darkRules
			}
			if target[class] == nil {
				target[class] = map[string]string{}
			}
			for k, v := range decls {
				target[class][k] = v
			}
		}
	}
	if len(lightRules) == 0 {
		t.Fatal("badgePalette parsed zero rules out of status-classes.css — the parser has drifted from the file")
	}

	merge := func(dst, src map[string]string) {
		for k, v := range src {
			dst[k] = v
		}
	}
	light = map[string]map[string]string{}
	dark = map[string]map[string]string{}
	classes := map[string]bool{}
	for c := range lightRules {
		classes[c] = true
	}
	for c := range darkRules {
		classes[c] = true
	}
	for c := range classes {
		if c == "badge" {
			continue
		}
		l := map[string]string{}
		merge(l, lightRules["badge"]) // the base pill, lowest in source order
		merge(l, lightRules[c])
		light[c] = l

		d := map[string]string{}
		merge(d, l) // dark inherits every light declaration it does not restate
		merge(d, darkRules["badge"])
		merge(d, darkRules[c])
		dark[c] = d
	}
	return light, dark
}

// tokenThemes returns the resolved token tables for both themes. Dark starts
// from light, because [data-theme="dark"] overrides a subset — a token the
// dark block does not restate keeps its :root value, and measuring the dark
// theme without that inheritance would report tokens as missing that are not.
func tokenThemes(t *testing.T, src string) map[string]map[string]string {
	t.Helper()
	lightBlock := extractTheme(t, src, ":root")
	darkBlock := extractTheme(t, src, `[data-theme="dark"]`)

	parse := func(block string) map[string]string {
		out := map[string]string{}
		re := regexp.MustCompile(`(--[a-zA-Z0-9-]+)\s*:\s*([^;]+);`)
		for _, m := range re.FindAllStringSubmatch(block, -1) {
			out[m[1]] = strings.TrimSpace(m[2])
		}
		return out
	}
	light := parse(lightBlock)
	dark := map[string]string{}
	for k, v := range light {
		dark[k] = v
	}
	for k, v := range parse(darkBlock) {
		dark[k] = v
	}
	if len(light) == 0 {
		t.Fatal("tokenThemes parsed zero tokens out of tokens.css :root")
	}
	return map[string]map[string]string{"light": light, "dark": dark}
}

// resolveToken follows a var() alias chain inside one theme to the literal it
// ends at. Returns the unresolved text when the chain does not end in a hex,
// so the caller reports what it found rather than substituting a default —
// a defaulted colour is how a contrast test measures black and passes.
func resolveToken(tbl map[string]string, name string) string {
	v, ok := tbl[name]
	if !ok {
		return ""
	}
	for range 10 {
		m := varRefPattern.FindStringSubmatch(strings.TrimSpace(v))
		if m == nil {
			break
		}
		next, ok := tbl[m[1]]
		if !ok {
			return v
		}
		v = next
	}
	return strings.TrimSpace(v)
}

func isHex(s string) bool { return hexPattern.MatchString(strings.TrimSpace(s)) }

func mustColor(t *testing.T, hex string) RGB {
	t.Helper()
	c, err := ParseHexColor(hex)
	if err != nil {
		t.Fatalf("parse %q: %v", hex, err)
	}
	return c
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
