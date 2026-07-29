package shared

import (
	"fmt"
	"regexp"
	"testing"
)

// The two chip floors — 4.5:1 text on pill, 3:1 pill against surface.
//
// `TestSignalBadgeTextClearsAA` holds every Signal badge to the first of
// those. The `.chip-*` family in components.css is a SECOND, separate pill
// vocabulary — derived-health verdicts rather than order lifecycle — and
// nothing measured it at all. That is the gap this file closes, and it is the
// gap both invisible-chip incidents came out of: `.chip-err` shipped at
// 1.2:1, and `.chip-warn` shipped with no rule whatsoever for two releases,
// rendering as bare `.chip` — pill-shaped and the colour of the page behind
// it. `chip_drift_test.go` closes the missing-rule half. A presence check
// cannot see 1.2:1.
//
// Chips went unmeasured because they are harder to measure than badges, which
// is the usual reason. A badge is a literal hex pair. A chip is
// `color-mix(in srgb, var(--viz-green) 16%, transparent)` over whatever
// surface it lands on, with its TEXT set to the same token as its fill — so
// the ratio is a function of the alpha and of the surface, and neither number
// appears in the rule. `CompositeOver` exists for precisely this.
//
// WHAT THE FIRST RUN FOUND, and why neither floor is a hard gate yet:
//
// Fifteen of the twenty-eight (theme x chip x surface) combinations are below
// AA on text, the worst being `.chip-ok` at 2.89:1 on the page. `.chip-err` —
// the one that was FIXED after the 1.2:1 incident — measures 4.17:1, still
// under the floor. Nothing on the boundary side reaches 3:1 at all; the whole
// family sits between 1.20 and 1.61.
//
// THE TWO FLOORS ARE IN TENSION FOR A CHIP WHOSE TEXT AND FILL ARE THE SAME
// TOKEN, and that is the actual finding rather than a list of bad numbers.
// The fill is a wash of the label's own colour, so lowering the mix
// percentage moves the fill toward the surface — text contrast up, boundary
// contrast down — and raising it does the reverse. There is no percentage
// that satisfies both. A badge escapes this because its foreground and
// background are independently chosen; a chip as currently specified cannot.
// Fixing it means giving each chip an ink colour distinct from its fill, which
// is a palette decision with seven new values in it, not a tuning pass — so it
// is recorded here rather than guessed at.
//
// Until then both floors are RATCHETS at today's measured worst, with the real
// targets named beside them. The ratchets say "nothing may get worse than the
// worst thing already shipping", which is a guard that can hold today. Setting
// them to the targets would paint the suite red on the first run, and a suite
// that is red on arrival gets the test deleted rather than the palette fixed.
// Both ratchets fail when they go STALE, so fixing chips forces them upward
// and the gap closes visibly instead of being forgotten.

type chipSpec struct {
	class   string
	fillTok string  // token composited to make the background
	alpha   float64 // color-mix percentage / 100; 1.0 = opaque
	textTok string  // token used for the label
	textHex string  // set instead of textTok when the rule uses a literal
}

// Declared here rather than parsed out of the CSS, because a color-mix parser
// would be a second implementation of the cascade to get wrong. The
// exhaustiveness check is what stops the table drifting: every `.chip-<x>`
// rule in components.css must appear here, and every entry here must still
// exist in the CSS.
var chipSpecs = []chipSpec{
	{class: "chip-ok", fillTok: "--viz-green", alpha: 0.16, textTok: "--viz-green"},
	{class: "chip-near", fillTok: "--viz-amber", alpha: 0.18, textTok: "--viz-amber"},
	{class: "chip-below", fillTok: "--viz-coral", alpha: 0.18, textTok: "--viz-coral"},
	{class: "chip-muted", fillTok: "--text-muted", alpha: 0.15, textTok: "--text-muted"},
	// The only opaque chip, and the one that was 1.2:1 before d01156b1.
	{class: "chip-err", fillTok: "--viz-coral", alpha: 1.0, textHex: "#1b0508"},
	{class: "chip-drift", fillTok: "--viz-violet", alpha: 0.20, textTok: "--viz-violet"},
	{class: "chip-warn", fillTok: "--viz-amber", alpha: 0.20, textTok: "--viz-amber"},
}

var chipRuleRe = regexp.MustCompile(`(?m)^\.(chip-[a-z]+)\s*\{`)

const (
	// Floor one's target: WCAG 2.2 SC 1.4.3, AA, normal text. Chip labels are
	// 0.74rem/600 — under the large-text cut-off, so 4.5 applies, not 3.0.
	chipTextTarget = AANormalText
	// Floor two's target: WCAG 2.2 SC 1.4.11, the same non-text floor the
	// --viz-* mark tokens are held to.
	chipBoundaryTarget = AANonText

	// Today's measured worst, rounded DOWN to two decimals so the pinned
	// value cannot sit a rounding error above the thing it is pinning. Raise
	// these as chips improve; the staleness checks below fail if you forget.
	chipTextRatchet     = 2.88 // light .chip-ok on --bg, measures 2.889
	chipBoundaryRatchet = 1.19 // light .chip-muted on --bg, measures 1.199
)

func TestChipContrast(t *testing.T) {
	themes := tokenThemes(t, readShared(t, "tokens.css"))
	componentsSrc := readShared(t, "components.css")

	// Exhaustiveness, both directions. An unmeasured chip is the entire
	// failure mode this file exists for.
	declared := map[string]bool{}
	for _, m := range chipRuleRe.FindAllStringSubmatch(componentsSrc, -1) {
		declared[m[1]] = true
	}
	if len(declared) == 0 {
		t.Fatal("no .chip-* rules found in components.css — the recogniser has drifted and this test is a no-op")
	}
	inTable := map[string]bool{}
	for _, c := range chipSpecs {
		inTable[c.class] = true
	}
	for class := range declared {
		if !inTable[class] {
			t.Errorf(".%s has a rule in components.css but no entry in chipSpecs, so nothing measures its contrast.\n"+
				"  Add it: fill token, color-mix percentage, text token.", class)
		}
	}
	for class := range inTable {
		if !declared[class] {
			t.Errorf("chipSpecs carries .%s but components.css no longer declares it — delete the stale entry", class)
		}
	}

	var measured, belowText, belowBoundary int
	worstText, worstBoundary := 999.0, 999.0
	var worstTextAt, worstBoundaryAt string

	for _, themeName := range []string{"light", "dark"} {
		tbl := themes[themeName]
		// Chips ride on cards and on the page, same as the viz tokens, and the
		// worse of the two is the one that has to pass.
		for _, surfTok := range []string{"--surface", "--bg"} {
			surfHex := resolveToken(tbl, surfTok)
			if !isHex(surfHex) {
				t.Fatalf("%s theme: %s resolved to %q, not a hex colour", themeName, surfTok, surfHex)
			}
			surf := mustColor(t, surfHex)

			for _, c := range chipSpecs {
				fillHex := resolveToken(tbl, c.fillTok)
				textHex := c.textHex
				if textHex == "" {
					textHex = resolveToken(tbl, c.textTok)
				}
				if !isHex(fillHex) || !isHex(textHex) {
					t.Errorf("%s theme: .%s resolved to fill=%q text=%q — not both hex, so no ratio can be computed",
						themeName, c.class, fillHex, textHex)
					continue
				}

				fill := CompositeOver(mustColor(t, fillHex), c.alpha, surf)
				textRatio := ContrastRatio(mustColor(t, textHex), fill)
				boundRatio := ContrastRatio(fill, surf)
				measured++
				where := fmt.Sprintf("%s/%s on %s", themeName, c.class, surfTok)

				if textRatio < worstText {
					worstText, worstTextAt = textRatio, where
				}
				if boundRatio < worstBoundary {
					worstBoundary, worstBoundaryAt = boundRatio, where
				}
				if textRatio < chipTextTarget {
					belowText++
				}
				if boundRatio < chipBoundaryTarget {
					belowBoundary++
				}

				if textRatio < chipTextRatchet {
					t.Errorf("chip text REGRESSION — %s: label %s on the composited fill (%s at %.0f%% over %s) is %.2f:1, "+
						"below the %.2f:1 ratchet (target %.2f:1).\n"+
						"  Text and fill are the same token here, so the mix percentage trades this floor against the boundary one. "+
						"Give the chip its own ink colour instead.",
						where, textHex, c.fillTok, c.alpha*100, surfHex, textRatio, chipTextRatchet, chipTextTarget)
				}
				if boundRatio < chipBoundaryRatchet {
					t.Errorf("chip boundary REGRESSION — %s: the fill is %.2f:1 against the surface, "+
						"below the %.2f:1 ratchet (target %.2f:1).\n"+
						"  This chip has become less visible than the least visible chip previously shipping.",
						where, boundRatio, chipBoundaryRatchet, chipBoundaryTarget)
				}
			}
		}
	}

	if measured == 0 {
		t.Fatal("no chips measured — the token table or the chip table has drifted")
	}
	t.Logf("chips measured: %d | text: worst %.2f:1 at %s, %d/%d below the %.2f:1 target | boundary: worst %.2f:1 at %s, %d/%d below the %.2f:1 target",
		measured, worstText, worstTextAt, belowText, measured, chipTextTarget,
		worstBoundary, worstBoundaryAt, belowBoundary, measured, chipBoundaryTarget)

	// A ratchet pinned below reality is not guarding anything. Same rule as
	// knownCollapses: the moment the number moves, somebody has to look.
	if worstText > chipTextRatchet+0.05 {
		t.Errorf("chipTextRatchet is STALE — the worst chip text now measures %.2f:1 (%s), above the %.2f:1 ratchet.\n"+
			"  Raise chipTextRatchet to %.2f. The target is %.2f:1.",
			worstText, worstTextAt, chipTextRatchet, worstText, chipTextTarget)
	}
	if worstBoundary > chipBoundaryRatchet+0.05 {
		t.Errorf("chipBoundaryRatchet is STALE — the worst chip boundary now measures %.2f:1 (%s), above the %.2f:1 ratchet.\n"+
			"  Raise chipBoundaryRatchet to %.2f. The target is %.2f:1.",
			worstBoundary, worstBoundaryAt, chipBoundaryRatchet, worstBoundary, chipBoundaryTarget)
	}
}
