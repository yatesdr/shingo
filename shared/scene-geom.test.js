// scene-geom.test.js — the agreement test for the promoted scene geometry.
//
// scene-geom.vectors.json is the recorded output of the Core copy this module
// was promoted from (shingo-core/www/static/components/scene-geom.js at the
// promoting commit): five segments — a BezierPath, a DegenerateBezier, and
// three synthetic cases around the origin — pushed through the projection in
// both orientations, the cubic length, the midpoint, the coordinate predicate
// and the lane key. Every one is re-derived here through THIS file and
// compared exactly. A mismatch is a behaviour change to both surfaces that
// draw the plant, and the way to make one is to change the vector on purpose
// in the same commit, never to loosen the comparison.
//
// THE INPUTS ARE LITERALS HERE, NOT A FIXTURE READ. The two non-synthetic
// segments used to be looked up by name in the committed Springfield pull, so
// that the test did not take its inputs from the file it is checking. The pull
// is gone (owner ruling 2026-09-13) and the synthetic plant that replaced it
// moves every coordinate — which would invalidate an ORACLE that cannot be
// rebuilt, because the Core copy these numbers came from was deleted by the
// promotion. So the two rows are frozen below instead: still not read from the
// vector file, and the recorded outputs still mean what they meant. They are
// two anonymous segments, no name and no identity attached.
//
// Run under plain Node via the Go wrapper scene_geom_test.go. Exit 0 on pass.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let failures = 0;
function check(name, cond, detail) {
    if (cond) { console.log('  ok   ' + name); return; }
    failures++;
    console.log('  FAIL ' + name + (detail ? '\n       ' + detail : ''));
}

function load() {
    const file = path.join(__dirname, 'scene-geom.js');
    const raw = fs.readFileSync(file, 'utf8');
    const src = raw.replace(/^export /mg, '');
    if (src === raw) {
        throw new Error('shared/scene-geom.js no longer declares its exports as "export function"/"export var" ' +
            'at line start, which this harness strips to run it as a script; update load() in scene-geom.test.js');
    }
    const ctx = { Math: Math, isFinite: isFinite, Number: Number };
    vm.createContext(ctx);
    vm.runInContext(src, ctx);
    return ctx;
}

const g = load();
const vectors = JSON.parse(fs.readFileSync(path.join(__dirname, 'scene-geom.vectors.json'), 'utf8'));

check('vectors carry at least the five recorded segments', vectors.edges.length >= 5, String(vectors.edges.length));
check('CUBIC_SAMPLES is the recorded resolution', g.CUBIC_SAMPLES === vectors.cubicSamples,
    g.CUBIC_SAMPLES + ' vs ' + vectors.cubicSamples);

// The same inputs the generator used, held here rather than in the vector
// file, so the test does not take its inputs from the thing it is checking.
const byName = {
    // A BezierPath and a DegenerateBezier, as a vendor stores them: complete
    // handle pairs, and the second one's handles lie on its chord, which is
    // what "degenerate" means and why the class string cannot decide curvature.
    'LM9-PP224': { from_x: -0.604, from_y: 22.449, to_x: 0.986, to_y: 22.169, ctrl1_x: -0.287, ctrl1_y: 22.094, ctrl2_x: 0.303, ctrl2_y: 22.142 },
    'AP123-LM135': { from_x: -35.459, from_y: 57.966, to_x: -38.448, to_y: 58.156, ctrl1_x: -36.455, ctrl1_y: 58.029, ctrl2_x: -37.452, ctrl2_y: 58.093 },
};
const synth = {
    'SYN-origin-chord': { from_x: 0, from_y: 0, to_x: 3, to_y: 4, ctrl1_x: null, ctrl1_y: null, ctrl2_x: null, ctrl2_y: null },
    'SYN-origin-handles': { from_x: -1, from_y: -1, to_x: 1, to_y: 1, ctrl1_x: 0, ctrl1_y: 0, ctrl2_x: 0, ctrl2_y: 0 },
    'SYN-partial-handles': { from_x: 1, from_y: 1, to_x: -1, to_y: -1, ctrl1_x: 0.5, ctrl1_y: 0.5, ctrl2_x: null, ctrl2_y: null },
};

console.log('recorded vectors');
for (const rec of vectors.edges) {
    const e = byName[rec.instance_name] || synth[rec.instance_name];
    check(rec.instance_name + ': input segment is known', !!e);
    if (!e) continue;
    const c = [e.ctrl1_x, e.ctrl1_y, e.ctrl2_x, e.ctrl2_y];
    const curved = c.every(g.isCoord);
    check(rec.instance_name + ': curved decision matches', curved === rec.curved, curved + ' vs ' + rec.curved);
    const p0 = { x: e.from_x, y: e.from_y }, p3 = { x: e.to_x, y: e.to_y };
    const length = curved ? g.cubicLength(p0, c, p3) : Math.sqrt(g.dist2(p0, p3));
    check(rec.instance_name + ': length matches exactly', length === rec.length, length + ' vs ' + rec.length);
    for (const [key, rot] of [['upright', false], ['rotated', true]]) {
        const proj = g.makeProjector(rot);
        const P0 = proj(p0.x, p0.y), P3 = proj(p3.x, p3.y);
        check(rec.instance_name + ': ' + key + ' endpoints match',
            JSON.stringify([P0, P3]) === JSON.stringify(rec[key].endpoints),
            JSON.stringify([P0, P3]) + ' vs ' + JSON.stringify(rec[key].endpoints));
        const d = curved ? g.cubicPathD(P0, proj(c[0], c[1]), proj(c[2], c[3]), P3)
            : 'M' + P0[0] + ' ' + P0[1] + 'L' + P3[0] + ' ' + P3[1];
        check(rec.instance_name + ': ' + key + ' path matches', d === rec[key].pathD, d + ' vs ' + rec[key].pathD);
    }
    if (curved) {
        const m = g.cubicPoint(p0, c, p3, 0.5);
        check(rec.instance_name + ': midpoint matches', m.x === rec.midpoint.x && m.y === rec.midpoint.y,
            JSON.stringify(m) + ' vs ' + JSON.stringify(rec.midpoint));
    }
}

console.log('isCoord');
const probes = { zero: 0, negative: -3.5, null: null, undefined: undefined, nan: NaN, 'string-zero': '0', infinity: Infinity };
for (const [label, v] of Object.entries(probes)) {
    check('isCoord(' + label + ') = ' + vectors.isCoord[label], g.isCoord(v) === vectors.isCoord[label], String(g.isCoord(v)));
}
check('isCoord(null) is false even though isFinite(null) is true', g.isCoord(null) === false && isFinite(null) === true);

console.log('laneKey');
for (const rec of vectors.laneKey) {
    check('laneKey(' + rec.a + ', ' + rec.b + ') = ' + rec.key, g.laneKey(rec.a, rec.b) === rec.key, g.laneKey(rec.a, rec.b));
}

// rotate90For is new with the promotion — it was an inline expression in
// Core's computeView — so it is pinned on whole-plant bounds rather than on a
// vector: a portrait plant rotates, a landscape one does not, and the decision
// is over whatever bounds are handed in, which the callers make the FULL
// plant's. The first two are the extents two real plants have; a bounding box
// is four numbers and names nothing.
console.log('rotate90For');
check('a 116.8 m x 164.0 m plant rotates', g.rotate90For(-99.412, 17.392, -103.309, 60.697) === true);
check('a 75.7 m x 102.9 m plant rotates', g.rotate90For(-52.621, 23.061, -22.377, 80.529) === true);
check('a landscape plant does not', g.rotate90For(0, 100, 0, 60) === false);
check('a square plant does not (strictly taller only)', g.rotate90For(0, 50, 0, 50) === false);
check('one press alone would NOT decide it (a subgraph is the wrong input)', g.rotate90For(-21.945, -15.157, 59.542, 60.697) === false);

if (failures) { console.log(failures + ' FAILED'); process.exit(1); }
console.log('scene-geom agreement: all vectors match');
