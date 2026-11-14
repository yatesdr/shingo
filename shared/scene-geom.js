// scene-geom.js — the drawing substrate under a scene view: the world→screen
// projection, the cubic-Bezier arithmetic that makes a drawn aisle the DRIVEN
// aisle, the orientation rule, and the key that decides what counts as one
// physical lane.
//
// Pure geometry. Numbers in, numbers or an SVG path string out — no DOM, no
// fetch, no module state. Extracted from Core's pages/dashboard-map.js. The
// shared scene modules are built on it: scene-net.js (a scene's network and
// the route model), scene-path.js (a robot's path drawn along the rows),
// scene-view.js (the retained drawing) and comet-layer.js (the comets). The
// robot map keeps its viewport easing, pan/zoom, minimap and SSE feeds.
//
// SHARED, under docs/shared-layer-promotion.md. Core's robot map and its
// localization board and the Edge's desktop process map project the same
// plant from the same rows. A disagreement reaches the floor: an operator and
// an engineer looking at the same aisle from two surfaces would see two
// different floors. (The Edge's station cell picture, operator-flow.js,
// imports none of this: it draws a schematic from point coordinates.) Routes
// are not this file's: the route model is scene-net.js and a driving robot's
// route is RDS's path drawn by scene-path.js, and both weigh or draw a curved
// row with the cubic arithmetic here, so a route lies on the aisle drawn under
// it. The cubic arithmetic travels with the projection because the file is
// one unit and a seam down the middle of it is the seam this promotion
// removes. Dependencies: none. Lockstep: a change to any of these is a change
// every surface wants at once.
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

// turnedProjector is makeProjector for a chosen quarter turn clockwise of the
// (x, −y) base image: 0, 90, 180 or 270. Every turn is a rotation and none a
// mirror, so a map turned on a wall still reads as the floor it shows.
export function turnedProjector(deg) {
    if (deg === 180) return function (x, y) { return [-x, y]; };
    if (deg === 270) return function (x, y) { return [-y, -x]; };
    return makeProjector(deg === 90);
}

// mapTurn is the wall map's orientation: the quarter turn the display was set to,
// or with none set, the same decision rotate90For makes. The bounds are the full
// plant's, for the reason rotate90For gives.
export function mapTurn(chosen, minX, maxX, minY, maxY) {
    if (chosen === 0 || chosen === 90 || chosen === 180 || chosen === 270) return chosen;
    return rotate90For(minX, maxX, minY, maxY) ? 90 : 0;
}

// headingDeg is a world heading (radians) as an SVG rotate on a turned map.
export function headingDeg(angle, turn) {
    return -(angle * 180 / Math.PI) + turn;
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

// laneKey collapses the directed rows of one physical lane onto one key.
//
// A scene stores one row per direction a robot may drive a lane: most lanes
// both ways, so two rows, and some one way only (a robot may not drive those
// back). A walk of the rows straight through visits a two-way lane twice;
// drawing both double-strokes the aisle, and any per-lane tally built the same
// way double-counts the network.
//
// The key is symmetric — laneKey(a, b) === laneKey(b, a) — so it says only
// which rows are one lane. Which row of a lane survives a dedup is whichever
// the caller reaches first, and the caller turns that row's handles to run the
// way it draws the lane (scene-net.js laneList).
export function laneKey(a, b) {
    return a < b ? a + '_' + b : b + '_' + a;
}
