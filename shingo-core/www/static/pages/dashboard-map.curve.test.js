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
// tnodes/tadj/sceneEdges are REASSIGNED inside the module, so they are handed
// out through accessors — an exported array reference would go stale the first
// time buildGraph ran and every assertion after that would read the old graph.
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

    const file = path.join(__dirname, 'dashboard-map.js');
    const raw = fs.readFileSync(file, 'utf8');
    const stripped = raw.replace(/^import[^;]+;\s*/m, '');
    const INJECT = '  __export({ buildGraph: buildGraph, drawAisles: drawAisles,' +
        ' cubicLength: cubicLength, cubicPoint: cubicPoint,' +
        ' setEdges: function (v) { sceneEdges = v; },' +
        ' tnodes: function () { return tnodes; }, tadj: function () { return tadj; },' +
        ' graphScale: function () { return graphScale; } });\n})();\n';
    const src = stripped.replace(/\}\)\(\);\s*$/, INJECT);
    if (src === stripped) {
        throw new Error('dashboard-map.js no longer ends in the "})();" IIFE close this ' +
            'harness injects before; update the injection in dashboard-map.curve.test.js');
    }
    vm.runInContext(src, ctx);
    if (!exported) throw new Error('__export was not reached inside dashboard-map.js');
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

function chord(e) {
    return Math.hypot(e.to_x - e.from_x, e.to_y - e.from_y);
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
