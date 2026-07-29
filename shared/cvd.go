package shared

import "math"

// Colour-vision-deficiency simulation and perceptual difference, for holding
// the Signal palette to the claim its own header makes: that an operator
// scanning twenty rows can "distinguish dispatched / in_transit / staged /
// reshuffling at a glance without reading the label".
//
// That claim is about HUE, and roughly one man in twelve does not receive the
// hue the palette is sending. WCAG contrast says nothing about it — two
// colours can sit at a comfortable 6:1 against their own backgrounds and still
// be the same colour to each other for a dichromat. Contrast answers "can this
// be read"; this answers "can these two be told apart", and the Signal palette
// exists entirely to answer the second question.
//
// The --viz-* palette got this pass when it was built (tokens.css records teal
// being inserted to separate the indigo/violet collapse under protanopia).
// Signal never did, and Signal carries the riskier ramp.

// CVDType is a dichromacy. Anomalous trichromacy (the commoner, milder form —
// protanomaly, deuteranomaly) is deliberately not modelled: dichromacy is its
// limiting case, so a pair that separates for a dichromat separates for the
// anomalous trichromat too. Simulating the worst case is the only way to get a
// bound rather than an average.
type CVDType int

const (
	// Protanopia: no long-wavelength (red) cone. ~1% of men.
	Protanopia CVDType = iota
	// Deuteranopia: no medium-wavelength (green) cone. ~1% of men, and
	// deuteranomaly (its milder form) is another ~5%.
	Deuteranopia
	// Tritanopia: no short-wavelength (blue) cone. Rare (~1 in 10,000) and
	// not sex-linked. Included because ShinGo's ACTIVE band is a cool ramp —
	// blue to cyan to teal to green — and tritanopia is what flattens cool
	// ramps. Rarity is a reason to weigh a finding, not a reason not to
	// measure it.
	Tritanopia
)

func (c CVDType) String() string {
	switch c {
	case Protanopia:
		return "protanopia"
	case Deuteranopia:
		return "deuteranopia"
	case Tritanopia:
		return "tritanopia"
	}
	return "unknown"
}

// AllCVDTypes is the set a table-driven check walks.
func AllCVDTypes() []CVDType { return []CVDType{Protanopia, Deuteranopia, Tritanopia} }

// Viénot, Brettel & Mollon (1999). sRGB primaries into Hunt-Pointer-Estevez
// LMS, the dichromat projection, and back. The projection matrices below
// replace the missing cone's response with the plane the remaining two span,
// which is why a neutral survives untouched — a useful self-check, and one the
// test asserts rather than assumes.
var (
	rgbToLMS = [3][3]float64{
		{0.31399022, 0.63951294, 0.04649755},
		{0.15537241, 0.75789446, 0.08670142},
		{0.01775239, 0.10944209, 0.87256922},
	}
	lmsToRGB = [3][3]float64{
		{5.47221206, -4.64196010, 0.16963708},
		{-1.12524190, 2.29317094, -0.16789520},
		{0.02980165, -0.19318073, 1.16364789},
	}
	cvdProjection = map[CVDType][3][3]float64{
		Protanopia:   {{0, 1.05118294, -0.05116099}, {0, 1, 0}, {0, 0, 1}},
		Deuteranopia: {{1, 0, 0}, {0.95130920, 0, 0.04866992}, {0, 0, 1}},
		Tritanopia:   {{1, 0, 0}, {0, 1, 0}, {-0.86744736, 1.86727089, 0}},
	}
)

func apply(m [3][3]float64, v [3]float64) [3]float64 {
	return [3]float64{
		m[0][0]*v[0] + m[0][1]*v[1] + m[0][2]*v[2],
		m[1][0]*v[0] + m[1][1]*v[1] + m[1][2]*v[2],
		m[2][0]*v[0] + m[2][1]*v[1] + m[2][2]*v[2],
	}
}

// delinearize is linearize's inverse — linear light back to an 8-bit sRGB
// channel, clamped into gamut.
func delinearize(c float64) float64 {
	c = math.Max(0, math.Min(1, c))
	if c <= 0.0031308 {
		return 255 * 12.92 * c
	}
	return 255 * (1.055*math.Pow(c, 1/2.4) - 0.055)
}

// SimulateCVD returns the colour as a dichromat of the given type receives it.
// The transform runs in LINEAR light; doing it on gamma-encoded channels is
// the standard way to get a plausible-looking wrong answer.
func SimulateCVD(c RGB, t CVDType) RGB {
	lin := [3]float64{linearize(c.R), linearize(c.G), linearize(c.B)}
	out := apply(lmsToRGB, apply(cvdProjection[t], apply(rgbToLMS, lin)))
	return RGB{R: delinearize(out[0]), G: delinearize(out[1]), B: delinearize(out[2])}
}

// ─── CIELAB and CIEDE2000 ────────────────────────────────────────────────
//
// Contrast ratio is the wrong instrument for "are these two the same colour":
// it is a pure luminance quotient, so two hues at identical lightness score
// 1.00:1 no matter how far apart they look. Signal's whole ACTIVE band is
// built that way on purpose — four hues at one calm weight — so measuring it
// with contrast would report the band as maximally collapsed when it is
// working exactly as designed. Perceptual difference is the instrument that
// answers the question actually being asked.

// Lab is CIELAB under a D65 white point.
type Lab struct{ L, A, B float64 }

// RGBToLab converts sRGB to CIELAB (D65).
func RGBToLab(c RGB) Lab {
	r, g, b := linearize(c.R), linearize(c.G), linearize(c.B)
	x := 0.4124564*r + 0.3575761*g + 0.1804375*b
	y := 0.2126729*r + 0.7151522*g + 0.0721750*b
	z := 0.0193339*r + 0.1191920*g + 0.9503041*b
	const xn, yn, zn = 0.95047, 1.0, 1.08883
	f := func(t float64) float64 {
		if t > 216.0/24389.0 {
			return math.Cbrt(t)
		}
		return (841.0/108.0)*t + 4.0/29.0
	}
	fx, fy, fz := f(x/xn), f(y/yn), f(z/zn)
	return Lab{L: 116*fy - 16, A: 500 * (fx - fy), B: 200 * (fy - fz)}
}

// DeltaE2000 is the CIEDE2000 colour difference, kL=kC=kH=1.
//
// Rough reading: 1.0 is a just-noticeable difference between two large patches
// touching each other under ideal light. Two small pills, several rows apart,
// on a shop-floor LCD under fluorescent tubes, need considerably more.
func DeltaE2000(p, q Lab) float64 {
	c1 := math.Hypot(p.A, p.B)
	c2 := math.Hypot(q.A, q.B)
	cBar := (c1 + c2) / 2
	cBar7 := math.Pow(cBar, 7)
	g := 0.5 * (1 - math.Sqrt(cBar7/(cBar7+math.Pow(25, 7))))

	a1p, a2p := (1+g)*p.A, (1+g)*q.A
	c1p, c2p := math.Hypot(a1p, p.B), math.Hypot(a2p, q.B)

	hue := func(a, b float64) float64 {
		if a == 0 && b == 0 {
			return 0
		}
		h := math.Atan2(b, a) * 180 / math.Pi
		if h < 0 {
			h += 360
		}
		return h
	}
	h1p, h2p := hue(a1p, p.B), hue(a2p, q.B)

	dLp := q.L - p.L
	dCp := c2p - c1p

	var dhp float64
	switch {
	case c1p*c2p == 0:
		dhp = 0
	case math.Abs(h2p-h1p) <= 180:
		dhp = h2p - h1p
	case h2p-h1p > 180:
		dhp = h2p - h1p - 360
	default:
		dhp = h2p - h1p + 360
	}
	dHp := 2 * math.Sqrt(c1p*c2p) * math.Sin(dhp*math.Pi/360)

	lBar := (p.L + q.L) / 2
	cBarP := (c1p + c2p) / 2

	var hBarP float64
	switch {
	case c1p*c2p == 0:
		hBarP = h1p + h2p
	case math.Abs(h1p-h2p) <= 180:
		hBarP = (h1p + h2p) / 2
	case h1p+h2p < 360:
		hBarP = (h1p + h2p + 360) / 2
	default:
		hBarP = (h1p + h2p - 360) / 2
	}

	rad := func(d float64) float64 { return d * math.Pi / 180 }
	tTerm := 1 - 0.17*math.Cos(rad(hBarP-30)) + 0.24*math.Cos(rad(2*hBarP)) +
		0.32*math.Cos(rad(3*hBarP+6)) - 0.20*math.Cos(rad(4*hBarP-63))

	dTheta := 30 * math.Exp(-math.Pow((hBarP-275)/25, 2))
	cBarP7 := math.Pow(cBarP, 7)
	rC := 2 * math.Sqrt(cBarP7/(cBarP7+math.Pow(25, 7)))
	sL := 1 + (0.015*math.Pow(lBar-50, 2))/math.Sqrt(20+math.Pow(lBar-50, 2))
	sC := 1 + 0.045*cBarP
	sH := 1 + 0.015*cBarP*tTerm
	rT := -math.Sin(rad(2*dTheta)) * rC

	tl, tc, th := dLp/sL, dCp/sC, dHp/sH
	return math.Sqrt(tl*tl + tc*tc + th*th + rT*tc*th)
}

// DeltaE2000RGB is DeltaE2000 over two sRGB colours.
func DeltaE2000RGB(a, b RGB) float64 { return DeltaE2000(RGBToLab(a), RGBToLab(b)) }
