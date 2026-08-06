package scenemap

import (
	"math"
	"testing"
)

func areaFixture() Area {
	return Area{
		Name:  "08",
		Class: ClassReflectorArea,
		Polygon: []Point{
			{X: -1.279, Y: 0.363}, {X: -0.284, Y: 0.363},
			{X: -0.284, Y: 2.601}, {X: -1.279, Y: 2.601},
		},
		ColorPen:   4294901760,
		ColorBrush: 4294923520,
		Properties: map[string]any{"TextFontSize": int64(9), "useAutoReloc": true},
	}
}

// A cosmetic edit must not break the series.
//
// TextFontSize is present on all eleven Springfield areas and the colours on
// all of them too — and those colours are the editor's fixed default per
// class, identical across every ReflectorArea. Hashing the object naively
// means somebody enlarging a label breaks that zone's history, and the reader
// who chases it finds nothing, because nothing happened.
func TestAreaFingerprint_CosmeticEditsDoNotCount(t *testing.T) {
	base := AreaFingerprint(areaFixture())

	for name, mutate := range map[string]func(*Area){
		"label font size": func(a *Area) { a.Properties["TextFontSize"] = int64(14) },
		"outline colour":  func(a *Area) { a.ColorPen = 0xFF00FF00 },
		"fill colour":     func(a *Area) { a.ColorBrush = 0xFF00FF00 },
		"a new cosmetic":  func(a *Area) { a.Properties["LineWidth"] = 3.0 },
	} {
		a := areaFixture()
		mutate(&a)
		got := AreaFingerprint(a)
		if got.DefHash != base.DefHash {
			t.Errorf("%s changed DefHash — a cosmetic edit must not break the "+
				"series; add the key to cosmeticKeys with its reason", name)
		}
		if got.ShapeHash != base.ShapeHash {
			t.Errorf("%s changed ShapeHash — nothing moved", name)
		}
	}
}

// A behaviour edit MUST count, and must not look like a move.
//
// useAutoReloc decides whether the robot re-localizes in the zone, which is
// as localization-relevant as an edit gets. The lane equivalent is maxspeed,
// set on 91 of Springfield's 405 curves: a tech who slows a lane to fix a
// localization problem has made exactly the change this system exists to
// evaluate, and a shape-only hash reports it as continuous.
func TestAreaFingerprint_BehaviourCountsAndIsNotAMove(t *testing.T) {
	base := AreaFingerprint(areaFixture())

	a := areaFixture()
	a.Properties["useAutoReloc"] = false
	got := AreaFingerprint(a)

	if got.DefHash == base.DefHash {
		t.Error("useAutoReloc changed and DefHash did not — the series would read " +
			"as continuous across the change it exists to evaluate")
	}
	if got.ShapeHash != base.ShapeHash {
		t.Error("useAutoReloc changed ShapeHash — nothing moved, and a shape " +
			"change means samples re-attribute, which they must not here")
	}
}

// An unknown property counts, because nobody has decided it does not.
//
// A vendor firmware update that adds a field should break the series
// conservatively and get reviewed, rather than pass silently because the hash
// was never taught about it. Same rule as the wire drift test, one layer down.
func TestAreaFingerprint_UnknownPropertiesAreIncluded(t *testing.T) {
	base := AreaFingerprint(areaFixture())
	a := areaFixture()
	a.Properties["someNewVendorFlag"] = true
	if AreaFingerprint(a).DefHash == base.DefHash {
		t.Error("an unrecognised property left the fingerprint unchanged — " +
			"unknown fields must be INCLUDED so the next vendor addition is a " +
			"reviewed decision rather than a silent one")
	}
}

// Geometry counts in both hashes, because a move re-attributes samples.
func TestAreaFingerprint_GeometryCountsInBoth(t *testing.T) {
	base := AreaFingerprint(areaFixture())
	a := areaFixture()
	a.Polygon[2].X += 0.5
	got := AreaFingerprint(a)
	if got.ShapeHash == base.ShapeHash || got.DefHash == base.DefHash {
		t.Error("a moved vertex must change both hashes")
	}
	// Re-declaring the class is a shape change even with no vertex moved:
	// the polygon means something different, and the class is the strongest
	// predictor in the data.
	c := areaFixture()
	c.Class = ClassLocConfigArea
	if AreaFingerprint(c).ShapeHash == base.ShapeHash {
		t.Error("changing the area class must change ShapeHash — the same " +
			"polygon declaring a different kind of region is not the same object")
	}
}

// The hash must be stable across runs and across map iteration order.
//
// Go randomises map iteration, so a fingerprint that walks properties
// unsorted returns a different digest each run — which is not a hash, and
// would break every series on every sync.
func TestAreaFingerprint_IsStable(t *testing.T) {
	first := AreaFingerprint(areaFixture())
	for i := 0; i < 200; i++ {
		if got := AreaFingerprint(areaFixture()); got != first {
			t.Fatalf("fingerprint is not deterministic: run %d gave %+v, want %+v",
				i, got, first)
		}
	}
}

// Negative zero is the same coordinate as zero.
//
// This map genuinely carries negative zeros — the vendor writes them into
// geometry and publishes -0.0 for a missing confidence — so a sign bit
// surviving a round trip must not read as a move.
func TestFingerprint_NegativeZeroIsNotAMove(t *testing.T) {
	a := areaFixture()
	a.Polygon[0].X = 0
	b := areaFixture()
	b.Polygon[0].X = math.Copysign(0, -1)
	if AreaFingerprint(a).ShapeHash != AreaFingerprint(b).ShapeHash {
		t.Error("0 and -0 hashed differently — a sign bit off the wire would " +
			"read as an edit")
	}
}

// An absent reflector width is not a zero-width reflector.
func TestReflectorFingerprint_AbsentWidthIsItsOwnState(t *testing.T) {
	zero := 0.0
	absent := ReflectorFingerprint(Reflector{Kind: "cylinder", X: 1, Y: 2})
	measured := ReflectorFingerprint(Reflector{Kind: "cylinder", X: 1, Y: 2, Width: &zero})
	if absent.DefHash == measured.DefHash {
		t.Error("a reflector with no width hashed the same as one measured at " +
			"0.0 — later measuring it would look like nothing happened")
	}
	// A reflector has no behaviour, so the two hashes coincide by design.
	if absent.ShapeHash != absent.DefHash {
		t.Error("a reflector's two hashes should be equal — it has no behaviour")
	}
}

// The magnitude is what turns a hash change into a decision.
//
// Coordinates are stored to three decimals, so a re-registration nudging the
// plant 2 mm changes every hash in it. Without this number, that is thousands
// of series breaking in one day for a change that moved nothing; with it, the
// break decision happens at query time.
func TestMaxVertexDelta(t *testing.T) {
	before := []Point{{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}}

	// A rescan-scale nudge.
	nudged := []Point{{X: 0.002, Y: 0}, {X: 1.001, Y: 0}, {X: 1, Y: 1.002}}
	if d := MaxVertexDelta(before, nudged); math.Abs(d-0.002) > 1e-9 {
		t.Errorf("nudge delta = %v, want 0.002", d)
	}

	// A re-route-scale move.
	moved := []Point{{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 3.3, Y: 1}}
	if d := MaxVertexDelta(before, moved); math.Abs(d-2.3) > 1e-9 {
		t.Errorf("move delta = %v, want 2.3", d)
	}

	// A polygon that gained or lost a corner is a REDRAW, not a move, and
	// must not be given a small number. +Inf so any threshold treats it as a
	// break.
	redrawn := []Point{{X: 0, Y: 0}, {X: 1, Y: 0}}
	if d := MaxVertexDelta(before, redrawn); !math.IsInf(d, 1) {
		t.Errorf("a vertex-count change gave %v, want +Inf — measuring it would "+
			"invent a small number for a large change", d)
	}
	if d := MaxVertexDelta(before, before); d != 0 {
		t.Errorf("identical geometry gave %v, want 0", d)
	}
}

// Every cosmetic exclusion carries a reason, because the list is a decision
// to make a class of edit invisible.
func TestCosmeticKeys_EachHasAReason(t *testing.T) {
	for _, k := range []string{"TextFontSize", "TextColor", "LineWidth"} {
		if !IsCosmetic(k) {
			t.Errorf("%s should be cosmetic", k)
		}
		if CosmeticReason(k) == "" {
			t.Errorf("%s is excluded from every fingerprint with no reason recorded", k)
		}
	}
	if IsCosmetic("useAutoReloc") || IsCosmetic("maxspeed") {
		t.Error("a localization- or motion-relevant property must never be cosmetic")
	}
}
