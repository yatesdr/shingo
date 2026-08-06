package scenemap

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// The fixture is a trimmed slice of Springfield's real SPRAMRMAP, pulled
// 2026-08-06 via Robokit #4011. All eleven areas and all seventy-one
// reflectors are kept verbatim because they ARE the finding; the drivable
// network is reduced to one curve of each class, and both scan clouds to
// three points each so the trap they set is still testable.
func load(t *testing.T) *Map {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash("testdata/spramrmap-trimmed.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

// THE FINDING, pinned against the real map.
//
// Nine ReflectorArea polygons declare reflector localization and contain zero
// reflectors between them. 849 of 850 no-estimate readings at Springfield
// fall inside them. If this test ever fails because a count moved off zero,
// somebody installed reflectors and that is the best possible reason for a
// red build.
func TestParse_NineReflectorAreasHoldNoReflectors(t *testing.T) {
	m := load(t)

	if len(m.Areas) != 11 {
		t.Fatalf("areas = %d, want 11", len(m.Areas))
	}
	if len(m.Reflectors) != 71 {
		t.Fatalf("reflectors = %d, want 71", len(m.Reflectors))
	}

	var reflectorAreas, empty, inAnyArea int
	for _, a := range m.Areas {
		if a.Class != ClassReflectorArea {
			continue
		}
		reflectorAreas++
		n := m.ReflectorsInside(a)
		if n == 0 {
			empty++
		} else {
			t.Logf("area %s now holds %d reflectors", a.Name, n)
		}
	}
	for _, a := range m.Areas {
		inAnyArea += m.ReflectorsInside(a)
	}

	if reflectorAreas != 9 {
		t.Errorf("ReflectorArea count = %d, want 9", reflectorAreas)
	}
	if empty != 9 {
		t.Errorf("%d of %d ReflectorAreas are empty, want all 9 — if a count moved "+
			"off zero, reflectors were installed and this expectation is stale",
			empty, reflectorAreas)
	}
	// Seven of the seventy-one fall inside any declared area at all, and that
	// area is 05 — a LocConfigArea, which is not a reflector zone. Sixty-four
	// sit outside every declared area in the plant.
	if inAnyArea != 7 {
		t.Errorf("reflectors inside any declared area = %d, want 7", inAnyArea)
	}
}

// The class is the predictor, so it has to survive parsing exactly.
//
// Areas 04 and 05 are LocConfigArea and are the only two the robot's 54018
// alarm does not name; the other nine are ReflectorArea and are named
// verbatim in the alarm text. That correspondence is what turned a strong
// inference into an identity, and it rests entirely on this field.
func TestParse_AreaClassesMatchTheAlarmText(t *testing.T) {
	m := load(t)
	byName := map[string]Area{}
	for _, a := range m.Areas {
		byName[a.Name] = a
	}
	// Alarm 54018 at Springfield reads:
	//   "Reflector Area Name: 01,02,03,06,07,08,09,10,11,"
	for _, n := range []string{"01", "02", "03", "06", "07", "08", "09", "10", "11"} {
		a, ok := byName[n]
		if !ok {
			t.Errorf("area %s missing from the map", n)
			continue
		}
		if a.Class != ClassReflectorArea {
			t.Errorf("area %s is %s, want %s — the alarm names it as a reflector area",
				n, a.Class, ClassReflectorArea)
		}
	}
	for _, n := range []string{"04", "05"} {
		if a := byName[n]; a.Class != ClassLocConfigArea {
			t.Errorf("area %s is %s, want %s — the alarm does NOT name it",
				n, a.Class, ClassLocConfigArea)
		}
	}
}

// THE TRAP. rssiPosList is the scan cloud, not the reflector list.
//
// The Robokit protocol PDF places its "Reflector point array" comment
// ambiguously between rssi_pos_list and reflector_pos_list. Reading the wrong
// one reports 3,067 reflectors in a plant that has 71 — and would make nine
// empty polygons look fully equipped, which is the exact opposite of the
// finding.
func TestParse_ScanCloudIsNotTheReflectorList(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash("testdata/spramrmap-trimmed.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	// The fixture keeps three rssiPosList entries precisely so this can be
	// asserted rather than assumed.
	if !contains(raw, []byte("rssiPosList")) {
		t.Fatal("fixture no longer carries rssiPosList — the trap it exists to " +
			"guard is untested")
	}
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Reflectors) != 71 {
		t.Errorf("reflectors = %d, want 71 — anything larger means rssiPosList "+
			"or normalPosList leaked into the reflector list", len(m.Reflectors))
	}
	// Every real reflector is typed. The scan cloud's points are bare {x,y}.
	for _, r := range m.Reflectors {
		if r.Kind != "plane" && r.Kind != "cylinder" {
			t.Errorf("reflector %d has kind %q — an untyped point reached the "+
				"list, which is what a scan-cloud entry looks like", r.Index, r.Kind)
		}
	}
}

// An absent width stays absent.
//
// Three of Springfield's seventy-one cylinders carry no width. Coalescing
// that to 0.0 would claim a zero-width reflector — a measurement nobody made
// — and would make a later real measurement look like a change.
func TestParse_AbsentWidthIsNotZero(t *testing.T) {
	m := load(t)
	var absent, zero int
	for _, r := range m.Reflectors {
		switch {
		case r.Width == nil:
			absent++
		case *r.Width == 0:
			zero++
		}
	}
	if absent != 3 {
		t.Errorf("reflectors with no width = %d, want 3", absent)
	}
	if zero != 0 {
		t.Errorf("%d reflectors carry width 0.0 — absence was coalesced into a "+
			"measurement", zero)
	}
}

// The polygon is not closed on the wire, and the point-in-polygon must close
// it itself.
//
// Springfield's areas are axis-aligned rectangles with four vertices. A ray
// cast that iterates edges without wrapping the last vertex to the first
// drops one whole wall — on a rectangle that is a quarter of the boundary,
// and points leak out along it.
func TestPointInPolygon_ClosesTheRingItself(t *testing.T) {
	m := load(t)
	var area08 Area
	for _, a := range m.Areas {
		if a.Name == "08" {
			area08 = a
		}
	}
	if len(area08.Polygon) != 4 {
		t.Fatalf("area 08 has %d vertices, want the real 4", len(area08.Polygon))
	}
	// The last vertex must not repeat the first, or the fixture is not
	// exercising the case.
	first, last := area08.Polygon[0], area08.Polygon[len(area08.Polygon)-1]
	if first == last {
		t.Fatal("fixture: the polygon arrived closed, so the wrap is untested")
	}

	minX, minY, maxX, maxY := Bounds(area08.Polygon)
	centre := Point{X: (minX + maxX) / 2, Y: (minY + maxY) / 2}
	if !PointInPolygon(centre, area08.Polygon) {
		t.Error("the centre of area 08 must be inside it")
	}
	// A point just outside each of the four walls. The wall between the last
	// and first vertices is the one an unwrapped loop leaks through.
	for name, p := range map[string]Point{
		"left":  {X: minX - 0.5, Y: centre.Y},
		"right": {X: maxX + 0.5, Y: centre.Y},
		"below": {X: centre.X, Y: minY - 0.5},
		"above": {X: centre.X, Y: maxY + 0.5},
	} {
		if PointInPolygon(p, area08.Polygon) {
			t.Errorf("a point %s area 08 reads as inside it", name)
		}
	}
	// The documented footprint, so a coordinate-frame slip is loud.
	if math.Abs(minX-(-1.279)) > 1e-6 || math.Abs(maxY-2.601) > 1e-6 {
		t.Errorf("area 08 bbox = x[%.3f..%.3f] y[%.3f..%.3f], want x[-1.279..-0.284] y[0.363..2.601]",
			minX, maxX, minY, maxY)
	}
}

// The robot says "8"; the map says "08".
//
// The join between rbk_report.area_ids and an area's instanceName is on this,
// and mismatched forms return NO ROWS rather than an error — a quiet zero
// exactly where the finding should be.
func TestNormalizeAreaID(t *testing.T) {
	for in, want := range map[string]string{
		"8":   "08",
		"08":  "08",
		"11":  "11",
		"0":   "00",
		" 8 ": "08",
		"":    "",
		// Not numeric: passed through rather than mangled. Nothing documents
		// that area names must be numbers.
		"dock": "dock",
	} {
		if got := NormalizeAreaID(in); got != want {
			t.Errorf("NormalizeAreaID(%q) = %q, want %q", in, got, want)
		}
	}
	// The property that matters, stated directly.
	m := load(t)
	var found bool
	for _, a := range m.Areas {
		if a.Name == NormalizeAreaID("8") {
			found = true
		}
	}
	if !found {
		t.Error(`no area matches NormalizeAreaID("8") — the wire form does not ` +
			`join to the stored form, which is the trap this function exists for`)
	}
}

// A payload that is not a map must fail loudly.
//
// #4011 through the RDS proxy returns an error body on the same 200, so a
// parser that shrugs at missing keys would store an empty map version and
// report a plant that lost all its reflectors overnight.
func TestParse_RejectsANonMapPayload(t *testing.T) {
	for name, body := range map[string]string{
		"error envelope": `{"ret_code":1,"err_msg":"no such map"}`,
		"empty object":   `{}`,
		"not json":       `<html>gateway timeout</html>`,
	} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("%s: parsed without error — an unrecognisable payload must "+
				"not become an empty map version", name)
		}
	}
}

func contains(haystack, needle []byte) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n []byte) int {
	for i := 0; i+len(n) <= len(h); i++ {
		match := true
		for j := range n {
			if h[i+j] != n[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
