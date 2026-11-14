// Unit tests for the robot-map dashboard's CURVED-SEGMENT rendering — the
// rule that makes the drawn network the driven network. Run under plain Node
// via the Go wrapper dashboard_map_curve_test.go. Exit 0 on pass, 1 on any
// assertion failure.
//
// The bug these exist to prevent: shingo knew a lane was a BezierPath or a
// DegenerateBezier and drew the CHORD anyway, because SEER's two control
// handles were dropped at JSON decode three layers upstream. At Springfield
// that paints a lane up to 1.30 m from the one the robot drives (LM10-LM113,
// off a 7.17 m chord), so the fleet visibly leaves the network as painted, and
// the Dijkstra weight built on the same chord runs up to 24% short of the
// driven distance.
//
// Three properties carry the fix:
//   1. The decision is the HANDLES, not the className. 264 of Springfield's
//      377 segments say "DegenerateBezier"; 171 of those are straight lines
//      spelled as cubics, 93 bow past a centimetre and 58 past 10 cm — while
//      the only two segments actually named "BezierPath" bow 0.17 m. Reading
//      the name fixes the two that barely matter and misses the rest.
//   2. A curved segment is WEIGHED along its arc, not across its chord. The
//      weight is what routes robots, so a chord weight makes the map prefer a
//      path the robot does not find shorter.
//   3. All four handle coordinates or none. Three describe no cubic, and the
//      renderer would have to invent the fourth.
//
// The Springfield coordinates below are verbatim from the live scene.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

// The page code under test. This file lives in www/testdata, outside the
// embedded static tree, so no binary carries or serves it
// (TestStaticTree_HoldsNoTestJS); the code it reads stays in www/static.
const SRC_DIR = path.join(__dirname, '..', '..', '..', 'static', 'pages');

let failures = 0;
function check(name, cond, detail) {
    if (cond) {
        console.log('  ok  ' + name);
    } else {
        failures++;
        console.log('  FAIL ' + name + (detail ? ' — ' + detail : ''));
    }
}

// --- harness -------------------------------------------------------------
// Same injection technique as dashboard-map.stall.test.js: dashboard-map.js is
// an IIFE, so one __export call goes in just before it closes rather than
// stripping the wrapper and changing scoping for every `var` in the file.
// tnodes/tadj/sceneEdges/points are REASSIGNED inside the module, and so is
// `proj` (computeView reconfigures it for the plant's orientation), so all of
// them are handed out through accessors — an exported reference would go stale
// the first time buildGraph ran and every assertion after that would read the
// old graph, or the old projection.
//
// The geometry and the drawing are shared/'s now (scene-geom.js, scene-net.js,
// scene-view.js). vm runs a SCRIPT, not a module, so shared-modules.js loads
// each shared file once into the page's context, as a browser's module map
// does, and binds the page's imports to them: the page and the assertions
// below hold the very same functions.
const SM = require(path.join(SRC_DIR, '..', '..', '..', '..', 'shared', 'testdata', 'js', 'shared-modules.js'));

function load() {
    const ctx = {
        console: console,
        Date: Date,
        Math: Math,
        Object: Object,
        Array: Array,
        JSON: JSON,
        isFinite: isFinite,
        document: {
            readyState: 'loading',
            body: { getAttribute() { return '1'; } },
            documentElement: {},
            getElementById() { return null; },
            querySelector() { return null; },
            querySelectorAll() { return []; },
            createElementNS(ns, name) { return svgNode(name); },
            addEventListener() {},
        },
        window: { addEventListener() {}, matchMedia() { return { matches: false }; } },
        setInterval() { return 0; },
        clearInterval() {},
        setTimeout() { return 0; },
        clearTimeout() {},
        requestAnimationFrame() { return 0; },
        cancelAnimationFrame() {},
        fetch() { return Promise.resolve({ ok: true, json() { return Promise.resolve([]); } }); },
        onSSE() {},
        setSSEReloadOnBuild() {},
    };
    let exported = null;
    ctx.__export = function (o) { exported = o; };
    vm.createContext(ctx);
    // drawAisles is the scene view's own (scene-view.js, not exported): the
    // aisle drawing alone, handed the lanes the page draws.
    const shared = SM.realm(ctx, null, { 'scene-view.js': ['drawAisles'] });

    const file = path.join(SRC_DIR, 'dashboard-map.js');
    const raw = fs.readFileSync(file, 'utf8');
    const INJECT = '  __export({ buildGraph: buildGraph,' +
        ' lanes: function () { return sceneNet(scene).lanes; }, scene: function () { return scene; },' +
        ' computeView: computeView, routeWorld: routeWorld,' +
        ' shortestPath: shortestPath, nearestTNode: nearestTNode,' +
        ' setEdges: function (v) { sceneEdges = v; },' +
        ' setPoints: function (v) { points = v; },' +
        ' proj: function (x, y) { return proj(x, y); },' +
        ' tnodes: function () { return tnodes; }, tadj: function () { return tadj; },' +
        ' graphScale: function () { return graphScale; } });\n})();\n';
    const src = raw.replace(/\}\)\(\);\s*$/, INJECT);
    if (src === raw) {
        throw new Error('dashboard-map.js no longer ends in the "})();" IIFE close this ' +
            'harness injects before; update the injection in dashboard-map.curve.test.js');
    }
    SM.runPage(ctx, src, shared, null, 'dashboard-map.js');
    if (!exported) throw new Error('__export was not reached inside dashboard-map.js');
    exported.drawAisles = function (svg, nodeR) {
        shared('scene-view.js').drawAisles(svg, exported.lanes(), nodeR, exported.proj);
    };
    // cubicPoint/cubicLength are scene-geom's. Taken from the realm the page
    // runs in, so what the assertions measure is the very binding the page
    // weighed its graph with.
    exported.cubicPoint = shared('scene-geom.js').cubicPoint;
    exported.cubicLength = shared('scene-geom.js').cubicLength;
    exported.sceneRoute = shared('scene-net.js').sceneRoute;
    return exported;
}

// A recording stand-in for an SVG element: keeps the tag and every attribute
// so an assertion can talk about what was actually drawn.
function svgNode(name) {
    return {
        tag: name,
        attrs: {},
        children: [],
        style: {},
        setAttribute(k, v) { this.attrs[k] = v; },
        getAttribute(k) { return this.attrs[k]; },
        appendChild(c) { this.children.push(c); return c; },
        removeAttribute(k) { delete this.attrs[k]; },
    };
}

// --- Springfield fixtures ------------------------------------------------
// LM10-LM113 is the widest bow in the scene: a DegenerateBezier 1.30 m off its
// own chord. LM100-AP102 is a StraightPath — Core strips SEER's all-zero
// sentinel, so it reaches the browser with no handle fields at all.
const CURVED = {
    instance_name: 'LM10-LM113', class_name: 'DegenerateBezier',
    from_name: 'LM10', to_name: 'LM113',
    from_x: -0.254, from_y: 33.554, to_x: -6.572, to_y: 36.951,
    ctrl1_x: -0.198, ctrl1_y: 36.401, ctrl2_x: -5.065, ctrl2_y: 36.951,
};
const STRAIGHT = {
    instance_name: 'LM100-AP102', class_name: 'StraightPath',
    from_name: 'LM100', to_name: 'AP102',
    from_x: -0.544, from_y: 11.787, to_x: 0.886, to_y: 11.807,
};
// A lane SEER stores in one direction only. Springfield's 405 directed edges
// are 212 physical lanes, which is 193 reciprocal pairs and 19 of these; both
// kinds have to come out of the dedup as exactly one drawn aisle.
const ONE_WAY = {
    instance_name: 'AP102-LM10', class_name: 'StraightPath',
    from_name: 'AP102', to_name: 'LM10',
    from_x: STRAIGHT.to_x, from_y: STRAIGHT.to_y,
    to_x: CURVED.from_x, to_y: CURVED.from_y,
};

function chord(e) {
    return Math.hypot(e.to_x - e.from_x, e.to_y - e.from_y);
}

// The same physical lane as SEER's other stored direction: endpoints swapped,
// and the handle pair swapped with them (the same curve read end-to-start).
function reverseOf(e) {
    const r = {
        instance_name: e.to_name + '-' + e.from_name, class_name: e.class_name,
        from_name: e.to_name, to_name: e.from_name,
        from_x: e.to_x, from_y: e.to_y, to_x: e.from_x, to_y: e.from_y,
    };
    if (e.ctrl1_x !== undefined) {
        r.ctrl1_x = e.ctrl2_x; r.ctrl1_y = e.ctrl2_y;
        r.ctrl2_x = e.ctrl1_x; r.ctrl2_y = e.ctrl1_y;
    }
    return r;
}

function drawnFor(m, edges) {
    m.setEdges(edges);
    m.buildGraph();
    const svg = svgNode('svg');
    m.drawAisles(svg, 1);
    return svg.children;
}

// --- the weight ----------------------------------------------------------

console.log('edge weight');

(function curvedSegmentIsWeighedAlongItsArc() {
    const m = load();
    m.setEdges([CURVED]);
    m.buildGraph();
    const adj = m.tadj();
    const w = adj[0][0].w;
    const c = chord(CURVED);
    check('a bowed lane weighs MORE than its chord', w > c,
        'w=' + w.toFixed(4) + ' chord=' + c.toFixed(4));
    // A band, not a point: the excess for this segment measures 9.7% against a
    // 4096-sample reference. Asserting a range means the sample count can be
    // retuned without hollowing the check out, while still failing loudly if
    // the weight silently reverts to the chord (0%) or runs away.
    const excess = w / c - 1;
    check('the excess is the segment\'s real 9.7%, within sampling error',
        excess > 0.09 && excess < 0.105, 'excess=' + (excess * 100).toFixed(3) + '%');
})();

(function straightSegmentKeepsItsChordWeight() {
    const m = load();
    m.setEdges([STRAIGHT]);
    m.buildGraph();
    const w = m.tadj()[0][0].w;
    check('a straight aisle weighs exactly its chord',
        Math.abs(w - chord(STRAIGHT)) < 1e-9,
        'w=' + w + ' chord=' + chord(STRAIGHT));
})();

(function bothDirectionsWeighTheSame() {
    const m = load();
    m.setEdges([CURVED]);
    m.buildGraph();
    const adj = m.tadj();
    check('the reverse edge carries the same weight',
        Math.abs(adj[0][0].w - adj[1][0].w) < 1e-12,
        adj[0][0].w + ' vs ' + adj[1][0].w);
})();

// --- partial handles are not a curve -------------------------------------

console.log('handle completeness');

(function threeCoordinatesAreNotACubic() {
    const m = load();
    [
        { ctrl1_x: -0.198 },
        { ctrl1_x: -0.198, ctrl1_y: 36.401 },
        { ctrl1_x: -0.198, ctrl1_y: 36.401, ctrl2_x: -5.065 },
        { ctrl2_x: -5.065, ctrl2_y: 36.951 },
    ].forEach(function (partial, i) {
        const e = Object.assign({}, STRAIGHT, partial);
        m.setEdges([e]);
        m.buildGraph();
        check('partial handle set ' + i + ' draws no curve', m.tadj()[0][0].c === null,
            JSON.stringify(m.tadj()[0][0].c));
    });
})();

(function aNonFiniteHandleIsNoHandle() {
    const m = load();
    // null and undefined both pass through JSON as absent-ish; NaN is what a
    // bad parse leaves behind. None of them may become geometry.
    [null, undefined, NaN, 'x'].forEach(function (bad) {
        const e = Object.assign({}, CURVED, { ctrl2_y: bad });
        m.setEdges([e]);
        m.buildGraph();
        check('ctrl2_y=' + String(bad) + ' draws no curve', m.tadj()[0][0].c === null);
    });
})();

// --- what actually gets drawn --------------------------------------------

console.log('drawAisles');

(function aStraightAisleStaysALine() {
    const m = load();
    const drawn = drawnFor(m, [STRAIGHT]);
    check('one element drawn', drawn.length === 1, 'got ' + drawn.length);
    check('a straight aisle draws a <line>', drawn[0] && drawn[0].tag === 'line',
        drawn[0] && drawn[0].tag);
})();

(function aBowedAisleDrawsACubicPath() {
    const m = load();
    const drawn = drawnFor(m, [CURVED]);
    check('one element drawn (reciprocal directions dedup)', drawn.length === 1,
        'got ' + drawn.length);
    const el = drawn[0];
    check('a bowed aisle draws a <path>', el && el.tag === 'path', el && el.tag);
    check('the path is a single cubic segment', /^M[^C]+C[^C]+$/.test(el.attrs.d || ''),
        el && el.attrs.d);
    // SVG fills a <path> black by default. An unfilled aisle is the whole
    // point of the shape; a filled one blots the floor.
    check('the path is not filled', el.attrs.fill === 'none', el && el.attrs.fill);
    check('the path carries the same aisle class as a line',
        el.attrs['class'] === 'map-aisle', el && el.attrs['class']);
})();

(function theDrawnCubicIsTheSceneGeometry() {
    const m = load();
    const el = drawnFor(m, [CURVED])[0];
    // proj() is [x, -y] with no rotation, and a Bezier is affine-invariant, so
    // the drawn control points must be the scene's handles with y negated. If
    // the renderer ever reordered or reused the wrong pair, the numbers move.
    const nums = (el.attrs.d || '').split(/[MC ]+/).filter(function (s) { return s !== ''; })
        .map(Number);
    check('the path has 8 coordinates', nums.length === 8, JSON.stringify(nums));
    const want = [
        CURVED.from_x, -CURVED.from_y,
        CURVED.ctrl1_x, -CURVED.ctrl1_y,
        CURVED.ctrl2_x, -CURVED.ctrl2_y,
        CURVED.to_x, -CURVED.to_y,
    ];
    check('the drawn cubic is start, handle 1, handle 2, end — in that order',
        nums.length === 8 && want.every(function (v, i) { return Math.abs(nums[i] - v) < 1e-9; }),
        'got ' + JSON.stringify(nums) + ' want ' + JSON.stringify(want));
})();

(function reversingTheSceneEdgeDrawsTheSameCurve() {
    const m = load();
    // The same lane authored end-to-start. buildGraph swaps the handle pair
    // with the direction, so the shape on screen must not depend on which way
    // round RDS happened to store it — Springfield stores most lanes both ways.
    const reversed = {
        instance_name: 'LM113-LM10', class_name: 'DegenerateBezier',
        from_name: 'LM113', to_name: 'LM10',
        from_x: CURVED.to_x, from_y: CURVED.to_y,
        to_x: CURVED.from_x, to_y: CURVED.from_y,
        ctrl1_x: CURVED.ctrl2_x, ctrl1_y: CURVED.ctrl2_y,
        ctrl2_x: CURVED.ctrl1_x, ctrl2_y: CURVED.ctrl1_y,
    };
    const forward = drawnFor(load(), [CURVED])[0].attrs.d;
    const backward = drawnFor(m, [reversed])[0].attrs.d;
    // Sampled rather than string-compared: the two are the same curve
    // traversed in opposite directions, so their `d` strings differ.
    const f = parseCubic(forward), b = parseCubic(backward);
    let worst = 0;
    for (let i = 0; i <= 20; i++) {
        const t = i / 20;
        const pf = cubicAt(f, t), pb = cubicAt(b, 1 - t);
        worst = Math.max(worst, Math.hypot(pf[0] - pb[0], pf[1] - pb[1]));
    }
    check('both stored directions paint the same curve', worst < 1e-9,
        'max separation ' + worst);
})();

(function theReverseAdjacencyEntryDrawsCorrectly() {
    const m = load();
    // drawAisles dedups reciprocal entries by keeping the one hanging off the
    // LOWER vertex index, and vertex indices come from the order endpoints
    // first appear across the whole edge list. So whether the drawn entry is
    // the curve's forward or reverse direction is decided by an unrelated
    // edge earlier in the list.
    //
    // Here a straight lane introduces LM113 first, making it vertex 0 and the
    // REVERSE entry the one that gets drawn. buildGraph stores that entry's
    // handles swapped; if it did not, the drawn cubic would use handle 2 as
    // handle 1 and the bend would come out mirrored — on THIS lane about 1 m
    // of error, and only on the lanes whose endpoints happened to be indexed
    // in that order, which is the worst kind of wrong to notice.
    const lead = {
        instance_name: 'LM113-LMX', class_name: 'StraightPath',
        from_name: 'LM113', to_name: 'LMX',
        from_x: CURVED.to_x, from_y: CURVED.to_y, to_x: CURVED.to_x + 5, to_y: CURVED.to_y,
    };
    m.setEdges([lead, CURVED]);
    m.buildGraph();
    const idx = { LM113: 0 };
    check('the fixture puts LM113 at vertex 0 (else this test proves nothing)',
        Math.abs(m.tnodes()[idx.LM113].x - CURVED.to_x) < 1e-9 &&
        Math.abs(m.tnodes()[idx.LM113].y - CURVED.to_y) < 1e-9,
        JSON.stringify(m.tnodes()[0]));

    const svg = svgNode('svg');
    m.drawAisles(svg, 1);
    const p = svg.children.filter(function (c) { return c.tag === 'path'; });
    check('the curved lane still draws exactly one path', p.length === 1, 'got ' + p.length);

    // Same curve as the forward-drawn case, traversed the other way.
    const drawnRev = parseCubic(p[0].attrs.d);
    const drawnFwd = parseCubic(drawnFor(load(), [CURVED])[0].attrs.d);
    let worst = 0;
    for (let i = 0; i <= 20; i++) {
        const t = i / 20;
        const a = cubicAt(drawnRev, t), b = cubicAt(drawnFwd, 1 - t);
        worst = Math.max(worst, Math.hypot(a[0] - b[0], a[1] - b[1]));
    }
    check('the reverse-drawn entry paints the same curve as the forward one',
        worst < 1e-9, 'max separation ' + worst.toFixed(6) + ' m');
})();

(function aMixedNetworkDrawsBothShapes() {
    const m = load();
    const drawn = drawnFor(m, [CURVED, STRAIGHT]);
    const tags = drawn.map(function (d) { return d.tag; }).sort();
    check('a mixed network draws one of each',
        tags.length === 2 && tags[0] === 'line' && tags[1] === 'path',
        JSON.stringify(tags));
})();

(function oneElementPerPHYSICALLane() {
    const m = load();
    // ONE DRAWN AISLE PER PHYSICAL LANE, handed both stored directions of the
    // same lane. Springfield stores most of its network both ways — 405
    // directed edges over 212 physical lanes — so a renderer walking the
    // adjacency lists straight through paints nearly every aisle TWICE: double
    // stroke weight where the map is meant to be faint connective tissue, a
    // curve laid over its own mirror, and twice the DOM on a kiosk that
    // rebuilds the scene every SSE tick.
    //
    // The dedup has always been there and nothing asserted it. Every other
    // fixture here hands drawAisles a single directed edge, which the dedup
    // cannot get wrong — the reciprocal entry it must collapse is the one
    // buildGraph synthesises, and buildGraph's own `some(e.n === b)` guard
    // hides a broken key until two REAL twins arrive together.
    const drawn = drawnFor(m, [
        CURVED, reverseOf(CURVED), STRAIGHT, reverseOf(STRAIGHT), ONE_WAY,
    ]);
    check('5 directed edges over 3 physical lanes draw 3 elements',
        drawn.length === 3,
        'got ' + drawn.length + ': ' + JSON.stringify(drawn.map(function (d) { return d.tag; })));
    const tags = drawn.map(function (d) { return d.tag; }).sort();
    check('the survivor of each pair keeps its shape — one <path>, two <line>s',
        JSON.stringify(tags) === JSON.stringify(['line', 'line', 'path']),
        JSON.stringify(tags));
})();

// --- the projection ------------------------------------------------------
//
// NOT covered by anything above, and the gap had teeth. Every fixture in this
// file is wider than it is tall, so proj() resolves to its unrotated [x, −y]
// branch throughout — theDrawnCubicIsTheSceneGeometry says as much in its own
// comment. Springfield is the other way round: its drivable network measures
// 55.5 m across (x −52.6…2.85) by 83.2 m deep (y −22.38…60.85), so the live
// kiosk runs the ROTATED branch and no assertion here ever touched it. An
// extraction that dropped the rotation would have passed the whole suite and
// letterboxed the real floor into a thin vertical strip.

console.log('projection');

(function orientationFollowsThePlantFootprint() {
    // computeView is what decides orientation, from the FULL-plant extent, and
    // it is driven here rather than a predicate tested in isolation — the
    // decision and the projector it configures are the pair that has to agree.
    const tall = load();
    tall.setPoints([{ pos_x: -52.6, pos_y: -22.38 }, { pos_x: 2.85, pos_y: 60.85 }]);
    tall.computeView();
    const t = tall.proj(3, 7);
    check('a taller-than-wide plant projects rotated — [y, x]',
        t[0] === 7 && t[1] === 3, JSON.stringify(t));

    // The same extent transposed: 83.2 m across by 55.5 m deep.
    const wide = load();
    wide.setPoints([{ pos_x: -22.38, pos_y: -52.6 }, { pos_x: 60.85, pos_y: 2.85 }]);
    wide.computeView();
    // BOTH directions asserted deliberately. "Rotate always" and "rotate never"
    // are the two ways this breaks, each still renders a map, and each is wrong
    // on only one of the two footprints — one check would catch one of them.
    const w = wide.proj(3, 7);
    check('a wider-than-tall plant projects unrotated — [x, −y]',
        w[0] === 3 && w[1] === -7, JSON.stringify(w));
})();

(function theRotatedBranchIsExactly90CW() {
    const m = load();
    m.setPoints([{ pos_x: -52.6, pos_y: -22.38 }, { pos_x: 2.85, pos_y: 60.85 }]);
    m.computeView();
    // The rotated branch is documented as "90° CW of the (x, −y) base image".
    // Screen Y points down, so a 90° CW turn maps (a, b) → (−b, a); applied to
    // the base that is exactly (y, x). Asserted as the composition rather than
    // as the literal pair, so the two spellings cannot drift apart — and over
    // the real corners of the Springfield footprint, where a sign slip that
    // vanishes at the origin does not.
    let worst = 0;
    [[0, 0], [1, 0], [0, 1], [-3.5, 12.25], [-52.6, 60.85], [2.85, -22.38]].forEach(function (p) {
        const base = [p[0], -p[1]];       // the unrotated projection
        const cw = [-base[1], base[0]];   // turned 90° CW, screen Y down
        const got = m.proj(p[0], p[1]);
        worst = Math.max(worst, Math.hypot(got[0] - cw[0], got[1] - cw[1]));
    });
    check('the rotated branch is exactly 90° CW of the unrotated image',
        worst < 1e-12, 'max separation ' + worst);
})();

// --- routes ride the lane ------------------------------------------------
// The comet, the frozen lane and the intent line all draw routeWorld's
// polyline. It used to join the graph's VERTICES, so a route over LM10-LM113
// drew the 7.17 m chord while the aisle beneath it bowed 1.30 m away.

console.log('routes');

(function aRouteOverACurveFollowsTheCurve() {
    const m = load();
    m.setEdges([CURVED, STRAIGHT, ONE_WAY]);
    m.buildGraph();
    const from = { x: CURVED.from_x, y: CURVED.from_y };
    const to = { x: CURVED.to_x, y: CURVED.to_y };
    const c = [CURVED.ctrl1_x, CURVED.ctrl1_y, CURVED.ctrl2_x, CURVED.ctrl2_y];

    const fwd = m.routeWorld(from.x, from.y, to);
    // Every point but the robot and destination ends is on the authored cubic.
    let worst = 0;
    for (let i = 1; i < fwd.length - 1; i++) {
        let best = Infinity;
        for (let k = 0; k <= 200; k++) {
            const p = m.cubicPoint(from, c, to, k / 200);
            best = Math.min(best, Math.hypot(fwd[i][0] - p.x, fwd[i][1] - p.y));
        }
        worst = Math.max(worst, best);
    }
    check('a route over a curved lane has interior points, not just the chord',
        fwd.length > 4, 'points ' + fwd.length);
    check('every route point over a curved lane lies on the authored cubic',
        worst < 0.02, 'worst off-curve ' + worst.toFixed(4) + ' m');
    // The bow is where the chord was wrong: the route must pass through it.
    const bow = m.cubicPoint(from, c, to, 0.5);
    const reach = Math.min.apply(null, fwd.map(function (p) { return Math.hypot(p[0] - bow.x, p[1] - bow.y); }));
    check('the route passes through the lane\'s bow, not across its chord',
        reach < 1e-9, 'closest ' + reach);

    // Driven the other way, the same lane: the reversed handle pair, so the
    // quarter point going back is the three-quarter point going out.
    const back = m.routeWorld(to.x, to.y, from);
    const q3 = m.cubicPoint(from, c, to, 0.75);
    const reachBack = Math.min.apply(null, back.map(function (p) { return Math.hypot(p[0] - q3.x, p[1] - q3.y); }));
    check('a route driven against the stored direction rides the same curve',
        reachBack < 1e-9, 'closest ' + reachBack);
})();

(function aStraightLaneAddsNoPoints() {
    const m = load();
    m.setEdges([CURVED, STRAIGHT, ONE_WAY]);
    m.buildGraph();
    const r = m.routeWorld(STRAIGHT.from_x, STRAIGHT.from_y, { x: STRAIGHT.to_x, y: STRAIGHT.to_y });
    // robot, LM100, AP102, destination — nothing sampled on a straight segment.
    check('a route over a straight lane is its vertices only', r.length === 4, 'points ' + r.length);
})();

// --- route vectors: the route model over the synthetic route scene --------
//
// THE ROUTE THE MAP DRAWS AN INTENT LINE ALONG, vertex by vertex, on
// shared/testdata/js/route-scene.json, the scene the Edge's routers are
// pinned on too (composer-model.test.js, processes-desktop.route.test.js),
// through the page's own adaptation of /api/map/edges (sceneOfRows). Three
// ways run from SMN_05 to PLN_02: A straight and two-way (20 m), C straight
// and stored ONE WAY toward SMN_05 (16.47 m), B two cubic hops (16 m of chord,
// 21.6 m of arc). The route model (shared/scene-net.js sceneRoute, lead
// decision D9) keeps the stored direction and weighs the arc, so it takes A,
// as RDS plans.
//
// THREE ANSWERS FLIPPED ON PURPOSE (D9, 2026-11-14) when the map moved onto
// the route model: the two this map drove against the stored direction while
// its graph was undirected (SMN_05 -> PLN_02 along C, LM931 -> LM932 straight
// back up C), and a vertex to itself, which was [itself] and is no route.

console.log('route vectors (synthetic route scene)');

(function routeVectorsOnTheSyntheticScene() {
    // The scene as GET /api/map/edges serves it (route-scene.js).
    const RS = require('./route-scene.js');
    const at = RS.at;
    const m = load();
    m.setEdges(RS.edgeRows());
    m.buildGraph();
    function walk(a, b) {
        const seq = m.sceneRoute(m.scene(), a, b);
        return seq ? seq.join(' ') : null;
    }
    [
        // [from, to, the route model's vertices, why]
        ['SMN_05', 'PLN_02', 'SMN_05 LM911 LM912 PLN_02', 'A: C is stored the other way (was C, against it: D9)'],
        ['PLN_02', 'SMN_05', 'PLN_02 LM932 LM931 SMN_05', 'C, with the stored direction'],
        ['LM931', 'LM932', 'LM931 SMN_05 LM911 LM912 PLN_02 LM932', 'the 24.5 m loop via A (was straight back up C: D9)'],
        ['LM921', 'LM911', 'LM921 SMN_05 LM911', 'a curved hop, then a straight one'],
        ['LM911', 'PLN_02', 'LM911 LM912 PLN_02', 'A: undirected and directed agree'],
        ['LM912', 'SMN_05', 'LM912 LM911 SMN_05', 'A: undirected and directed agree'],
        ['LM941', 'LM942', 'LM941 LM942', 'the island, inside itself'],
        ['LM921', 'LM941', null, 'the island is unreachable'],
        ['LM921', 'LM921', null, 'a vertex to itself is no route (was [LM921]: D9)'],
    ].forEach(function (v) {
        const got = walk(v[0], v[1]);
        check(v[0] + ' -> ' + v[1] + ': ' + (v[2] === null ? 'no route' : v[2]) + ' (' + v[3] + ')',
            got === v[2], 'got ' + JSON.stringify(got));
    });

    // routeWorld is the polyline drawn: the robot's own place, then the
    // vertices, every curved hop sampled CUBIC_SAMPLES-1 times between its
    // ends, then the destination's place. The ends snap to the NEAREST vertex.
    const snapped = m.routeWorld(8.4, 0.3, { x: 16.2, y: 0.1 });
    check('routeWorld snaps the robot to LM921 and the destination to PLN_02, over the curved hop',
        snapped && snapped.length === 27 &&
        snapped[1][0] === 8 && snapped[1][1] === 0 && snapped[25][0] === 16 && snapped[25][1] === 0,
        snapped ? snapped.length + ' points, [1]=' + JSON.stringify(snapped[1]) + ' [25]=' + JSON.stringify(snapped[25]) : 'null');
    const straight = m.routeWorld(at.SMN_05.x, at.SMN_05.y, at.PLN_02);
    check('the straight route SMN_05 -> PLN_02 draws robot, four vertices, destination: 6 points',
        straight && straight.length === 6, straight ? JSON.stringify(straight) : 'null');
    check('no network route: routeWorld answers null (the caller draws a straight hint)',
        m.routeWorld(at.LM921.x, at.LM921.y, at.LM941) === null);
})();

// --- helpers -------------------------------------------------------------

function parseCubic(d) {
    return (d || '').split(/[MC ]+/).filter(function (s) { return s !== ''; }).map(Number);
}

function cubicAt(n, t) {
    const mt = 1 - t;
    const a = mt * mt * mt, b = 3 * mt * mt * t, c = 3 * mt * t * t, e = t * t * t;
    return [
        a * n[0] + b * n[2] + c * n[4] + e * n[6],
        a * n[1] + b * n[3] + c * n[5] + e * n[7],
    ];
}

console.log(failures === 0 ? 'PASS' : failures + ' FAILURE(S)');
process.exit(failures === 0 ? 0 : 1);
