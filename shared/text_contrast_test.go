package shared

import (
	"fmt"
	"regexp"
	"sort"
	"testing"
)

// The body-text ramp, measured — which nothing did before this file.
//
// TestSignalBadgeTextClearsAA measures badge ink. TestChipContrast measures chip
// ink. TestVizTokenContrastAgainstSurfaces measures chart ink. The tokens that
// paint ordinary page text — `--text`, `--text-muted`, `--text-strong`,
// `--text-tertiary` — were measured by NONE of them. `--text-muted` had been
// measured once, and only by accident: `--viz-secondary` aliases it, so it rode
// in through the chart-ink test, and that is how its 4.33:1 on `--bg` was
// found. Nothing was watching the other three.
//
// `--text-tertiary` is what was underneath. tokens.css described it as "the
// faintest labels" and it measured 3.15:1 on a card and 2.91:1 on the page in
// light, 3.77:1 on a card and 3.27:1 on a raised panel in dark — a text token
// below the text floor in both themes, live on the KPI strips, the overview
// support panels and the Replenishment Health threshold editor.
//
// AND IT COULD NOT BE FIXED IN PLACE. Solving for the lightest grey of its own
// hue and saturation that clears 4.5:1 on every light surface gives #656d78 —
// DARKER than `--text-muted`'s #68717a. `--text-muted` had already been nudged
// to sit just above the floor (4.58:1 on the worse light surface), so there is
// no room left underneath it: any third step quiet enough to read as quieter
// than muted is too quiet to read at all. The ramp inverts. This is the same
// shape as the guide's "13 statuses at one calm weight is at capacity" — the
// palette ran out of room, and the honest move is to say so rather than ship a
// step that is below the floor OR a step that is not actually quieter.
//
// So `--text-tertiary` is gone. Its fifteen type declarations — four in
// components.css, eleven in Core's style.css — take `--text-muted`;
// they were already distinguished from body text by size, case and
// letter-spacing, and per U5's own ruling on `.de-muted` vs `.de-nodata`, a
// one-step luminance difference is not an encoding — it is colour-alone
// signalling for a distinction the type already carries. Its one non-type site,
// the map's offline-robot dot, moved to `--sub-4`, which is the token whose
// documented role is exactly that ("tick marks, structural dots — marks that
// carry meaning, must clear 3:1"). Using a TEXT token as a MARK is the mirror
// of the category error `TestChipInkIsNotItsFill` guards.
//
// WHAT THIS TEST IS FOR, therefore, is not the three survivors — they pass
// comfortably. It is the EXHAUSTIVENESS check below. "There is no correct token
// for quiet type" is a claim that decays the moment someone adds
// `--text-faint`, and the failure mode is silent: a new token gets a plausible
// grey, no test names it, and the ramp grows a fourth step below the floor
// again. Every `--text*` token in tokens.css must appear in textTokens, so
// adding one forces a measurement.

// textSurfaces are the elevation steps that actually host text, each with the
// evidence that it does. `--elev-canvas` is deliberately absent: it has ZERO
// use sites in any stylesheet or template, so measuring against it would invent
// a failure (light canvas is the worst surface in the file at 2.71:1 for the
// old tertiary) that no reader can ever see. If canvas gains a use site, add it
// here and re-derive whatever stops clearing.
var textSurfaces = []struct{ token, why string }{
	{"--elev-surface", "cards and panels — the common case"},
	{"--bg", "the page behind the cards; aliases --elev-base"},
	{"--elev-raised", "raised panels and hover states: components.css .kpi-tile--clickable:hover and the .ov-support panel, style.css .rh-editor > td and .rh-row:hover"},
}

// textTokens is the whole body-text family. `role` records what the token is
// for, because that is what decides its floor — and every member of this family
// is type, so every member takes the normal-text floor. A token in tokens.css
// named `--text*` that is NOT type is a naming bug, not an exemption.
var textTokens = []string{
	"--text",
	"--text-muted",
	"--text-strong",
}

var textTokenPattern = regexp.MustCompile(`(?m)^\s*(--text[a-zA-Z0-9-]*)\s*:`)

func TestBodyTextTokensClearAA(t *testing.T) {
	src := readShared(t, "tokens.css")
	themes := tokenThemes(t, src)

	// Exhaustiveness, both ways. This is the load-bearing half of the test.
	declared := map[string]bool{}
	for _, m := range textTokenPattern.FindAllStringSubmatch(src, -1) {
		declared[m[1]] = true
	}
	if len(declared) == 0 {
		t.Fatal("no --text* tokens found in tokens.css — the recogniser has drifted and this test is a no-op")
	}
	inTable := map[string]bool{}
	for _, tok := range textTokens {
		inTable[tok] = true
	}
	for _, tok := range sortedTextKeys(declared) {
		if !inTable[tok] {
			t.Errorf("%s is declared in tokens.css but is not in textTokens, so nothing measures it.\n"+
				"  Add it here and let it be measured. If it cannot clear %.1f:1 on every surface in textSurfaces, it is\n"+
				"  not a type token — and note that --text-tertiary was DELETED for exactly this reason: the ramp has no\n"+
				"  room for a step below --text-muted (4.58:1 on the worse light surface) that is still readable.",
				tok, AANormalText)
		}
	}
	for _, tok := range textTokens {
		if !declared[tok] {
			t.Errorf("textTokens carries %s but tokens.css no longer declares it — delete the stale entry", tok)
		}
	}

	var measured int
	worst := 999.0
	var worstAt string

	for _, themeName := range []string{"light", "dark"} {
		tbl := themes[themeName]
		for _, surf := range textSurfaces {
			surfHex := resolveToken(tbl, surf.token)
			if !isHex(surfHex) {
				t.Fatalf("%s theme: %s resolved to %q, not a hex colour", themeName, surf.token, surfHex)
			}
			bg := mustColor(t, surfHex)

			for _, tok := range textTokens {
				fgHex := resolveToken(tbl, tok)
				if !isHex(fgHex) {
					t.Errorf("%s theme: %s resolved to %q, not a hex colour — no ratio can be computed, which is how a\n"+
						"  token missing from one theme block passes unmeasured", themeName, tok, fgHex)
					continue
				}
				got := ContrastRatio(mustColor(t, fgHex), bg)
				measured++
				where := fmt.Sprintf("%s/%s on %s", themeName, tok, surf.token)
				if got < worst {
					worst, worstAt = got, where
				}
				if got < AANormalText {
					t.Errorf("body text below AA — %s: %s on %s (%s) is %.4f:1, floor %.1f:1 (WCAG 2.2 SC 1.4.3, AA, normal text).\n"+
						"  That surface hosts text: %s.\n"+
						"  Fix the TOKEN, not the call site — and solve the new value against the WORST surface in\n"+
						"  textSurfaces, because a token used in three places has to pass in the weakest one.",
						where, fgHex, surf.token, surfHex, got, AANormalText, surf.why)
				}
			}
		}
	}

	if measured == 0 {
		t.Fatal("no text/surface pairs measured — the token table or the surface list has drifted")
	}
	t.Logf("body text: %d pairs measured, worst %.4f:1 at %s (floor %.1f:1)", measured, worst, worstAt, AANormalText)
}

func sortedTextKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
