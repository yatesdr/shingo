package shared

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// The chip contrast floors.
//
// `TestSignalBadgeTextClearsAA` holds every Signal badge to a text floor. The
// `.chip-*` family in components.css is a SECOND, separate pill vocabulary —
// derived-health verdicts rather than order lifecycle — and nothing measured
// it at all until this file. That is the gap both invisible-chip incidents
// came out of: `.chip-err` shipped at 1.2:1, and `.chip-warn` shipped with no
// rule whatsoever for two releases, rendering as bare `.chip` — pill-shaped
// and the colour of the page behind it. `chip_drift_test.go` closes the
// missing-rule half. A presence check cannot see 1.2:1.
//
// Chips are harder to measure than badges, which is why they went unmeasured.
// A badge is a literal hex pair. A chip is
// `color-mix(in srgb, var(--viz-green) 16%, transparent)` over whatever
// surface it lands on, so the background its text is read against is not any
// declared token — it is the composite. `CompositeOver` exists for that.
//
// ─── WHAT 4.4 CHANGED, AND WHY THE SECOND FLOOR IS GONE ──────────────────
//
// The first run of this file found fifteen of twenty-eight combinations below
// AA on text and reported the cause as structural: a chip's fill was a wash of
// its own LABEL colour, so lowering the mix percentage moved the fill toward
// the surface (text contrast up, boundary contrast down) and raising it did
// the reverse. No percentage satisfied both floors. That diagnosis was right.
// The prescription recorded beside it — "an ink colour per chip, seven new
// values" — was measured in 4.4 and was wrong in three ways:
//
//  1. INK CANNOT MOVE THE BOUNDARY FLOOR AT ALL. The boundary ratio is
//     fill-vs-surface; no text term appears in it. Giving every chip its own
//     ink closes 15 text failures and leaves all 24 boundary failures exactly
//     where they were. The two floors were never one fix.
//
//  2. IT WAS NEVER SEVEN VALUES. `.chip-drift` had no use site anywhere and
//     was deleted rather than derived. `.chip-near` and `.chip-warn` are both
//     amber two percentage points apart and share one ink. Every DARK
//     combination already cleared the floor. What remained was four light
//     hues plus one dark pin — and `--chip-ink-err` is white, which is not a
//     new colour. The count came from counting rules, not from measuring.
//
//  3. THE SECOND FLOOR DOES NOT APPLY TO THESE CHIPS. WCAG 2.2 SC 1.4.11 is
//     about user-interface components and about graphics "required to
//     understand the content". A health chip is neither: it is not
//     interactive, and its content is the word printed inside it — "OK",
//     "Near threshold", "Below — order due". The pill is redundant encoding
//     around a text label that carries the meaning on its own. The floor was
//     borrowed from the --viz-* MARK tokens, where it does apply because a
//     chart mark IS the information and has no text alternative. Holding a
//     labelled pill to it produced 24 unfixable failures and a ratchet pinned
//     at 1.19 that guarded nothing.
//
//     Chasing it would also have cost the vocabulary its reason to exist. The
//     only way to lift a 15%-wash fill to 3:1 against the surface is to stop
//     it being a wash: TestChipBoundaryNeedsNearOpacity measures what that
//     would actually take — 69% to 89% opacity — which is the Signal badge,
//     the loud vocabulary these chips were deliberately built quiet against.
//
//     So the boundary floor is now asserted for OPAQUE chips only, where the
//     pill genuinely is the object being seen. `.chip-err` is the one such
//     chip and it passes at 4.34:1, so this is a real gate that is green, not
//     a ratchet parked under reality.
//
// PRECONDITION, and the thing to re-open this ruling over: every chip prints a
// label. An icon-only chip would put the meaning back into the shape, and the
// boundary floor would apply to it again.
//
// The text floor is now a HARD GATE at 4.5:1 rather than a ratchet, because
// every chip clears it. Ratchets were the right instrument while the family
// was structurally unable to pass; they are the wrong one once it can.

type chipSpec struct {
	class   string
	fillTok string  // token composited to make the background
	alpha   float64 // color-mix percentage / 100; 1.0 = opaque
	inkTok  string  // token used for the label — MUST NOT be fillTok
}

func (c chipSpec) opaque() bool { return c.alpha >= 1.0 }

// Declared here rather than parsed out of the CSS, because a color-mix parser
// would be a second implementation of the cascade to get wrong. The
// exhaustiveness check is what stops the table drifting: every `.chip-<x>`
// rule in components.css must appear here, and every entry here must still
// exist in the CSS.
var chipSpecs = []chipSpec{
	{class: "chip-ok", fillTok: "--viz-green", alpha: 0.16, inkTok: "--chip-ink-ok"},
	{class: "chip-near", fillTok: "--viz-amber", alpha: 0.18, inkTok: "--chip-ink-near"},
	{class: "chip-below", fillTok: "--viz-coral", alpha: 0.18, inkTok: "--chip-ink-below"},
	{class: "chip-muted", fillTok: "--text-muted", alpha: 0.15, inkTok: "--chip-ink-muted"},
	// The only opaque chip, and the one that was 1.2:1 before d01156b1.
	{class: "chip-err", fillTok: "--viz-coral", alpha: 1.0, inkTok: "--chip-ink-err"},
	// Shares .chip-near's ink on purpose; see components.css.
	{class: "chip-warn", fillTok: "--viz-amber", alpha: 0.20, inkTok: "--chip-ink-near"},
}

var chipRuleRe = regexp.MustCompile(`(?m)^\.(chip-[a-z]+)\s*\{`)

const (
	// SC 1.4.3, AA, normal text. Chip labels are 0.74rem/600 — under the
	// large-text cut-off, so 4.5 applies, not 3.0.
	chipTextFloor = AANormalText
	// SC 1.4.11, asserted for OPAQUE chips only. See the header.
	chipBoundaryFloor = AANonText
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
				"  Add it: fill token, color-mix percentage, ink token.", class)
		}
	}
	for class := range inTable {
		if !declared[class] {
			t.Errorf("chipSpecs carries .%s but components.css no longer declares it — delete the stale entry", class)
		}
	}

	var measured int
	worstText := 999.0
	var worstTextAt string

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
				inkHex := resolveToken(tbl, c.inkTok)
				if !isHex(fillHex) || !isHex(inkHex) {
					t.Errorf("%s theme: .%s resolved to fill=%q ink=%q — not both hex, so no ratio can be computed.\n"+
						"  A --chip-ink-* token missing from one theme block is the likely cause.",
						themeName, c.class, fillHex, inkHex)
					continue
				}

				fill := CompositeOver(mustColor(t, fillHex), c.alpha, surf)
				textRatio := ContrastRatio(mustColor(t, inkHex), fill)
				measured++
				where := fmt.Sprintf("%s/%s on %s", themeName, c.class, surfTok)

				if textRatio < worstText {
					worstText, worstTextAt = textRatio, where
				}

				if textRatio < chipTextFloor {
					t.Errorf("chip text below AA — %s: ink %s (%s) on the composited fill (%s at %.0f%% over %s) is %.2f:1, "+
						"floor %.2f:1.\n"+
						"  Re-derive the ink: scale the fill hue toward black (light) or white (dark) until it clears "+
						"on the WORSE of --surface and --bg.",
						where, inkHex, c.inkTok, c.fillTok, c.alpha*100, surfHex, textRatio, chipTextFloor)
				}

				// Boundary floor: opaque chips only. See the header for why a
				// labelled translucent pill is outside SC 1.4.11.
				if c.opaque() {
					boundRatio := ContrastRatio(fill, surf)
					if boundRatio < chipBoundaryFloor {
						t.Errorf("opaque chip boundary below AA — %s: the fill is %.2f:1 against the surface, floor %.2f:1.\n"+
							"  This chip has no wash to hide behind: its pill IS the object being seen.",
							where, boundRatio, chipBoundaryFloor)
					}
				}
			}
		}
	}

	if measured == 0 {
		t.Fatal("no chips measured — the token table or the chip table has drifted")
	}
	t.Logf("chips measured: %d | text: worst %.2f:1 at %s (floor %.2f)", measured, worstText, worstTextAt, chipTextFloor)
}

// TestChipInkIsNotItsFill is the structural rule the 4.4 derivation rests on,
// asserted directly rather than left to be re-discovered from a table of bad
// numbers.
//
// A chip whose ink token IS its fill token cannot be tuned into compliance:
// the fill is a wash of the ink, so the two move together and the gap between
// them is capped by the mix percentage. That was true of five of the six chips
// before 4.4 and it is what put every light-theme combination below the floor.
// TestChipContrast would catch a re-collapse only if the resulting ratio
// happened to land under 4.5 — on a dark surface with a bright mark token it
// might not — so the invariant is worth asserting on its own.
func TestChipInkIsNotItsFill(t *testing.T) {
	for _, c := range chipSpecs {
		if c.inkTok == c.fillTok {
			t.Errorf(".%s uses %s for both fill and ink.\n"+
				"  A chip's fill is a wash of its ink, so the two cannot both be tuned — "+
				"give the chip a --chip-ink-* token.", c.class, c.fillTok)
		}
		if !strings.HasPrefix(c.inkTok, "--chip-ink-") {
			t.Errorf(".%s draws its ink from %s, which is not a --chip-ink-* token.\n"+
				"  --viz-* are MARK colours held to 3:1 and --sub-* are STRUCTURE steps; "+
				"neither is held to a text floor. Ink is type and needs a type token.", c.class, c.inkTok)
		}
	}
}

// TestChipBoundaryNeedsNearOpacity records the measurement the boundary ruling
// rests on, so the ruling can be checked rather than taken on faith.
//
// The header argues SC 1.4.11 does not reach a labelled translucent pill. The
// supporting fact is that satisfying it would cost the vocabulary its calm:
// this test fails if any translucent chip could reach 3:1 against the surface
// at an opacity that still reads as a wash (<= 50%). Today every one of them
// needs 69% or more. If a token change ever makes a chip's fill reach the
// boundary floor cheaply, this fails and the ruling should be revisited for
// that chip rather than silently inherited.
func TestChipBoundaryNeedsNearOpacity(t *testing.T) {
	const washCeiling = 0.50
	themes := tokenThemes(t, readShared(t, "tokens.css"))

	for _, c := range chipSpecs {
		if c.opaque() {
			continue
		}
		// Smallest alpha at which this fill clears the boundary floor on the
		// WORSE of the two surfaces, in the worse theme.
		needed := -1.0
		for i := 1; i <= 100; i++ {
			a := float64(i) / 100.0
			worst := 999.0
			for _, themeName := range []string{"light", "dark"} {
				tbl := themes[themeName]
				for _, surfTok := range []string{"--surface", "--bg"} {
					surf := mustColor(t, resolveToken(tbl, surfTok))
					fill := CompositeOver(mustColor(t, resolveToken(tbl, c.fillTok)), a, surf)
					if r := ContrastRatio(fill, surf); r < worst {
						worst = r
					}
				}
			}
			if worst >= chipBoundaryFloor {
				needed = a
				break
			}
		}
		if needed < 0 {
			t.Logf(".%s cannot reach %.1f:1 against the surface at any opacity", c.class, chipBoundaryFloor)
			continue
		}
		t.Logf(".%s would need %.0f%% opacity to clear the %.1f:1 boundary floor (ships at %.0f%%)",
			c.class, needed*100, chipBoundaryFloor, c.alpha*100)
		if needed <= washCeiling {
			t.Errorf(".%s reaches the %.1f:1 boundary floor at only %.0f%% opacity, still within wash range.\n"+
				"  The ruling that SC 1.4.11 does not reach this chip was justified by the cost of satisfying it "+
				"(69-89%% opacity, i.e. becoming a Signal badge). That justification no longer holds here — "+
				"revisit the ruling for this chip.", c.class, chipBoundaryFloor, needed*100)
		}
	}
}
