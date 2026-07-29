package shared

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Colour science for the UI drift tests.
//
// This exists because the guard that came out of the invisible-chip incident
// checks CLASS PRESENCE, not contrast — see shingo-core/www/chip_drift_test.go.
// `.chip-err` shipped at 1.2:1 twice, and a presence check would have passed
// both times: the rule was there, it was just unreadable. A check named for
// contrast has to compute contrast.
//
// The functions are exported so the per-surface drift tests (Core's and
// Edge's www packages) can measure their own stylesheets with the same maths
// rather than each growing its own copy. Same reason chips.go is production
// code rather than a _test.go helper.
//
// Everything here is WCAG 2.2's definitions verbatim: sRGB, the 0.03928
// linearisation knee, the 0.2126/0.7152/0.0722 luminance weights, and the
// (L1+0.05)/(L2+0.05) ratio. TestContrastMathMatchesWCAGReference pins the
// output against the published worked examples, because a contrast test whose
// arithmetic is wrong is the same failure as a contrast test that never
// measured: green, and meaningless.

// RGB is an 8-bit-per-channel sRGB colour held as floats so a composite of a
// translucent fill over a surface does not lose precision to rounding before
// the luminance is taken. Channels are 0..255.
type RGB struct{ R, G, B float64 }

// ParseHexColor parses #rgb / #rrggbb (case-insensitive, leading # optional).
//
// It deliberately does NOT accept rgb()/hsl()/color-mix() — a caller handing
// one of those in has a value this package cannot honestly measure, and
// returning a zero colour would make it black and pass whatever floor was
// being tested. Fail loudly instead.
func ParseHexColor(s string) (RGB, error) {
	h := strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) != 6 {
		return RGB{}, fmt.Errorf("not a hex colour: %q", s)
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return RGB{}, fmt.Errorf("not a hex colour: %q", s)
	}
	return RGB{
		R: float64((v >> 16) & 0xff),
		G: float64((v >> 8) & 0xff),
		B: float64(v & 0xff),
	}, nil
}

// Hex renders the colour back to #rrggbb, for failure messages.
func (c RGB) Hex() string {
	clamp := func(x float64) int {
		return int(math.Round(math.Max(0, math.Min(255, x))))
	}
	return fmt.Sprintf("#%02x%02x%02x", clamp(c.R), clamp(c.G), clamp(c.B))
}

// linearize converts one 8-bit sRGB channel to linear light. WCAG 2.2's
// formula, knee included.
func linearize(channel float64) float64 {
	c := channel / 255.0
	if c <= 0.03928 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

// RelativeLuminance is WCAG 2.2's relative luminance, 0 (black) to 1 (white).
func RelativeLuminance(c RGB) float64 {
	return 0.2126*linearize(c.R) + 0.7152*linearize(c.G) + 0.0722*linearize(c.B)
}

// ContrastRatio is WCAG 2.2's contrast ratio between two colours, 1.0 to 21.0.
// Order does not matter; the lighter colour always takes the numerator.
func ContrastRatio(a, b RGB) float64 {
	la, lb := RelativeLuminance(a), RelativeLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// CompositeOver flattens a colour painted at the given alpha over an opaque
// backdrop, which is what the browser does before anyone reads the pixel.
//
// It matters for this codebase specifically: every health chip in
// shared/components.css is `color-mix(in srgb, <token> N%, transparent)` over
// the card, so the background a chip's text is actually read against is not
// any declared token — it is the composite. Measuring the token against the
// card instead would report a contrast nobody ever sees.
func CompositeOver(fg RGB, alpha float64, bg RGB) RGB {
	a := math.Max(0, math.Min(1, alpha))
	return RGB{
		R: a*fg.R + (1-a)*bg.R,
		G: a*fg.G + (1-a)*bg.G,
		B: a*fg.B + (1-a)*bg.B,
	}
}

// ─── WCAG conformance floors ─────────────────────────────────────────────
//
// Named rather than spelled as bare numbers at each call site, so a failure
// message can say which success criterion it is quoting and a reader can
// check the choice rather than take 4.5 on faith.

const (
	// AANormalText is SC 1.4.3 Contrast (Minimum), AA, for text below
	// 18.66px regular / 14px bold. Every badge label in this UI is 0.8rem
	// at weight 600 — under the large-text cut-off, so 4.5 is the floor
	// that applies, not 3.0.
	AANormalText = 4.5
	// AALargeText is SC 1.4.3 for large text.
	AALargeText = 3.0
	// AANonText is SC 1.4.11 Non-text Contrast: graphical objects required
	// to understand the content — chart series marks, tick marks, the
	// structural steps of the substrate ramp.
	AANonText = 3.0
)
