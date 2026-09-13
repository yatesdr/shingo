// scene-geom.js — the drawing substrate under a scene view: the world→screen
// projection, the cubic-Bezier arithmetic that makes a drawn aisle the DRIVEN
// aisle, the orientation rule, and the key that decides what counts as one
// physical lane.
//
// Pure geometry. Numbers in, numbers or an SVG path string out — no DOM, no
// fetch, no module state. Extracted from Core's pages/dashboard-map.js, which
// still owns viewport easing, pan/zoom, comets, the minimap and the SSE feeds;
// none of that is here, because a read-only scene view needs none of it.
//
// SHARED, under docs/shared-layer-promotion.md. Core's map and the Edge's
// station cell picture project the same plant, and the desktop renderer that
// follows the station picture draws the same press from the same rows. A
// disagreement reaches the floor: an operator and an engineer looking at the
// same cell from two surfaces would see two different presses, and a route
// weighed on one surface would not be the route drawn on the other. Both
// sides compute the projection and the coordinate predicate today; the cubic
// arithmetic travels with them because the file is one unit and a seam down
// the middle of it is the seam this promotion removes. Dependencies: none.
// Lockstep: a change to any of these is a change both surfaces want at once.
//
// THE AGREEMENT TEST is scene-geom.vectors.json beside this file: the output
// of the Core copy this was promoted from, recorded at the promoting commit
// over real Springfield segments and the synthetic cases that matter, and
// re-checked by scene-geom.test.js on every run. A change here that moves a
// vector is a behaviour change to both surfaces and has to be made on
// purpose — the same posture as shared/loadervectors.
//
// NO MODULE STATE, and the projection is where that rule earns its keep. The
// orientation flag used to be a `var rotate90` living beside the code that
// framed the kiosk's view. A module-level flag shared by two pages is one
// orientation shared by two pages: whichever page computed a frame last would
// silently reorient the other. So orientation is a PARAMETER — makeProjector()
// hands back a configured projector and the caller keeps the decision.

// makeProjector returns one page's world→screen projection.
//
// Y is negated (world Y is up, screen Y is down) so the plant isn't drawn
// upside-down. rotate90 additionally turns that base image 90° CW so a plant
// whose long axis runs across the world Y axis fills a landscape monitor
// instead of letterboxing into a thin central strip.
//
// THE ROTATED BRANCH IS THE LIVE ONE, which is why it is worth stating twice.
// Springfield's drivable network measures 55.5 m across (x −52.6…2.85) by
// 83.2 m deep (y −22.38…60.85) — taller than wide, so the kiosk on that floor
// runs [y, x], not [x, −y]. A fixture that happens to be landscape exercises
// only the other branch.
//
// The caller owns the decision and it is one line:
//   rotate90 = (maxWorldY - minWorldY) > (maxWorldX - minWorldX)
// measured over the FULL plant, never over a zoomed-in region — an ROI that
// happens to be tall must not flip the whole map under the operator.
export function makeProjector(rotate90) {
    if (rotate90) return function (x, y) { return [y, x]; }; // 90° CW of the (x, −y) base image
    return function (x, y) { return [x, -y]; };
}

// rotate90For is the orientation decision itself: a plant taller than it is
// wide is turned so its long axis runs along screen X.
//
// MEASURED OVER THE FULL PLANT, NEVER OVER A SUBGRAPH. The bounds handed in
// are the whole scene's — every point, every segment endpoint — and the
// caller passes them once. A zoomed-in region that happens to be tall, or a
// single cell's six positions, must not flip the map under the operator; the
// cell picture on the station is drawn unrotated for exactly that reason
// (both plants are portrait, and their press rows run along world X).
export function rotate90For(minX, maxX, minY, maxY) {
    return (maxY - minY) > (maxX - minX);
}

// Squared distance — squared because every caller either compares it against
// another distance or against a squared threshold, and the square root is the
// expensive half.
export function dist2(p, q) { var dx = p.x - q.x, dy = p.y - q.y; return dx * dx + dy * dy; }

// isCoord is isFinite plus a type check, and the type check is the point:
// isFinite(null) is TRUE because null coerces to 0, and 0 is a real coordinate
// on a plant map. A JSON null on one handle would otherwise become a control
// point at the origin and sweep the lane across the floor — the same failure
// the all-zero sentinel exists to prevent, arriving one layer later.
export function isCoord(v) { return typeof v === 'number' && isFinite(v); }

// CUBIC_SAMPLES is the polyline resolution used to measure a curved segment.
// 24 is well past the point where more samples change a Springfield lane
// length in the third decimal, and this runs once per edge per graph rebuild,
// not per frame.
export var CUBIC_SAMPLES = 24;

// cubicPoint evaluates the cubic Bezier p0 → (c[0],c[1]) → (c[2],c[3]) → p3
// at t. Endpoints are {x, y}; the handle pair is the flat 4-array the scene
// edge carries.
export function cubicPoint(p0, c, p3, t) {
    var mt = 1 - t, a = mt * mt * mt, b = 3 * mt * mt * t, d = 3 * mt * t * t, e = t * t * t;
    return {
        x: a * p0.x + b * c[0] + d * c[2] + e * p3.x,
        y: a * p0.y + b * c[1] + d * c[3] + e * p3.y
    };
}

// cubicLength is the driven distance along a curved segment. The chord is what
// the travel graph used to weigh, and on Springfield's bowed lanes it is up to
// 24% short — which makes the shortest path prefer a route the robot does not
// actually find shorter.
export function cubicLength(p0, c, p3) {
    var total = 0, prev = p0;
    for (var i = 1; i <= CUBIC_SAMPLES; i++) {
        var pt = cubicPoint(p0, c, p3, i / CUBIC_SAMPLES);
        total += Math.sqrt(dist2(prev, pt));
        prev = pt;
    }
    return total;
}

// cubicPathD spells one cubic segment as an SVG path `d`.
//
// SCREEN SPACE, all four points, each [x, y] — project the endpoints AND the
// handles, then build. A Bezier is affine-invariant, so the projected handles
// describe the projected curve exactly; there is no re-fitting to do, and a
// caller that projects the endpoints but not the handles gets a curve bending
// toward a point on the wrong side of the plant.
export function cubicPathD(p0, c1, c2, p3) {
    return 'M' + p0[0] + ' ' + p0[1] + 'C' + c1[0] + ' ' + c1[1] +
        ' ' + c2[0] + ' ' + c2[1] + ' ' + p3[0] + ' ' + p3[1];
}

// laneKey collapses the two directed entries of one physical lane onto one key.
//
// The travel graph is stored directed but driven both ways, so it holds a
// reciprocal entry for most segments and a straight walk of the adjacency
// lists visits those lanes twice: Springfield's 405 directed edges are 212
// physical lanes. Drawing both entries double-strokes every aisle, and any
// per-lane tally built the same way double-counts the network.
//
// The a<b ordering is not arbitrary — it is what makes the surviving entry the
// one whose handles were stored in the a→b direction, so the control points
// project across in the order the path string wants them.
export function laneKey(a, b) {
    return a < b ? a + '_' + b : b + '_' + a;
}
