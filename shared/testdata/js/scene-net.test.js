// scene-net.test.js — the scene network and the route model (shared/scene-net.js)
// on the synthetic route scene (route-scene.json, beside this file).
//
// THE ROUTE MODEL's VECTORS ARE THE DESKTOP's (lead decision D9). The Edge's
// processes-desktop.js travelGraph/graphNodeFor/route is directed and weighs
// arcs, and its answers on this scene are pinned by TestProcessesDesktopRouteJS
// (shingo-edge/www/testdata/static/js/pages/processes-desktop.route.test.js).
// Every one of those vectors is here with the same answer, so the model
// agrees with the desktop, except the one case named below: the desktop snaps
// a destination over the vertices with a row LEAVING them, so a dead end, which
// rows only arrive at, snapped away from itself. When the desktop map is
// removed these vectors live on here.
//
// THE LANES ARE CORE's AISLES. Core's robot map drew every physical lane once,
// in buildGraph's vertex order, each from its earlier vertex, and its
// scene golden (dashboard-map.scene.golden.txt) holds that drawing. The lane
// list pinned here is that order and orientation.
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
const { sceneNet, sceneRoute, nearestVertex } = load('scene-net.js');

// The route scene as the D11 shape (ComposerMap): points by name, one edge per
// stored row with its handles in its own direction.
const SCENE = JSON.parse(fs.readFileSync(path.join(__dirname, 'route-scene.json'), 'utf8'));
function d11(scene, more) {
    const m = { points: {}, edges: [] };
    scene.points.forEach(function (p) { m.points[p.name] = { x: p.x, y: p.y, class: p.class }; });
    scene.segments.forEach(function (s) {
        const e = { from: s.from, to: s.to };
        if (s.h) e.h = s.h.slice();
        m.edges.push(e);
    });
    if (more) {
        Object.keys(more.points || {}).forEach(function (n) { m.points[n] = more.points[n]; });
        (more.edges || []).forEach(function (e) { m.edges.push(e); });
    }
    return m;
}
const MAP = d11(SCENE);
const walk = function (a, b, scene) {
    const r = sceneRoute(scene || MAP, a, b);
    return r ? r.join(' ') : null;
};

console.log('the network');
(function theNetwork() {
    const net = sceneNet(MAP);
    check('sceneNet reads a scene once: the same object hands back the same network', sceneNet(MAP) === net);
    check('the vertices are the points the rows name, in the order the rows first name them',
        net.names.join(' ') === 'SMN_05 LM911 LM912 PLN_02 LM932 LM931 LM921 LM941 LM942', net.names.join(' '));
    check('directed: a hop only where a row is stored',
        !!net.hop('LM931', 'SMN_05') && net.hop('SMN_05', 'LM931') === null &&
        !!net.hop('PLN_02', 'LM932') && net.hop('LM932', 'PLN_02') === null);
    const arc = net.hop('SMN_05', 'LM921');
    check('a curved row weighs its arc, not its chord (8)', arc.len > 10.79 && arc.len < 10.81, String(arc.len));
    check('...and carries its handles in its own direction',
        JSON.stringify(arc.h) === '[1,-4,7,-4]' && JSON.stringify(net.hop('LM921', 'SMN_05').h) === '[7,-4,1,-4]');
    check('a straight row weighs its chord', net.hop('LM911', 'LM912').len === 16 && net.hop('LM911', 'LM912').h === null);
    check('the scale is the median row length, every stored row counted (Core\'s graphScale): 4',
        net.scale === 4, String(net.scale));
})();

console.log('the lanes');
(function theLanes() {
    const lanes = sceneNet(MAP).lanes.map(function (l) {
        return l.from + '>' + l.to + (l.h ? ' ' + JSON.stringify(l.h) : '');
    });
    const want = ['SMN_05>LM911', 'SMN_05>LM931', 'SMN_05>LM921 [1,-4,7,-4]', 'LM911>LM912', 'LM912>PLN_02',
        'PLN_02>LM932', 'PLN_02>LM921 [15,-4,9,-4]', 'LM932>LM931', 'LM941>LM942'];
    check('15 rows are 9 lanes, the three one-way ones included, each once, in Core\'s order and orientation',
        JSON.stringify(lanes) === JSON.stringify(want), JSON.stringify(lanes));
    // SMN_05-LM931 is stored only as LM931>SMN_05: the lane is drawn from SMN_05,
    // the earlier vertex, along that row read backwards.
    const flipped = d11({ points: SCENE.points, segments: [{ from: 'PLN_02', to: 'SMN_05', h: [12, 4, 4, 4] }] });
    const l = sceneNet(flipped).lanes;
    check('a lane stored one way is drawn from its earlier vertex, its handles turned to run that way',
        l.length === 1 && l[0].from === 'PLN_02' && JSON.stringify(l[0].h) === '[12,4,4,4]', JSON.stringify(l));
    const lead = d11({ points: SCENE.points, segments: [{ from: 'SMN_05', to: 'LM911' }, { from: 'PLN_02', to: 'SMN_05', h: [12, 4, 4, 4] }] });
    const l2 = sceneNet(lead).lanes;
    check('...and when an earlier row makes the far end the earlier vertex, from there, the handles reversed',
        l2.length === 2 && l2[1].from === 'SMN_05' && l2[1].to === 'PLN_02' && JSON.stringify(l2[1].h) === '[4,4,12,4]',
        JSON.stringify(l2));
})();

console.log('the route model: the desktop\'s vectors (TestProcessesDesktopRouteJS)');
[
    // [from, to, the desktop's answer as vertices, why]
    ['SMN_05', 'PLN_02', 'SMN_05 LM911 LM912 PLN_02', 'A: C is one-way the other way, B is longer by arc'],
    ['PLN_02', 'SMN_05', 'PLN_02 LM932 LM931 SMN_05', 'C, with its stored direction'],
    ['LM931', 'LM932', 'LM931 SMN_05 LM911 LM912 PLN_02 LM932', 'the loop round, not back down C'],
    ['LM921', 'LM911', 'LM921 SMN_05 LM911', 'a curved hop, then a straight one'],
    ['LM911', 'PLN_02', 'LM911 LM912 PLN_02', 'A'],
    ['LM921', 'LM941', null, 'the island is unreachable'],
    ['LM921', 'LM921', null, 'a vertex to itself is no route'],
].forEach(function (v) {
    const got = walk(v[0], v[1]);
    check(v[0] + ' -> ' + v[1] + ': ' + (v[2] === null ? 'none' : v[2]) + ' (' + v[3] + ')', got === v[2], 'got ' + JSON.stringify(got));
});
[
    // Two more from Core's map pins (dashboard-map.curve.test.js), where the
    // undirected and the directed answers agree.
    ['LM912', 'SMN_05', 'LM912 LM911 SMN_05', 'A'],
    ['LM941', 'LM942', 'LM941 LM942', 'the island, inside itself'],
].forEach(function (v) {
    const got = walk(v[0], v[1]);
    check(v[0] + ' -> ' + v[1] + ': ' + v[2] + ' (' + v[3] + ')', got === v[2], 'got ' + JSON.stringify(got));
});

console.log('the route model: the ends');
(function theEnds() {
    check('a point on no segment starts from the nearest vertex with a way out (the desktop: BIN_901 -> PLN_02)',
        walk('BIN_901', 'LM911') === 'PLN_02 LM912 LM911', walk('BIN_901', 'LM911'));
    check('...and is reached at the nearest vertex with a way in', walk('LM911', 'BIN_901') === 'LM911 LM912 PLN_02',
        walk('LM911', 'BIN_901'));
    check('a name the scene does not have is no end', walk('NOT_ON_THE_MAP', 'PLN_02') === null && walk('SMN_05', '') === null);
    check('a place snaps: (8.4, 0.3) starts at LM921, (16.2, 0.1) arrives at PLN_02, over the curved row',
        JSON.stringify(sceneRoute(MAP, { x: 8.4, y: 0.3 }, { x: 16.2, y: 0.1 })) === '["LM921","PLN_02"]',
        JSON.stringify(sceneRoute(MAP, { x: 8.4, y: 0.3 }, { x: 16.2, y: 0.1 })));
    check('a place that is not one is no end', sceneRoute(MAP, { x: NaN, y: 0 }, 'PLN_02') === null &&
        sceneRoute(MAP, { x: null, y: 0 }, 'PLN_02') === null);
    check('names match without case, and the answer is spelled as the scene spells them',
        walk('smn_05', 'pln_02') === 'SMN_05 LM911 LM912 PLN_02', walk('smn_05', 'pln_02'));
    // (32, 10) is 2 m from LM941 and from LM942. LM941>LM942 is stored before
    // LM942>LM941, so LM941 is the first vertex a row leaves and LM942 the
    // first a row arrives at.
    check('ties go to the vertex whose first such row is stored first: (32, 10) starts at LM941 and arrives at LM942',
        JSON.stringify(sceneRoute(MAP, { x: 32, y: 10 }, { x: 32, y: 10 })) === '["LM941","LM942"]',
        JSON.stringify(sceneRoute(MAP, { x: 32, y: 10 }, { x: 32, y: 10 })));
    const r = sceneRoute(MAP, 'SMN_05', 'PLN_02');
    r.push('X');
    check('an answer is the caller\'s to keep: changing it changes no later answer', walk('SMN_05', 'PLN_02') === 'SMN_05 LM911 LM912 PLN_02');
})();

console.log('the route model: a dead end (the flat bug fixed)');
(function aDeadEnd() {
    // DEAD_1 is reached by one row, LM912>DEAD_1, and left by none. The
    // desktop snapped the destination over the vertices with a way OUT, so it
    // answered SMN_05 -> LM912 and never reached DEAD_1.
    const spur = d11(SCENE, { points: { DEAD_1: { x: 18, y: 4, class: 'LocationMark' } }, edges: [{ from: 'LM912', to: 'DEAD_1' }] });
    check('a destination rows only arrive at is reached: SMN_05 -> DEAD_1 ends at DEAD_1',
        walk('SMN_05', 'DEAD_1', spur) === 'SMN_05 LM911 LM912 DEAD_1', walk('SMN_05', 'DEAD_1', spur));
    check('as a start it has no way out, so it starts from its nearest vertex with one, LM912',
        walk('DEAD_1', 'SMN_05', spur) === 'LM912 LM911 SMN_05', walk('DEAD_1', 'SMN_05', spur));
    // A place beside it, 0.2 m off, as a destination snaps to the dead end
    // (the vertices rows arrive at); the desktop's pool was LM912 and the rest.
    const near = { x: 18, y: 4.2 };
    check('a place beside the dead end, as a destination, snaps to it; as a start, to LM912',
        JSON.stringify(sceneRoute(spur, 'SMN_05', near)) === '["SMN_05","LM911","LM912","DEAD_1"]' &&
        JSON.stringify(sceneRoute(spur, near, 'SMN_05')) === '["LM912","LM911","SMN_05"]',
        JSON.stringify(sceneRoute(spur, 'SMN_05', near)) + ' / ' + JSON.stringify(sceneRoute(spur, near, 'SMN_05')));
})();

console.log('what is not a row');
(function notARow() {
    const pts = { A: { x: 0, y: 0 }, B: { x: 0, y: 0 }, C: { x: 3, y: 4 }, D: { x: 6, y: 8 } };
    const net = sceneNet({
        points: pts,
        edges: [
            { from: 'A', to: 'B' },                         // ends at one place
            { from: 'C', to: 'C' },                         // one point
            { from: 'A', to: 'C', h: [0, 6, 3, 6] },        // a bowed row...
            { from: 'A', to: 'C' },                         // ...and the same way again, straight
            { from: 'C', to: 'D', h: [4, 5, 5, NaN] },      // three handles
            { from: 'C', to: 'NOWHERE' },                   // an end the scene does not place
        ],
    });
    check('rows whose ends coincide are no hop and no lane, but name their vertices',
        net.hop('A', 'B') === null && net.hop('C', 'C') === null && net.names.join(' ') === 'A B C D' && net.lanes.length === 2,
        net.names.join(' ') + ' / ' + net.lanes.length);
    check('of two rows stored the same way, the first is the row', JSON.stringify(net.hop('A', 'C').h) === '[0,6,3,6]' && net.hop('A', 'C').len > 7,
        JSON.stringify(net.hop('A', 'C')));
    check('three handles are no handles: the row is straight', net.hop('C', 'D').h === null && net.hop('C', 'D').len === 5);
    check('an edge to a point the scene does not place is no row', net.names.indexOf('NOWHERE') < 0);
    // Lengths 5 (C>D), 5 (the second A>C) and the bowed A>C's: the median is 5
    // only because the second A>C is counted.
    check('every row that is a row counts toward the scale, the second A>C too: 5', net.scale === 5, String(net.scale));
    check('nearestVertex is by placement over every vertex, the first of equals: (0, 0) is A',
        nearestVertex({ points: pts, edges: [{ from: 'A', to: 'B' }, { from: 'A', to: 'C' }] }, 0.1, 0) === 'A');
    check('no scene is an empty network', sceneNet(null).names.length === 0 && sceneRoute(null, 'A', 'B') === null);
})();

console.log(failures === 0 ? 'PASS' : failures + ' FAILURE(S)');
process.exit(failures === 0 ? 0 : 1);
