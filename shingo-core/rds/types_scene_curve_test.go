package rds

import (
	"encoding/json"
	"math"
	"testing"
)

// The fixtures below are VERBATIM advanced curves from Springfield's live
// scene (GET http://10.222.10.76:8088/scene, 2026-07-26), with only `property`
// and `devices` emptied — property carries one key, bindRobotMap, listing
// twelve robot/map bindings and no geometry at all.
//
// They are here because the field names were asserted from a brief once
// before and the brief was wrong: it placed the control points inside
// `property`. Property does exist and the adapter does discard it, both true
// and both beside the point. A fixture cut from the real payload is the only
// thing that settles where SEER actually writes the handles, and pinning it
// means the next reader does not have to re-open a VPN to find out.
const (
	// A real curved lane. Free handles, both off the chord.
	sprBezierPath = `{"className":"BezierPath","controlPos1":{"x":-0.287,"y":22.094,"z":0},"controlPos2":{"x":0.303,"y":22.142,"z":0},"desc":"","devices":[],"endPos":{"className":"","desc":"","dir":0,"ignoreDir":false,"instanceName":"PP224","pos":{"x":0.986,"y":22.169,"z":0},"property":[]},"instanceName":"LM9-PP224","property":[],"startPos":{"className":"","desc":"","dir":0,"ignoreDir":false,"instanceName":"LM9","pos":{"x":-0.604,"y":22.449,"z":0},"property":[]}}`

	// DegenerateBezier does NOT mean straight. This one bows 1.30 m off its
	// 7.17 m chord — the widest gap in the scene.
	sprDegenerateCurved = `{"className":"DegenerateBezier","controlPos1":{"x":-0.198,"y":36.401,"z":0},"controlPos2":{"x":-5.065,"y":36.951,"z":0},"desc":"","devices":[],"endPos":{"className":"","desc":"","dir":0,"ignoreDir":false,"instanceName":"LM113","pos":{"x":-6.572,"y":36.951,"z":0},"property":[]},"instanceName":"LM10-LM113","property":[],"startPos":{"className":"","desc":"","dir":0,"ignoreDir":false,"instanceName":"LM10","pos":{"x":-0.254,"y":33.554,"z":0},"property":[]}}`

	// The other DegenerateBezier shape: handles parked exactly on the chord's
	// thirds, which is how a cubic spells "straight line".
	sprDegenerateStraight = `{"className":"DegenerateBezier","controlPos1":{"x":-36.455,"y":58.029,"z":0},"controlPos2":{"x":-37.452,"y":58.093,"z":0},"desc":"","devices":[],"endPos":{"className":"","desc":"","dir":0,"ignoreDir":false,"instanceName":"LM135","pos":{"x":-38.448,"y":58.156,"z":0},"property":[]},"instanceName":"AP123-LM135","property":[],"startPos":{"className":"","desc":"","dir":0,"ignoreDir":false,"instanceName":"AP123","pos":{"x":-35.459,"y":57.966,"z":0},"property":[]}}`

	// StraightPath, zero-sentinel encoding. Reading these as geometry bends a
	// 1.4 m aisle 52 m through the origin.
	sprStraightZeroed = `{"className":"StraightPath","controlPos1":{"x":0,"y":0,"z":0},"controlPos2":{"x":0,"y":0,"z":0},"desc":"","devices":[],"endPos":{"className":"","desc":"","dir":0,"ignoreDir":false,"instanceName":"LM137","pos":{"x":-37.034,"y":59.446,"z":0},"property":[]},"instanceName":"LM197-LM137","property":[],"startPos":{"className":"","desc":"","dir":0,"ignoreDir":false,"instanceName":"LM197","pos":{"x":-37.095,"y":60.847,"z":0},"property":[]}}`

	// StraightPath, keys-absent encoding. AP102-LM100 and LM100-AP102 are the
	// SAME aisle in the two directions, and the scene spells "no handles"
	// differently on each — which is why absence alone is not the test.
	sprStraightAbsent     = `{"className":"StraightPath","desc":"","devices":[],"endPos":{"className":"","desc":"","dir":0,"ignoreDir":false,"instanceName":"LM100","pos":{"x":-0.544,"y":11.787,"z":0},"property":[]},"instanceName":"AP102-LM100","property":[],"startPos":{"className":"","desc":"","dir":0,"ignoreDir":false,"instanceName":"AP102","pos":{"x":0.886,"y":11.807,"z":0},"property":[]}}`
	sprStraightZeroedTwin = `{"className":"StraightPath","controlPos1":{"x":0,"y":0,"z":0},"controlPos2":{"x":0,"y":0,"z":0},"desc":"","devices":[],"endPos":{"className":"","desc":"","dir":0,"ignoreDir":false,"instanceName":"AP102","pos":{"x":0.886,"y":11.807,"z":0},"property":[]},"instanceName":"LM100-AP102","property":[],"startPos":{"className":"","desc":"","dir":0,"ignoreDir":false,"instanceName":"LM100","pos":{"x":-0.544,"y":11.787,"z":0},"property":[]}}`
)

func decodeCurve(t *testing.T, raw string) AdvancedCurve {
	t.Helper()
	var c AdvancedCurve
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("unmarshal advanced curve: %v", err)
	}
	return c
}

// TestAdvancedCurve_ControlPointsSurviveDecode is the assertion the whole
// change rests on: the handles reach the struct with their VALUES intact.
// Before this field existed they were dropped silently at decode, and every
// consumer downstream drew the chord.
func TestAdvancedCurve_ControlPointsSurviveDecode(t *testing.T) {
	t.Parallel()

	c := decodeCurve(t, sprBezierPath)
	if c.ControlPos1 == nil || c.ControlPos2 == nil {
		t.Fatal("BezierPath decoded with nil control handles — the top-level " +
			"controlPos1/controlPos2 keys are not being modeled")
	}
	if got, want := *c.ControlPos1, (Pos3D{X: -0.287, Y: 22.094, Z: 0}); got != want {
		t.Errorf("controlPos1 = %+v, want %+v", got, want)
	}
	if got, want := *c.ControlPos2, (Pos3D{X: 0.303, Y: 22.142, Z: 0}); got != want {
		t.Errorf("controlPos2 = %+v, want %+v", got, want)
	}
	// The endpoints must not have moved.
	if got, want := c.StartPos.Pos, (Pos3D{X: -0.604, Y: 22.449, Z: 0}); got != want {
		t.Errorf("startPos.pos = %+v, want %+v", got, want)
	}
	if got, want := c.EndPos.Pos, (Pos3D{X: 0.986, Y: 22.169, Z: 0}); got != want {
		t.Errorf("endPos.pos = %+v, want %+v", got, want)
	}
}

// TestAdvancedCurve_ControlPointsUsability walks the four shapes Springfield
// actually emits. The two StraightPath cases are the ones that matter: both
// mean "no handles", and only one of them says so by omission.
func TestAdvancedCurve_ControlPointsUsability(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"BezierPath: free handles", sprBezierPath, true},
		{"DegenerateBezier bowing off its chord", sprDegenerateCurved, true},
		{"DegenerateBezier parked on its chord thirds", sprDegenerateStraight, true},
		{"StraightPath spelling no-handles as {0,0,0}", sprStraightZeroed, false},
		{"StraightPath spelling no-handles by omission", sprStraightAbsent, false},
		{"the same aisle, other direction, other spelling", sprStraightZeroedTwin, false},
	} {
		c := decodeCurve(t, tc.raw)
		if _, _, ok := c.ControlPoints(); ok != tc.want {
			t.Errorf("%s (%s): ControlPoints ok = %v, want %v",
				tc.name, c.InstanceName, ok, tc.want)
		}
	}
}

// TestAdvancedCurve_ZeroSentinelIsNotGeometry states the consequence in metres
// rather than in booleans. If the sentinel ever starts reading as geometry,
// this fails with the size of the error rather than with a false.
//
// LM197-LM137 is a 1.4 m aisle. Its zero handles put the cubic's midpoint 26 m
// away, near the origin, and the drawn lane sweeps 52 m off the chord.
func TestAdvancedCurve_ZeroSentinelIsNotGeometry(t *testing.T) {
	t.Parallel()

	c := decodeCurve(t, sprStraightZeroed)
	c1, c2, ok := c.ControlPoints()
	if ok {
		t.Fatalf("zero handles read as geometry: mid-curve lands %.1f m off a %.1f m aisle",
			maxChordDeviation(c.StartPos.Pos, c1, c2, c.EndPos.Pos),
			math.Hypot(c.EndPos.Pos.X-c.StartPos.Pos.X, c.EndPos.Pos.Y-c.StartPos.Pos.Y))
	}

	// And the guard is not vacuous: the same helper on a real curve reports a
	// deviation the map can see. Asserted as a band so retuning the fixture
	// cannot hollow the check out.
	b := decodeCurve(t, sprDegenerateCurved)
	bc1, bc2, ok := b.ControlPoints()
	if !ok {
		t.Fatal("LM10-LM113 must expose usable handles")
	}
	if d := maxChordDeviation(b.StartPos.Pos, bc1, bc2, b.EndPos.Pos); d < 1.0 || d > 1.6 {
		t.Errorf("LM10-LM113 deviates %.3f m from its chord, want 1.0–1.6 m", d)
	}

	// A chord-thirds DegenerateBezier is a straight line drawn as a cubic:
	// usable handles, no deviation. Rendering it as a cubic is a no-op, which
	// is why the render rule keys on the handles and not on the class name.
	s := decodeCurve(t, sprDegenerateStraight)
	sc1, sc2, ok := s.ControlPoints()
	if !ok {
		t.Fatal("AP123-LM135 must expose usable handles")
	}
	if d := maxChordDeviation(s.StartPos.Pos, sc1, sc2, s.EndPos.Pos); d > 0.001 {
		t.Errorf("AP123-LM135 deviates %.4f m from its chord, want ~0 (handles sit on the thirds)", d)
	}
}

// maxChordDeviation is the widest gap between the cubic through
// p0,c1,c2,p3 and the straight chord p0→p3, in metres. Test-only: the
// renderer needs the curve, not the number, and this exists so the
// assertions above can talk about the floor instead of about floats.
func maxChordDeviation(p0, c1, c2, p3 Pos3D) float64 {
	worst := 0.0
	for i := 0; i <= 64; i++ {
		t := float64(i) / 64
		mt := 1 - t
		x := mt*mt*mt*p0.X + 3*mt*mt*t*c1.X + 3*mt*t*t*c2.X + t*t*t*p3.X
		y := mt*mt*mt*p0.Y + 3*mt*mt*t*c1.Y + 3*mt*t*t*c2.Y + t*t*t*p3.Y
		if d := pointToSegment(x, y, p0, p3); d > worst {
			worst = d
		}
	}
	return worst
}

func pointToSegment(px, py float64, a, b Pos3D) float64 {
	dx, dy := b.X-a.X, b.Y-a.Y
	l2 := dx*dx + dy*dy
	if l2 == 0 {
		return math.Hypot(px-a.X, py-a.Y)
	}
	t := ((px-a.X)*dx + (py-a.Y)*dy) / l2
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(px-(a.X+t*dx), py-(a.Y+t*dy))
}
