package shared

import (
	"fmt"
	"math"
	"testing"
)

// The Signal palette under colour-vision deficiency.
//
// status-classes.css promises that "an operator scanning a table of 20 orders
// can distinguish dispatched / in_transit / staged / reshuffling at a glance
// without reading the label". That promise is made entirely in hue, at one
// deliberately flat lightness, and about one man in twelve does not receive
// the hue it is sent in. The --viz-* palette was checked for this when it was
// built — tokens.css records teal being inserted specifically to break up an
// indigo/violet collapse under protanopia. Signal was never checked, and
// Signal carries the riskier ramps: a warm sand -> orange -> red three-step,
// and a cool blue -> cyan -> teal -> green four-step.
//
// NOTHING IS RE-HUED HERE. The palette is a floor-facing decision and the
// deliverable of this pass is evidence. What the test does is make that
// evidence a thing CI holds rather than a paragraph in a commit: every
// adjacent pair in the declared lifecycle ramp has to clear a separation
// floor, and each pair that does not is listed below WITH THE VALUE IT
// ACTUALLY MEASURES. So a future re-hue cannot quietly make any of it worse,
// cannot quietly fix one and leave the list lying, and cannot introduce a new
// collapse without saying so.

// distinctionFloor is the CIEDE2000 separation below which two status pills
// are treated as the same colour.
//
// It is not a standards number — there is no WCAG criterion for "these two
// hues are different from each other". 1.0 is a just-noticeable difference
// between large patches touching under ideal light; these are small pills,
// rows apart, on a shop-floor LCD under fluorescent tubes, being read by
// somebody who is not looking for the difference. 5.0 is where this palette's
// own distribution separates: across the 120 (theme x pair x vision)
// combinations measured, the best-channel values run 1.6, 1.7, 2.8, 3.2, 3.3,
// 4.2, 4.5, 4.6, 4.6, 4.6, 4.8, 4.9, 4.9, 4.96 and then jump to 5.1, 5.2, 5.4
// and up. The floor sits in the gap rather than being imported from anywhere.
const distinctionFloor = 5.0

// signalRamp is the lifecycle progression status-classes.css declares in its
// header: early -> submitted -> active -> success -> no-op -> attention ->
// failure -> cancelled. Adjacency here means "these two are one step apart in
// the story an operator is reading", which is where a confusion costs the
// most: mistaking `staged` for `delivered` means walking away from a bin that
// is still waiting.
//
// acknowledged and confirmed are omitted deliberately — they are declared as
// exact colour twins of submitted and delivered, so a separation check on
// them would measure 0.0 and mean nothing.
var signalRamp = []string{
	"pending", "sourcing", "queued", "submitted", "dispatched", "in_transit",
	"staged", "reshuffling", "delivered", "skipped", "faulted", "failed", "cancelled",
}

// signalConcernPairs are pairs that are NOT one step apart but share a hue
// family, so a collapse between them is just as expensive. Adjacency in a
// ramp is not the same thing as adjacency in colour space.
var signalConcernPairs = [][2]string{
	// The warm ramp's two ends. status-classes.css moved faulted off amber
	// to orange precisely so "quietly looking for material" could not be
	// confused with "a robot is stuck"; this asks whether the ramp's ENDS
	// stayed apart too, which is the question the adjacent check misses.
	//
	// It was the headline B2 result, and it is FIXED as of 2026-07-26.
	//
	// What it found: on the BACKGROUND channel alone — the channel the
	// palette header promises operators can read "without reading the label"
	// — the dark warm ramp flattened. sourcing #2b2410 and failed #450a0a
	// measured 22.5 apart for normal vision and 2.70 / 3.32 under protanopia
	// / deuteranopia, and they also REORDERED: the three simulated to
	// #252510 (sourcing), #2e2e05 (faulted) and #1e1e09 (failed), putting the
	// benign end beside the failure end with the attention state outside
	// them. It was never a knownCollapses entry because the pill's TEXT
	// rescued it (16.7 / 10.0) and the floor passes a pair on its best
	// channel — which is precisely how a background-channel defect hides
	// from a best-channel floor.
	//
	// Fixed by lifting dark sourcing to #3e3a1e — on lightness, not hue,
	// because measurement said the hue move the style guide asks for has
	// nowhere to go. Now 9.79 protanopia and 6.15 deuteranopia on the
	// background alone. The pair stays listed: it is the ends of a
	// three-step ramp, so it has to keep being measured, and a pair that
	// stops being checked once it passes is a pair that quietly regresses.
	{"sourcing", "failed"},
	// teal vs green: "bin is at the destination, waiting for the next step"
	// against "this order is finished". The most expensive confusion on the
	// board — one means go and do something, the other means do nothing.
	{"staged", "delivered"},
	// Both are quiet slate. skipped was deliberately kept off the cancelled
	// grey; nothing checked it against pending.
	{"pending", "skipped"},
}

// knownCollapse is a pair/vision/theme combination measured below the floor.
//
// Each carries the value it measures TODAY, and the test holds it to that
// value. That is the difference between a finding and an excuse: the entry
// cannot rot into "we decided this was fine", because the moment the number
// moves in either direction the test says so and somebody has to look. An
// entry that starts clearing the floor fails too, with "delete this entry" —
// an allowlist that keeps claiming a problem that has been fixed is how a
// ledger stops being read.
type knownCollapse struct {
	theme, a, b string
	vision      string
	measured    float64
	why         string
}

// The findings, in worst-first order. Thirteen of the 120 (theme x pair x
// vision) combinations measured fail the floor on BOTH channels at once. Far
// more fail on the background alone and are carried only by their label text
// — the property the palette header says operators should not have to rely on
// ("without reading the label"). Those are visible in the -v log's bg column
// and are not listed here, because a list of thirty-one entries stops being
// read; the two that matter most are written up where the pair is declared.
var knownCollapses = []knownCollapse{
	// ── The cool band under tritanopia. in_transit cyan, staged teal and
	// delivered green all land on one colour: the band is four hues at one
	// deliberately flat lightness, and tritanopia removes the axis they
	// differ on. Dark staged vs delivered TEXT is 0.21 — the same pixel.
	{"dark", "in_transit", "staged", "tritanopia", 1.601, "cool band collapses; text 1.56 does not rescue it"},
	{"dark", "staged", "delivered", "tritanopia", 1.715, "teal vs green; TEXT separation is 0.21, effectively identical"},
	{"light", "in_transit", "staged", "tritanopia", 2.764, "cool band; background alone is 1.15"},
	{"light", "staged", "delivered", "tritanopia", 3.205, "teal vs green; background alone is 1.17"},
	{"dark", "dispatched", "in_transit", "tritanopia", 4.624, "blue vs cyan, the top of the same cool band"},
	{"light", "dispatched", "in_transit", "tritanopia", 4.822, "blue vs cyan"},

	// ── pending vs skipped was five entries here and is now none of them.
	// It was the finding that needed no colour deficiency at all — two
	// quiet slates 0.58 apart in light and 1.41 in dark for NORMAL vision,
	// carried entirely by their labels. Fixed by re-stepping the pair on
	// the slate ramp rather than re-hueing either one (see
	// .badge-skipped in status-classes.css): backgrounds now separate by
	// 7.4 in light and 6.1 in dark, and by almost exactly that same number
	// under all three dichromacies, because lightness is the axis none of
	// them remove. This test is what said the entries had gone stale.

	// ── staged vs reshuffling under deuteranopia: teal against pink. Both
	// are mid-lightness and both lose their distinguishing channel.
	{"dark", "staged", "reshuffling", "deuteranopia", 3.255, "teal vs pink; backgrounds 1.09 apart"},
	{"light", "staged", "reshuffling", "deuteranopia", 4.962, "teal vs pink; backgrounds 1.76 apart"},

	// ── The warm ramp. Its ADJACENT steps hold up; what does not is the
	// pair below plus the dark sourcing/failed case in signalConcernPairs.
	{"light", "faulted", "failed", "tritanopia", 4.910, "orange vs red; backgrounds 2.28 apart — tritanopia flattens the warm end"},

	// ── The blue early/submitted boundary.
	{"dark", "queued", "submitted", "protanopia", 4.940, "periwinkle vs steel blue; backgrounds 1.68 apart"},
}

// TestCVDMathMatchesReference pins the simulation and the colour-difference
// maths before any finding computed with them is believed. Same argument as
// TestContrastMathMatchesWCAGReference: every assertion downstream is "this
// number is at least N", and a transposed matrix or a degrees/radians slip
// still produces numbers in the right band.
func TestCVDMathMatchesReference(t *testing.T) {
	// CIEDE2000 against Sharma, Wu & Dalal (2005)'s published test vectors —
	// the set chosen specifically to exercise the discontinuities most
	// implementations get wrong (hue wrap, the RT rotation term, near-neutral
	// chroma).
	sharma := []struct {
		p, q Lab
		want float64
	}{
		{Lab{50.0000, 2.6772, -79.7751}, Lab{50.0000, 0.0000, -82.7485}, 2.0425},
		{Lab{50.0000, 3.1571, -77.2803}, Lab{50.0000, 0.0000, -82.7485}, 2.8615},
		{Lab{50.0000, 2.8361, -74.0200}, Lab{50.0000, 0.0000, -82.7485}, 3.4412},
		{Lab{50.0000, -1.3802, -84.2814}, Lab{50.0000, 0.0000, -82.7485}, 1.0000},
		{Lab{50.0000, 0.0000, 0.0000}, Lab{50.0000, -1.0000, 2.0000}, 2.3669},
		{Lab{50.0000, 2.4900, -0.0010}, Lab{50.0000, -2.4900, 0.0009}, 7.1792},
		{Lab{50.0000, 2.5000, 0.0000}, Lab{50.0000, 0.0000, -2.5000}, 4.3065},
		{Lab{50.0000, 2.5000, 0.0000}, Lab{73.0000, 25.0000, -18.0000}, 27.1492},
		{Lab{60.2574, -34.0099, 36.2677}, Lab{60.4626, -34.1751, 39.4387}, 1.2644},
		{Lab{2.0776, 0.0795, -1.1350}, Lab{0.9033, -0.0636, -0.5514}, 0.9082},
		{Lab{22.7233, 20.0904, -46.6940}, Lab{23.0331, 14.9730, -42.5619}, 2.0373},
		{Lab{90.9257, -0.5406, -0.9208}, Lab{88.6381, -0.8985, -0.7239}, 1.5381},
	}
	for _, c := range sharma {
		if got := DeltaE2000(c.p, c.q); math.Abs(got-c.want) > 0.0002 {
			t.Errorf("DeltaE2000(%v, %v) = %.4f, want %.4f (Sharma/Wu/Dalal reference)\n"+
				"  Every CVD finding in this file is computed with this function.", c.p, c.q, got, c.want)
		}
	}

	// The Lab conversion itself.
	for _, c := range []struct {
		hex     string
		wantL   float64
		neutral bool
	}{
		{"#ffffff", 100.0, true},
		{"#808080", 53.585, true},
		{"#000000", 0.0, true},
	} {
		lab := RGBToLab(mustColor(t, c.hex))
		if math.Abs(lab.L-c.wantL) > 0.001 {
			t.Errorf("RGBToLab(%s).L = %.4f, want %.4f", c.hex, lab.L, c.wantL)
		}
		if c.neutral && (math.Abs(lab.A) > 0.001 || math.Abs(lab.B) > 0.001) {
			t.Errorf("RGBToLab(%s) = (%.4f, %.4f, %.4f); a neutral must land on a=b=0 or the D65 white point is wrong",
				c.hex, lab.L, lab.A, lab.B)
		}
	}

	// Dichromat simulation. Neutrals first: a projection that alters grey has
	// its matrices wrong, and the error would be invisible in a coloured
	// result. Then one saturated primary per type, in the direction the
	// literature describes — red reads olive to a protanope, blue reads teal
	// to a tritanope, and blue is untouched by both red-green deficiencies.
	for _, hex := range []string{"#000000", "#808080", "#ffffff"} {
		in := mustColor(t, hex)
		for _, v := range AllCVDTypes() {
			out := SimulateCVD(in, v)
			if DeltaE2000RGB(in, out) > 0.5 {
				t.Errorf("SimulateCVD(%s, %s) = %s — a neutral must survive the projection unchanged; the LMS matrices are wrong",
					hex, v, out.Hex())
			}
		}
	}
	for _, c := range []struct {
		in     string
		vision CVDType
		want   string
	}{
		{"#ff0000", Protanopia, "#737300"},
		{"#ff0000", Deuteranopia, "#9c9c00"},
		{"#ff0000", Tritanopia, "#ff0000"},
		{"#00ff00", Protanopia, "#ebeb0e"},
		{"#00ff00", Tritanopia, "#64f0f0"},
		{"#0000ff", Protanopia, "#0000ff"},
		{"#0000ff", Deuteranopia, "#0000ff"},
		{"#0000ff", Tritanopia, "#006363"},
	} {
		got := SimulateCVD(mustColor(t, c.in), c.vision).Hex()
		if got != c.want {
			t.Errorf("SimulateCVD(%s, %s) = %s, want %s", c.in, c.vision, got, c.want)
		}
	}
}

// TestSignalPaletteSeparatesUnderCVD is the B2 evidence, held by CI.
//
// It measures every adjacent pair of the lifecycle ramp, plus the three named
// concern pairs, in both themes, under normal vision and all three
// dichromacies. A pair passes on its BEST channel: pill background or label
// text, whichever separates more, because either one being distinct is enough
// to tell two rows apart.
//
// Run with -v to print the whole table; the log IS the report.
func TestSignalPaletteSeparatesUnderCVD(t *testing.T) {
	css := readShared(t, "status-classes.css")
	light, dark := badgePalette(t, css)
	themes := map[string]map[string]map[string]string{"light": light, "dark": dark}

	type key struct{ theme, a, b, vision string }
	known := map[key]knownCollapse{}
	for _, kc := range knownCollapses {
		k := key{kc.theme, kc.a, kc.b, kc.vision}
		if _, dup := known[k]; dup {
			t.Fatalf("knownCollapses lists %s/%s>%s/%s twice", kc.theme, kc.a, kc.b, kc.vision)
		}
		known[k] = kc
	}
	hit := map[key]bool{}

	var pairs [][2]string
	for i := 0; i < len(signalRamp)-1; i++ {
		pairs = append(pairs, [2]string{signalRamp[i], signalRamp[i+1]})
	}
	pairs = append(pairs, signalConcernPairs...)

	visions := []struct {
		name     string
		simulate func(RGB) RGB
	}{
		{"normal", func(c RGB) RGB { return c }},
	}
	for _, v := range AllCVDTypes() {
		visions = append(visions, struct {
			name     string
			simulate func(RGB) RGB
		}{v.String(), func(c RGB) RGB { return SimulateCVD(c, v) }})
	}

	var measured int
	for _, themeName := range []string{"light", "dark"} {
		decls := themes[themeName]
		t.Logf("── %s theme ─────────────────────────────────────────────", themeName)
		for _, p := range pairs {
			aD, aOK := decls["badge-"+p[0]]
			bD, bOK := decls["badge-"+p[1]]
			if !aOK || !bOK {
				t.Errorf("%s theme: cannot measure %s vs %s — one of them has no rule in status-classes.css", themeName, p[0], p[1])
				continue
			}
			line := fmt.Sprintf("  %-13s vs %-13s", p[0], p[1])
			for _, v := range visions {
				bgDelta, ok1 := channelDelta(t, aD, bD, "background", v.simulate)
				txDelta, ok2 := channelDelta(t, aD, bD, "color", v.simulate)
				if !ok1 || !ok2 {
					t.Errorf("%s theme: %s vs %s under %s did not resolve to literal colours on both channels",
						themeName, p[0], p[1], v.name)
					continue
				}
				best := math.Max(bgDelta, txDelta)
				measured++
				line += fmt.Sprintf("  %s bg%5.1f/tx%5.1f", v.name[:4], bgDelta, txDelta)

				k := key{themeName, p[0], p[1], v.name}
				kc, isKnown := known[k]
				switch {
				case isKnown:
					hit[k] = true
					if best >= distinctionFloor {
						t.Errorf("knownCollapses entry is STALE — %s theme, %s vs %s under %s now measures %.3f, at or above the %.1f floor.\n"+
							"  Delete the entry. A ledger that keeps reporting a problem somebody fixed stops being read.",
							themeName, p[0], p[1], v.name, best, distinctionFloor)
					} else if math.Abs(best-kc.measured) > 0.15 {
						t.Errorf("knownCollapses entry has DRIFTED — %s theme, %s vs %s under %s measures %.3f, recorded as %.3f.\n"+
							"  Something re-hued one of these. Update the recorded value and say in the commit which way it moved: %s",
							themeName, p[0], p[1], v.name, best, kc.measured, kc.why)
					}
				case best < distinctionFloor:
					t.Errorf("Signal separation FAIL — %s theme: %s and %s are %.2f apart (CIEDE2000, best of background %.2f / text %.2f) under %s, below the %.1f floor.\n"+
						"  backgrounds %s vs %s, text %s vs %s.\n"+
						"  Two lifecycle states render as the same pill. Either separate them, or add a knownCollapses entry recording the measurement and why it is acceptable.",
						themeName, p[0], p[1], best, bgDelta, txDelta, v.name, distinctionFloor,
						aD["background"], bD["background"], aD["color"], bD["color"])
				}
			}
			t.Log(line)
		}
	}

	for k, kc := range known {
		if !hit[k] {
			t.Errorf("knownCollapses lists %s theme %s vs %s under %s, but that combination was never measured.\n"+
				"  Either the pair left signalRamp/signalConcernPairs or the name is misspelled — an entry nothing evaluates is an allowlist reporting on comparisons that never ran. (%s)",
				kc.theme, kc.a, kc.b, kc.vision, kc.why)
		}
	}
	if measured == 0 {
		t.Fatal("no Signal pairs measured — the test is a no-op")
	}
}

// channelDelta returns the CIEDE2000 difference between two badges on one CSS
// property, as seen through the given vision.
func channelDelta(t *testing.T, a, b map[string]string, prop string, see func(RGB) RGB) (float64, bool) {
	t.Helper()
	ah, bh := a[prop], b[prop]
	if !isHex(ah) || !isHex(bh) {
		return 0, false
	}
	return DeltaE2000RGB(see(mustColor(t, ah)), see(mustColor(t, bh))), true
}
