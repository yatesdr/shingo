// Package scenemap parses a SEER robot's own map file (.smap) into the two
// things RDS does not expose: the declared areas and the reflector positions.
//
// WHY THIS EXISTS AT ALL. RDS's /scene returns advancedAreaList: [] — it
// drops the areas — and has never exposed reflectors under any endpoint. So
// Core holds the travel network and nothing whatsoever about why localization
// works or fails on it. Every area and reflector fact in this project's
// findings came from a hand-pull off one robot.
//
// That gap hid the single most actionable finding the work produced: nine
// polygons declaring reflector localization, containing zero reflectors
// between them, with 849 of 850 no-estimate readings falling inside them.
// None of it needed telemetry. It was visible in this file alone.
//
// WHAT THIS PACKAGE DELIBERATELY DOES NOT PARSE. The .smap also carries the
// drivable network — advancedCurveList, 405 entries at Springfield, the same
// 405 rows Core already holds in scene_edges from RDS, agreeing to 0.000000 m
// on all 182 shared points. Parsing it here would give the plant two
// queryable copies of one network on two transports with nothing saying which
// wins. The curves stay in the archived blob, unparsed, and scene_edges
// remains the one answer to "where is lane X".
package scenemap

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ClassReflectorArea and ClassLocConfigArea are the two area kinds seen at
// Springfield. The class is the strongest predictor in the dataset: every
// ReflectorArea carrying traffic loses 23-71% of its readings and neither
// LocConfigArea loses any. Compare on this, never on the instance name.
const (
	ClassReflectorArea = "ReflectorArea"
	ClassLocConfigArea = "LocConfigArea"
)

// Map is the parsed subset of a .smap worth keeping in a database.
type Map struct {
	Name       string
	Resolution float64
	Areas      []Area
	Reflectors []Reflector
}

// Area is one declared region on the robot's map.
type Area struct {
	// Name is the map's instanceName, ZERO-PADDED as stored ("08").
	//
	// THE ROBOT REPORTS IT UNPADDED. rbk_report.area_ids carries "8" for the
	// same area, so a join on the literal strings silently returns nothing —
	// no error, no row, just a quiet zero where the finding should be. Use
	// NormalizeAreaID on anything arriving from the wire before comparing.
	Name  string
	Class string
	// Polygon is the boundary, in order, and NOT CLOSED — the last vertex
	// does not repeat the first. A point-in-polygon that assumes closure
	// drops the final edge and leaks along one side.
	Polygon []Point
	// ColorPen and ColorBrush are ARGB integers from the vendor's editor.
	// Kept for provenance and excluded from every hash: they are the
	// editor's defaults per class, not a decision anyone made about this
	// area. See Fingerprint.
	ColorPen   int64
	ColorBrush int64
	// Properties are the typed key/values the editor writes. UseAutoReloc
	// lives here and is localization-relevant; TextFontSize lives here and
	// is not.
	Properties map[string]any
}

// Reflector is one reflector position.
type Reflector struct {
	Index int
	Kind  string // plane | cylinder
	// Width is metres. ABSENT on some cylinders — three of Springfield's 71
	// carry no width at all — so this is a pointer: 0.0 would claim a
	// zero-width reflector, which is a measurement nobody made.
	Width *float64
	X     float64
	Y     float64
}

// Point is a map coordinate in metres, Y up.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// ── the wire shape ─────────────────────────────────────────────────────────

type rawMap struct {
	Header struct {
		MapName    string  `json:"mapName"`
		Resolution float64 `json:"resolution"`
	} `json:"header"`
	Areas []struct {
		ClassName    string  `json:"className"`
		InstanceName string  `json:"instanceName"`
		PosGroup     []Point `json:"posGroup"`
		Attribute    struct {
			ColorPen   int64 `json:"colorPen"`
			ColorBrush int64 `json:"colorBrush"`
		} `json:"attribute"`
		Property []rawProperty `json:"property"`
	} `json:"advancedAreaList"`
	// Reflectors is reflectorPosList and ONLY reflectorPosList.
	//
	// THE TRAP: rssiPosList sits beside it with 3,067 entries of bare {x, y}
	// spanning well outside the drivable network — it is the laser scan
	// cloud. The Robokit protocol PDF places the comment "Reflector point
	// array" ambiguously between the two fields. Counting the wrong one
	// reports 3,067 reflectors in a plant that has 71, which would make the
	// nine empty polygons look fully equipped.
	Reflectors []struct {
		Type  string   `json:"type"`
		Width *float64 `json:"width"`
		X     float64  `json:"x"`
		Y     float64  `json:"y"`
	} `json:"reflectorPosList"`
}

type rawProperty struct {
	Key         string   `json:"key"`
	Type        string   `json:"type"`
	BoolValue   *bool    `json:"boolValue"`
	Int32Value  *int64   `json:"int32Value"`
	DoubleValue *float64 `json:"doubleValue"`
	StringValue *string  `json:"stringValue"`
}

// value returns the typed field, ignoring the base64 `value` twin.
//
// Every property carries BOTH a typed field and `value`, which is base64 of
// the ASCII rendering — "MA==" for 0. The typed field is the usable one; the
// base64 exists for the editor.
func (p rawProperty) value() any {
	switch {
	case p.BoolValue != nil:
		return *p.BoolValue
	case p.Int32Value != nil:
		return *p.Int32Value
	case p.DoubleValue != nil:
		return *p.DoubleValue
	case p.StringValue != nil:
		return *p.StringValue
	}
	return nil
}

// Parse reads a .smap payload.
//
// The map arrives as the response body of Robokit #4011 verbatim — it is the
// map object itself, not wrapped in an envelope.
func Parse(payload []byte) (*Map, error) {
	var raw rawMap
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("scenemap: parse: %w", err)
	}
	if raw.Header.MapName == "" {
		return nil, fmt.Errorf("scenemap: payload has no header.mapName — " +
			"this is not a .smap, or the download returned an error body")
	}
	m := &Map{Name: raw.Header.MapName, Resolution: raw.Header.Resolution}

	for _, a := range raw.Areas {
		area := Area{
			Name:       a.InstanceName,
			Class:      a.ClassName,
			Polygon:    a.PosGroup,
			ColorPen:   a.Attribute.ColorPen,
			ColorBrush: a.Attribute.ColorBrush,
		}
		if len(a.Property) > 0 {
			area.Properties = make(map[string]any, len(a.Property))
			for _, p := range a.Property {
				if v := p.value(); v != nil {
					area.Properties[p.Key] = v
				}
			}
		}
		m.Areas = append(m.Areas, area)
	}
	for i, r := range raw.Reflectors {
		m.Reflectors = append(m.Reflectors, Reflector{
			Index: i, Kind: r.Type, Width: r.Width, X: r.X, Y: r.Y,
		})
	}
	return m, nil
}

// NormalizeAreaID makes a robot-reported area id comparable to a map-stored
// one.
//
// The robot reports "8"; the map stores "08". Both are correct in their own
// world and neither side is going to change, so the join has to normalise.
// Zero-padding to the map's width rather than stripping, because the map's
// form is what lands in the database and a query written against the table
// should match what it sees there.
//
// Only pads a purely numeric id — an id that is not a number is passed
// through untouched rather than mangled, since nothing documents that area
// names must be numeric.
func NormalizeAreaID(id string) string {
	s := strings.TrimSpace(id)
	if s == "" {
		return ""
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return s
	}
	return fmt.Sprintf("%02d", n)
}

// ReflectorsInside counts the reflectors that fall within an area's polygon.
//
// THE NUMBER IS PROVENANCE, NOT A PREDICTOR, and the distinction is
// load-bearing enough to state here rather than in a design note. It is worth
// storing — it is one integer, it is the input to any future sensor-coverage
// work, and "this declared reflector zone contains zero reflectors" is the
// most actionable sentence this project has produced. It must NOT drive a
// mark, a badge or a band: measured, the reflector count inside a zone has no
// predictive power over the no-estimate rate and the sign runs backwards.
// What predicts is the CLASS.
func (m *Map) ReflectorsInside(a Area) int {
	n := 0
	for _, r := range m.Reflectors {
		if PointInPolygon(Point{X: r.X, Y: r.Y}, a.Polygon) {
			n++
		}
	}
	return n
}

// PointInPolygon reports whether p lies inside the polygon, by ray casting.
//
// The polygon is NOT closed on the wire, so the loop wraps the last vertex to
// the first itself. Forgetting that drops one edge and leaks along a side —
// which on Springfield's axis-aligned rectangles means an entire wall.
func PointInPolygon(p Point, poly []Point) bool {
	if len(poly) < 3 {
		return false
	}
	inside := false
	for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
		pi, pj := poly[i], poly[j]
		if (pi.Y > p.Y) == (pj.Y > p.Y) {
			continue
		}
		x := (pj.X-pi.X)*(p.Y-pi.Y)/(pj.Y-pi.Y) + pi.X
		if p.X < x {
			inside = !inside
		}
	}
	return inside
}

// Bounds returns the polygon's axis-aligned bounding box.
func Bounds(poly []Point) (minX, minY, maxX, maxY float64) {
	if len(poly) == 0 {
		return 0, 0, 0, 0
	}
	minX, minY = math.Inf(1), math.Inf(1)
	maxX, maxY = math.Inf(-1), math.Inf(-1)
	for _, p := range poly {
		minX, minY = math.Min(minX, p.X), math.Min(minY, p.Y)
		maxX, maxY = math.Max(maxX, p.X), math.Max(maxY, p.Y)
	}
	return minX, minY, maxX, maxY
}

// SortedAreaNames returns the map's area names in a stable order.
func (m *Map) SortedAreaNames() []string {
	out := make([]string, 0, len(m.Areas))
	for _, a := range m.Areas {
		out = append(out, a.Name)
	}
	sort.Strings(out)
	return out
}
