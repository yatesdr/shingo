package scenemap

import (
	"fmt"
	"math"
	"sort"
)

// Lane fingerprinting — the version a per-lane confidence series is keyed to.
//
// WHY THIS LIVES BESIDE THE .smap PARSER AND NOT IN IT. The input here comes
// from scene_edges, which Core syncs from RDS's /scene — a different artifact
// on a different transport from the .smap this package parses. They are kept
// in one package because they must share ONE hashing discipline: one digest,
// one cosmetic allow-list, one rule about negative zero. Two implementations
// of "has this object changed" is how two halves of a plant's history end up
// answering the question differently.
//
// A LANE IS TWO ROWS. scene_edges stores every drivable lane twice, once per
// direction, and RoboShop lets each direction be drawn separately — so the
// fingerprint is computed over BOTH twins, order-independently, rather than
// over whichever one the query returned first. Recording only one twin's
// geometry would make the version row a coin flip on iteration order.
//
// Measured across Springfield's 193 reciprocal pairs on 2026-08-06: every
// pair mirrors to 0.000000000000 m including its Bezier handles, none differs
// in className, and none pairs a straight with a curve. So lane-grain is safe
// — TODAY. TwinsAgree is what makes the day that stops being true a visible
// event instead of a silent coin toss.
//
// THE BEHAVIOUR HALF OF THE HASH HAS NO INPUT, AND THAT IS A PROPERTY OF THE
// TRANSPORT RATHER THAN A SHORTCUT. Measured against the live SPR scene:
// RDS's /scene carries exactly ONE property on all 405 curves — bindRobotMap,
// its own binding key — while the robot's .smap carries fourteen for the same
// curves, including maxspeed on 91 of them, obstacle distances on 88, and
// laser on one. RDS strips them.
//
// So a lane's DefHash covers shape and class and nothing else, and a tech who
// halves a lane's maxspeed changes nothing this fingerprint can see. The fix
// is not available on this transport. The option, recorded rather than taken:
// parse the .smap's advancedCurveList for its property[] ONLY — never its
// geometry, which would give the plant two queryable copies of one network —
// and feed that into the lane's DefHash. That crosses the two-transport
// boundary deliberately kept elsewhere in this design, so it is a decision to
// make out loud rather than a gap to quietly fill.

// LaneEdge is one DIRECTED row of scene_edges.
type LaneEdge struct {
	Instance string
	Class    string
	FromName string
	ToName   string
	From     Point
	To       Point
	// Ctrl1/Ctrl2 are the cubic handles in the From→To direction, nil
	// together on a lane the fleet drives straight.
	Ctrl1 *Point
	Ctrl2 *Point
}

// Curved reports whether the edge carries a complete handle pair.
func (e LaneEdge) Curved() bool { return e.Ctrl1 != nil && e.Ctrl2 != nil }

// LaneKey is the undirected identity: the sorted endpoint pair. Empty when
// either endpoint is unnamed — such an edge has no lane and can have no
// version row. See store/robotconfidence.Segment.Keyable for the other half
// of that rule.
func LaneKey(fromName, toName string) string {
	if fromName == "" || toName == "" {
		return ""
	}
	if toName < fromName {
		fromName, toName = toName, fromName
	}
	return fromName + "-" + toName
}

// LaneVersion is one physical lane's fingerprint at a point in time.
type LaneVersion struct {
	Fingerprint
	// TwinsAgree is false when the two directed rows of a two-way lane do not
	// mirror each other — different class, one curved and one not, or
	// geometry that does not reflect. It has never been false at Springfield.
	//
	// When it IS false the lane's two directions are genuinely different
	// pieces of geometry and a single lane-grain version is describing only
	// part of the truth. The row is still written, because refusing to
	// version the lane would lose it entirely; the flag is what makes the
	// day someone draws the directions separately a thing a query can find.
	TwinsAgree bool
	// Disagreement says how they differ, for the log line. Empty when they
	// agree or when the lane is one-way.
	Disagreement string
	// Directed is how many rows scene_edges holds for this lane: 2 for a
	// reciprocal pair, 1 for a genuinely one-way lane. Springfield has 193
	// and 19 of them.
	Directed int
}

// FingerprintLane hashes one physical lane from its directed rows.
//
// Order-independent: the edges are sorted by instance name before hashing, so
// the version does not depend on which direction the query returned first.
func FingerprintLane(edges []LaneEdge) (LaneVersion, error) {
	if len(edges) == 0 {
		return LaneVersion{}, fmt.Errorf("scenemap: a lane needs at least one directed edge")
	}
	key := LaneKey(edges[0].FromName, edges[0].ToName)
	if key == "" {
		return LaneVersion{}, fmt.Errorf("scenemap: edge %q has an unnamed endpoint and no lane key",
			edges[0].Instance)
	}
	for _, e := range edges {
		if LaneKey(e.FromName, e.ToName) != key {
			return LaneVersion{}, fmt.Errorf(
				"scenemap: edge %q does not belong to lane %q", e.Instance, key)
		}
	}
	if len(edges) > 2 {
		return LaneVersion{}, fmt.Errorf(
			"scenemap: lane %q has %d directed rows; a lane has at most two", key, len(edges))
	}

	ordered := append([]LaneEdge(nil), edges...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Instance < ordered[j].Instance })

	v := LaneVersion{TwinsAgree: true, Directed: len(ordered)}
	if len(ordered) == 2 {
		v.TwinsAgree, v.Disagreement = twinsMirror(ordered[0], ordered[1])
	}

	shape := newDigest()
	shape.str("lane")
	shape.str(key)
	for _, e := range ordered {
		shape.str(e.Instance)
		// The class is shape: re-declaring a lane's kind changes what its
		// geometry means even with nothing moved.
		shape.str(e.Class)
		shape.float(e.From.X)
		shape.float(e.From.Y)
		shape.float(e.To.X)
		shape.float(e.To.Y)
		if e.Curved() {
			shape.float(e.Ctrl1.X)
			shape.float(e.Ctrl1.Y)
			shape.float(e.Ctrl2.X)
			shape.float(e.Ctrl2.Y)
		} else {
			// Distinct from a handle pair that happens to sit at the origin.
			// The all-zero pair is SEER's "no handles" sentinel and the
			// origin is a real coordinate on this map, so the two must not
			// hash alike.
			shape.str("handles:absent")
		}
	}
	h := shape.sum()
	// DefHash equals ShapeHash for a lane today: RDS publishes no behavioural
	// property to put in it. Kept as its own field rather than collapsed, so
	// that adding the .smap property source later is a value change and not a
	// type change.
	v.Fingerprint = Fingerprint{ShapeHash: h, DefHash: h}
	return v, nil
}

// twinsMirror reports whether two directed rows describe one piece of floor.
func twinsMirror(a, b LaneEdge) (bool, string) {
	if a.Class != b.Class {
		return false, fmt.Sprintf("%s is %s and %s is %s", a.Instance, a.Class, b.Instance, b.Class)
	}
	if a.Curved() != b.Curved() {
		return false, fmt.Sprintf("%s is %s and %s is %s",
			a.Instance, curvedWord(a.Curved()), b.Instance, curvedWord(b.Curved()))
	}
	const tol = 1e-9
	if !near(a.From, b.To, tol) || !near(a.To, b.From, tol) {
		return false, fmt.Sprintf("%s and %s do not share endpoints", a.Instance, b.Instance)
	}
	if a.Curved() {
		// The reverse direction swaps the handle pair: a's first handle is
		// b's second. A pair that does not swap is a lane drawn twice rather
		// than a lane drawn once and read both ways.
		if !near(*a.Ctrl1, *b.Ctrl2, tol) || !near(*a.Ctrl2, *b.Ctrl1, tol) {
			return false, fmt.Sprintf("%s and %s bow differently", a.Instance, b.Instance)
		}
	}
	return true, ""
}

func curvedWord(c bool) string {
	if c {
		return "curved"
	}
	return "straight"
}

func near(a, b Point, tol float64) bool {
	return math.Abs(a.X-b.X) <= tol && math.Abs(a.Y-b.Y) <= tol
}

// LaneShape returns the lane's vertices in canonical order, for measuring how
// far it moved between two versions.
//
// Canonical means the direction whose FromName sorts first, so two versions
// of the same lane are compared vertex-to-vertex even if scene_edges returned
// the twins in a different order on the two syncs. Without that, a re-sync
// that merely reordered rows would report the lane as having moved the length
// of itself.
func LaneShape(edges []LaneEdge) []Point {
	if len(edges) == 0 {
		return nil
	}
	pick := edges[0]
	for _, e := range edges {
		if e.FromName < pick.FromName {
			pick = e
		}
	}
	pts := []Point{pick.From}
	if pick.Curved() {
		pts = append(pts, *pick.Ctrl1, *pick.Ctrl2)
	}
	return append(pts, pick.To)
}
