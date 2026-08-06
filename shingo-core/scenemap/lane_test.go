package scenemap

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func pt(x, y float64) *Point { return &Point{X: x, Y: y} }

// LM73-LM14 and LM14-LM73, the pair the whole lane key exists for: one piece
// of floor that showed 48 readings under one name and 116 under the other.
func twinPair() []LaneEdge {
	return []LaneEdge{
		{Instance: "LM73-LM14", Class: "StraightPath", FromName: "LM73", ToName: "LM14",
			From: Point{X: 0, Y: 0}, To: Point{X: 5, Y: 0}},
		{Instance: "LM14-LM73", Class: "StraightPath", FromName: "LM14", ToName: "LM73",
			From: Point{X: 5, Y: 0}, To: Point{X: 0, Y: 0}},
	}
}

// The version must not depend on which direction the query returned first.
//
// Recording only one twin's geometry, or hashing in slice order, makes the
// version row a coin flip on iteration order — and every series break after
// it a phantom.
func TestFingerprintLane_IsOrderIndependent(t *testing.T) {
	fwd := twinPair()
	rev := []LaneEdge{fwd[1], fwd[0]}

	a, err := FingerprintLane(fwd)
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	b, err := FingerprintLane(rev)
	if err != nil {
		t.Fatalf("reversed: %v", err)
	}
	if a.Fingerprint != b.Fingerprint {
		t.Errorf("row order changed the fingerprint: %+v vs %+v", a.Fingerprint, b.Fingerprint)
	}
	if a.Directed != 2 || !a.TwinsAgree {
		t.Errorf("a mirrored pair should be Directed=2 TwinsAgree=true, got %d/%v",
			a.Directed, a.TwinsAgree)
	}
}

// Divergent twins are a VISIBLE EVENT, not a silent pick.
//
// RoboShop lets each direction be drawn separately. Springfield's 193 pairs
// all mirror today, so lane-grain is safe — and this flag is what makes the
// day that stops being true findable rather than a coin toss over which twin
// the version row happened to record.
func TestFingerprintLane_DivergentTwinsAreFlagged(t *testing.T) {
	for name, mutate := range map[string]func([]LaneEdge) []LaneEdge{
		"one direction re-classed": func(e []LaneEdge) []LaneEdge {
			e[1].Class = "BezierPath"
			return e
		},
		"one direction bowed": func(e []LaneEdge) []LaneEdge {
			e[1].Ctrl1, e[1].Ctrl2 = pt(4, 1), pt(1, 1)
			return e
		},
		"endpoints no longer meet": func(e []LaneEdge) []LaneEdge {
			e[1].From = Point{X: 5, Y: 3}
			return e
		},
	} {
		v, err := FingerprintLane(mutate(twinPair()))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if v.TwinsAgree {
			t.Errorf("%s: twins disagree and the flag says otherwise", name)
		}
		if v.Disagreement == "" {
			t.Errorf("%s: no reason recorded — the log line has nothing to say", name)
		}
		// The row is still produced. Refusing to version the lane would lose
		// it entirely, which is worse than versioning it with a caveat.
		if v.ShapeHash == "" {
			t.Errorf("%s: a disagreeing lane must still get a fingerprint", name)
		}
	}

	// Handles that do NOT swap are a lane drawn twice, not a lane read both
	// ways. Same geometry, wrong correspondence.
	e := twinPair()
	e[0].Ctrl1, e[0].Ctrl2 = pt(1, 1), pt(4, 1)
	e[1].Ctrl1, e[1].Ctrl2 = pt(1, 1), pt(4, 1) // should be swapped
	v, err := FingerprintLane(e)
	if err != nil {
		t.Fatalf("unswapped handles: %v", err)
	}
	if v.TwinsAgree {
		t.Error("handles that do not swap across the twins must not read as agreeing")
	}
}

// A one-way lane has no twin, so agreement is vacuously true and Directed
// says so. Nineteen of Springfield's 212 lanes are one-way, including the
// whole LM13-LM141-LM140-LM14 corridor.
func TestFingerprintLane_OneWayLane(t *testing.T) {
	v, err := FingerprintLane([]LaneEdge{{
		Instance: "LM13-LM141", Class: "StraightPath", FromName: "LM13", ToName: "LM141",
		From: Point{X: 2.7, Y: 49}, To: Point{X: 2.8, Y: 52},
	}})
	if err != nil {
		t.Fatalf("one-way: %v", err)
	}
	if v.Directed != 1 {
		t.Errorf("Directed = %d, want 1", v.Directed)
	}
	if !v.TwinsAgree || v.Disagreement != "" {
		t.Error("a one-way lane has nothing to disagree with")
	}
}

// Geometry changes the hash; an absent handle pair is not a pair at the
// origin.
//
// The all-zero pair is SEER's "no handles" sentinel and the origin is a real
// coordinate on this map — Springfield has scene points within a metre of it
// — so hashing them alike would make a lane that gained handles at (0,0)
// invisible.
func TestFingerprintLane_AbsentHandlesAreNotTheOrigin(t *testing.T) {
	straight := twinPair()
	base, err := FingerprintLane(straight)
	if err != nil {
		t.Fatal(err)
	}

	atOrigin := twinPair()
	atOrigin[0].Ctrl1, atOrigin[0].Ctrl2 = pt(0, 0), pt(0, 0)
	atOrigin[1].Ctrl1, atOrigin[1].Ctrl2 = pt(0, 0), pt(0, 0)
	got, err := FingerprintLane(atOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if got.ShapeHash == base.ShapeHash {
		t.Error("handles at the origin hashed the same as no handles at all")
	}

	moved := twinPair()
	moved[0].To.X = 5.5
	moved[1].From.X = 5.5
	m, err := FingerprintLane(moved)
	if err != nil {
		t.Fatal(err)
	}
	if m.ShapeHash == base.ShapeHash {
		t.Error("a moved endpoint must change the fingerprint")
	}
}

// A lane with an unnamed endpoint has no key and therefore no version row.
//
// This is the other half of the unkeyable-edge rule: the roll-up quarantines
// its samples, and the sync must not manufacture a version for it either.
func TestFingerprintLane_RejectsUnkeyableAndMalformedInput(t *testing.T) {
	if _, err := FingerprintLane(nil); err == nil {
		t.Error("an empty lane must be an error, not a fingerprint of nothing")
	}
	if _, err := FingerprintLane([]LaneEdge{{Instance: "E1", FromName: "LM1"}}); err == nil {
		t.Error("an unnamed endpoint must be rejected — it has no lane key")
	}
	// Two edges that are not twins.
	mixed := []LaneEdge{
		{Instance: "LM1-LM2", FromName: "LM1", ToName: "LM2"},
		{Instance: "LM3-LM4", FromName: "LM3", ToName: "LM4"},
	}
	if _, err := FingerprintLane(mixed); err == nil {
		t.Error("edges from two different lanes must not fingerprint as one")
	}
	// More rows than a lane can have.
	three := append(twinPair(), LaneEdge{
		Instance: "LM73-LM14-dup", FromName: "LM73", ToName: "LM14"})
	if _, err := FingerprintLane(three); err == nil {
		t.Error("three directed rows for one lane must be an error")
	}
}

func TestLaneKey(t *testing.T) {
	if got := LaneKey("LM73", "LM14"); got != "LM14-LM73" {
		t.Errorf("LaneKey = %q, want the sorted pair", got)
	}
	if LaneKey("LM73", "LM14") != LaneKey("LM14", "LM73") {
		t.Error("the key must not depend on direction")
	}
	if LaneKey("", "LM14") != "" || LaneKey("LM73", "") != "" {
		t.Error("an unnamed endpoint yields no key")
	}
}

// LaneShape must pick the same direction on both syncs, or a re-sync that
// merely reordered rows reports the lane as having moved its own length.
func TestLaneShape_IsCanonicalAcrossRowOrder(t *testing.T) {
	fwd := twinPair()
	rev := []LaneEdge{fwd[1], fwd[0]}
	a, b := LaneShape(fwd), LaneShape(rev)
	if len(a) != len(b) {
		t.Fatalf("shape lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("vertex %d differs by row order: %+v vs %+v", i, a[i], b[i])
		}
	}
	if d := MaxVertexDelta(a, b); d != 0 {
		t.Errorf("row order alone produced a movement of %v m", d)
	}
	// A curved lane contributes its handles, so a bow change is measurable.
	curved := twinPair()
	curved[0].Ctrl1, curved[0].Ctrl2 = pt(1, 1), pt(4, 1)
	curved[1].Ctrl1, curved[1].Ctrl2 = pt(4, 1), pt(1, 1)
	if got := len(LaneShape(curved)); got != 4 {
		t.Errorf("a curved lane's shape has %d vertices, want 4", got)
	}
}

// The real scene, both directions, straight from the fixture: every twin pair
// agrees, which is what makes lane-grain versioning safe at this plant today.
func TestFingerprintLane_RealSpringfieldPairsAgree(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash("testdata/spramrmap-trimmed.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var doc struct {
		Curves []struct {
			ClassName    string `json:"className"`
			InstanceName string `json:"instanceName"`
			StartPos     struct {
				InstanceName string `json:"instanceName"`
				Pos          Point  `json:"pos"`
			} `json:"startPos"`
			EndPos struct {
				InstanceName string `json:"instanceName"`
				Pos          Point  `json:"pos"`
			} `json:"endPos"`
			Ctrl1 *Point `json:"controlPos1"`
			Ctrl2 *Point `json:"controlPos2"`
		} `json:"advancedCurveList"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}

	byLane := map[string][]LaneEdge{}
	for _, c := range doc.Curves {
		e := LaneEdge{
			Instance: c.InstanceName, Class: c.ClassName,
			FromName: c.StartPos.InstanceName, ToName: c.EndPos.InstanceName,
			From: c.StartPos.Pos, To: c.EndPos.Pos, Ctrl1: c.Ctrl1, Ctrl2: c.Ctrl2,
		}
		k := LaneKey(e.FromName, e.ToName)
		byLane[k] = append(byLane[k], e)
	}

	var pairs int
	for k, edges := range byLane {
		v, err := FingerprintLane(edges)
		if err != nil {
			t.Fatalf("lane %s: %v", k, err)
		}
		if v.Directed == 2 {
			pairs++
			if !v.TwinsAgree {
				t.Errorf("lane %s: twins disagree (%s) — measured across all 193 "+
					"Springfield pairs on 2026-08-06 they mirrored to 0.000000000000 m, "+
					"so this is either new or a parsing regression", k, v.Disagreement)
			}
		}
	}
	// LM10-LM48 / LM48-LM10 is in the fixture precisely so a real pair is
	// exercised rather than only synthetic ones.
	if pairs == 0 {
		t.Fatal("the fixture no longer contains a reciprocal pair, so twin " +
			"agreement is untested against real geometry")
	}
}

// A rescan-scale nudge and a re-route-scale move must be distinguishable on
// real lane geometry, because that distinction is what stops a 2 mm
// re-registration breaking every series in the plant.
func TestLaneShape_MovementScalesAreDistinguishable(t *testing.T) {
	before := LaneShape(twinPair())

	nudged := twinPair()
	nudged[0].To.X += 0.002
	nudged[1].From.X += 0.002
	if d := MaxVertexDelta(before, LaneShape(nudged)); math.Abs(d-0.002) > 1e-9 {
		t.Errorf("nudge = %v m, want 0.002", d)
	}

	rerouted := twinPair()
	rerouted[0].To.X += 2.3
	rerouted[1].From.X += 2.3
	if d := MaxVertexDelta(before, LaneShape(rerouted)); math.Abs(d-2.3) > 1e-9 {
		t.Errorf("re-route = %v m, want 2.3", d)
	}

	// A lane that gained handles changed vertex count: a redraw, not a move.
	bowed := twinPair()
	bowed[0].Ctrl1, bowed[0].Ctrl2 = pt(1, 1), pt(4, 1)
	bowed[1].Ctrl1, bowed[1].Ctrl2 = pt(4, 1), pt(1, 1)
	if d := MaxVertexDelta(before, LaneShape(bowed)); !math.IsInf(d, 1) {
		t.Errorf("a lane that gained handles gave %v, want +Inf", d)
	}
}
