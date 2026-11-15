// scene-path.test.js — a robot's route drawn as RDS planned it (shared/scene-path.js)
// on the synthetic route scene (route-scene.json, beside this file).
//
// Lead decisions D6 and D10: a driving robot's route is RDS's path, each hop
// a->b along the stored a->b row with that row's own handles, starting at the
// robot's projection onto the first hop. A hop with no stored row is no route
// at all, counted; an empty path is no route; a one-name path is a robot at its
// last point, which draws nothing.
//
// Run under plain Node via the Go wrapper scene_geom_test.go. Exit 0 on pass.

'use strict';

const fs = require('fs');
const path = require('path');
const { realm } = require('./shared-modules.js');

let failures = 0;
function check(name, cond, detail) {
    if (cond) { console.log('  ok   ' + name); return; }
    failures++;
    console.log('  FAIL ' + name + (detail ? '\n       ' + detail : ''));
}

const load = realm();
const { pathWorld, hopsWorld } = load('scene-path.js');
const { cubicPoint, CUBIC_SAMPLES } = load('scene-geom.js');

const SCENE = JSON.parse(fs.readFileSync(path.join(__dirname, 'route-scene.json'), 'utf8'));
const MAP = { points: {}, edges: [] };
SCENE.points.forEach(function (p) { MAP.points[p.name] = { x: p.x, y: p.y, class: p.class }; });
SCENE.segments.forEach(function (s) { MAP.edges.push(s.h ? { from: s.from, to: s.to, h: s.h.slice() } : { from: s.from, to: s.to }); });

const spell = function (pts) { return pts ? pts.map(function (p) { return p.join(','); }).join(' ') : null; };
const A = ['SMN_05', 'LM911', 'LM912', 'PLN_02'];

console.log('the route as RDS planned it');
(function theRoute() {
    check('a robot partway along its first hop: from its projection onto that hop, then every hop whole',
        spell(pathWorld(MAP, { x: 0.5, y: 0.3 }, A)) === '0,0.3 0,2 16,2 16,0', spell(pathWorld(MAP, { x: 0.5, y: 0.3 }, A)));
    check('a robot short of its first point starts at that point (the projection is onto the hop, not its line)',
        spell(pathWorld(MAP, { x: -0.5, y: -1 }, A)) === '0,0 0,2 16,2 16,0', spell(pathWorld(MAP, { x: -0.5, y: -1 }, A)));
    check('a robot with no position starts at the first point', spell(pathWorld(MAP, { x: NaN, y: 0 }, A)) === '0,0 0,2 16,2 16,0' &&
        spell(pathWorld(MAP, null, A)) === '0,0 0,2 16,2 16,0');
    check('the route ends at the path\'s last point: nothing is added after it',
        spell(pathWorld(MAP, { x: 0, y: 0 }, ['LM911', 'LM912'])) === '0,2 16,2');
    check('names match without case', spell(pathWorld(MAP, { x: 0, y: 0 }, ['smn_05', 'lm911'])) === '0,0 0,2');
})();

console.log('what is no route');
(function noRoute() {
    const tally = {};
    check('an empty path is no route', pathWorld(MAP, { x: 0, y: 0 }, [], tally) === null &&
        pathWorld(MAP, { x: 0, y: 0 }, undefined, tally) === null);
    check('a one-name path (the robot at its last point) draws nothing', pathWorld(MAP, { x: 16, y: 0 }, ['PLN_02'], tally) === null);
    check('...and neither is counted as a miss', !tally.misses, JSON.stringify(tally));
    // Route C driven from SMN_05: every hop runs against its stored row.
    check('a hop with no stored row in its direction is no route at all, not half of one',
        pathWorld(MAP, { x: 0, y: 0 }, ['SMN_05', 'LM911', 'LM912', 'PLN_02', 'LM932', 'LM932x'], tally) === null &&
        pathWorld(MAP, { x: 0, y: 0 }, ['SMN_05', 'LM931', 'LM932', 'PLN_02'], tally) === null, JSON.stringify(tally));
    check('...and each one is counted', tally.misses === 2, JSON.stringify(tally));
    check('a name the scene does not have is a missing hop too',
        pathWorld(MAP, { x: 0, y: 0 }, ['SMN_05', 'NOWHERE'], tally) === null && tally.misses === 3, JSON.stringify(tally));
    check('a robot already past the end of its only hop draws nothing',
        pathWorld(MAP, { x: 0, y: 3 }, ['SMN_05', 'LM911']) === null);
})();

console.log('one way');
(function oneWay() {
    check('a one-way stretch is drawn in its stored direction, PLN_02 -> LM932 -> LM931 -> SMN_05',
        spell(pathWorld(MAP, { x: 16, y: 0 }, ['PLN_02', 'LM932', 'LM931', 'SMN_05'])) === '16,0 14,1 2,1 0,0');
    check('...and the other way it is no route', pathWorld(MAP, { x: 0, y: 0 }, ['SMN_05', 'LM931', 'LM932', 'PLN_02']) === null);
})();

console.log('curves');
(function curves() {
    const a = MAP.points.LM921, b = MAP.points.PLN_02, h = [9, -4, 15, -4];
    const whole = pathWorld(MAP, { x: 8, y: 0 }, ['LM921', 'PLN_02']);
    let worst = 0;
    for (let k = 0; k <= CUBIC_SAMPLES; k++) {
        const q = cubicPoint(a, h, b, k / CUBIC_SAMPLES);
        worst = Math.max(worst, Math.hypot(whole[k][0] - q.x, whole[k][1] - q.y));
    }
    check('a curved hop is its row\'s cubic, sampled at CUBIC_SAMPLES: ' + (CUBIC_SAMPLES + 1) + ' points',
        whole.length === CUBIC_SAMPLES + 1 && worst < 1e-12, whole.length + ' points, worst ' + worst);
    const half = pathWorld(MAP, { x: 12, y: -3.2 }, ['LM921', 'PLN_02']);
    check('a robot at the bow starts there: the second half of the curve, 13 points',
        half.length === 13 && half[0][0] === 12 && half[0][1] === -3, half.length + ' ' + JSON.stringify(half[0]));
    // The other way along the same lane is the PLN_02>LM921 row, with its own
    // handles: the same curve, read the other way.
    const back = pathWorld(MAP, { x: 16, y: 0 }, ['PLN_02', 'LM921']);
    let sep = 0;
    for (let k = 0; k <= CUBIC_SAMPLES; k++) sep = Math.max(sep, Math.hypot(back[k][0] - whole[CUBIC_SAMPLES - k][0], back[k][1] - whole[CUBIC_SAMPLES - k][1]));
    check('the hop the other way is drawn from that row\'s own handles: the same curve backwards', sep < 1e-9, 'separation ' + sep);
    const bowed = { points: MAP.points, edges: [{ from: 'PLN_02', to: 'LM921', h: [16, 4, 8, 4] }, { from: 'LM921', to: 'PLN_02', h: [9, -4, 15, -4] }] };
    check('each hop takes its own row, not its twin\'s: two rows stored with different bows draw two lines',
        pathWorld(bowed, { x: 16, y: 0 }, ['PLN_02', 'LM921'])[12][1] > 0 && pathWorld(bowed, { x: 8, y: 0 }, ['LM921', 'PLN_02'])[12][1] < 0);
})();

console.log('a route model answer, drawn');
check('hopsWorld draws a list of names whole, from the first name\'s place', spell(hopsWorld(MAP, A)) === '0,0 0,2 16,2 16,0');
check('...and is no line with a missing hop or fewer than two names',
    hopsWorld(MAP, ['SMN_05', 'LM931']) === null && hopsWorld(MAP, ['SMN_05']) === null);

console.log(failures === 0 ? 'PASS' : failures + ' FAILURE(S)');
process.exit(failures === 0 ? 0 : 1);
