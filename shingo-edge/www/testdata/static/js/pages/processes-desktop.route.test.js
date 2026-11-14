// processes-desktop.route.test.js — the desktop process map's route, recorded.
//
// The desktop's travel graph (travelGraph) is DIRECTED and weighs each segment
// by its arc (a cubic's length, not its chord): the rule the shared route model
// adopts, and the rule RDS's own planned path matched point for point on the
// three drives compared on 2026-11-14 (three samples, not a proof of RDS's
// cost function). So its answers are pinned here as the vectors that model must
// reproduce, on
// the scene Core's map and the composer's key-route walk are pinned on too
// (shared/testdata/js/route-scene.json). When the desktop map is removed, this
// file goes with it and the vectors live on in the shared model's own pins.
//
// THE PAGE IS U0's HARNESS (../../../process-setup-harness/page-harness.js),
// unmodified: processes-desktop.js booted for real on process 1, its composer
// read answered with a ComposerMap built from the scene the way
// station_composer.go's composerMap builds it, and the page's own route,
// travelGraph and graphNodeFor called in the module's scope.
//
// Run by www/processes_desktop_route_test.go. Exit 0 on pass.

'use strict';

const fs = require('fs');
const path = require('path');
const assert = require('assert');
const H = require(path.join(__dirname, '..', '..', '..', 'process-setup-harness', 'page-harness.js'));

const SCENE = JSON.parse(fs.readFileSync(path.join(__dirname, '..', '..', '..', '..', '..', '..',
    'shared', 'testdata', 'js', 'route-scene.json'), 'utf8'));

// The ComposerMap the composer read carries (domain.ComposerMap): points by
// name, every stored segment with its chord as `len` (sceneDerived) and its
// handles as `h`.
function composerMap(scene) {
    const m = { revision: 'synthetic', points: {}, edges: [] };
    for (const p of scene.points) m.points[p.name] = { class: p.class, x: p.x, y: p.y };
    for (const e of scene.segments) {
        const a = m.points[e.from], b = m.points[e.to];
        const edge = { from: e.from, to: e.to, len: Math.hypot(b.x - a.x, b.y - a.y) };
        if (e.h) edge.h = e.h.slice();
        m.edges.push(edge);
    }
    return m;
}

let failures = 0;
let checks = 0;
function test(name, fn) {
    try {
        fn();
        checks++;
    } catch (err) {
        failures++;
        console.error('FAIL  ' + name + '\n      ' + (err && err.message));
    }
}

(async function main() {
    const MAP = composerMap(SCENE);
    const page = await H.open({
        page: {
            processes: [{ id: 1, name: 'Press A', group_id: 0, active_style_id: 0, flow_composer_enabled: true }],
            styles: [],
            activeProcessId: 1,
        },
        search: '?process=1',
        hash: '#tab=flows',
        serve: (method, url) => (method === 'GET' && url.split('?')[0] === '/api/processes/1/composer'
            ? { body: { styles: [], routing: [], cell: { positions: [], groups: {} }, map: MAP } } : undefined),
    });
    const route = page.pd('route');
    const graphNodeFor = page.pd('graphNodeFor');
    const travelGraph = page.pd('travelGraph');
    const legs = (a, b) => {
        const r = route(a, b);
        return r ? r.map(e => e.from + '>' + e.to).join(' ') : null;
    };

    test('the page holds the scene\'s map from its composer read', () => {
        assert.strictEqual(page.S.composer.map && page.S.composer.map.revision, 'synthetic');
    });

    test('the travel graph is directed: an edge only where a row is stored', () => {
        const adj = travelGraph();
        const out = n => (adj[n] || []).map(x => x.to).join(' ');
        assert.strictEqual(out('LM931'), 'SMN_05', 'the one-way stretch runs only toward SMN_05');
        assert.strictEqual(out('SMN_05'), 'LM911 LM921', 'no SMN_05 -> LM931');
        assert.strictEqual(out('PLN_02'), 'LM912 LM932 LM921');
    });

    test('a curved segment weighs its arc, not its chord', () => {
        const hop = travelGraph().SMN_05.find(x => x.to === 'LM921');
        assert.ok(hop.len > 10.79 && hop.len < 10.81, 'arc ' + hop.len + ' (chord 8)');
    });

    [
        // [from, to, today's legs, why]
        ['SMN_05', 'PLN_02', 'SMN_05>LM911 LM911>LM912 LM912>PLN_02', 'A: C is one-way the other way, B is longer by arc'],
        ['PLN_02', 'SMN_05', 'PLN_02>LM932 LM932>LM931 LM931>SMN_05', 'C, with its stored direction'],
        ['LM931', 'LM932', 'LM931>SMN_05 SMN_05>LM911 LM911>LM912 LM912>PLN_02 PLN_02>LM932', 'the loop round, not back down C'],
        ['LM921', 'LM911', 'LM921>SMN_05 SMN_05>LM911', 'a curved hop, then a straight one'],
        ['LM911', 'PLN_02', 'LM911>LM912 LM912>PLN_02', 'A'],
        ['LM921', 'LM941', null, 'the island is unreachable'],
        ['LM921', 'LM921', null, 'a node to itself is no route (Core answers [s])'],
    ].forEach(v => {
        test('route ' + v[0] + ' -> ' + v[1] + ': ' + (v[2] === null ? 'none' : v[2]) + ' (' + v[3] + ')', () => {
            assert.strictEqual(legs(v[0], v[1]), v[2]);
        });
    });

    test('graphNodeFor: a node of the graph is itself', () => {
        assert.strictEqual(graphNodeFor('LM921'), 'LM921');
    });
    test('graphNodeFor: a point on no segment is the nearest node with a way out', () => {
        assert.strictEqual(graphNodeFor('BIN_901'), 'PLN_02');
    });
    test('graphNodeFor: a name the map does not have is no node', () => {
        assert.strictEqual(graphNodeFor('NOT_ON_THE_MAP'), '');
        assert.strictEqual(graphNodeFor(''), '');
    });

    if (failures) {
        console.error(failures + ' of ' + (checks + failures) + ' desktop route checks failed');
        process.exit(1);
    }
    console.log(checks + ' desktop route checks passed');
})().catch(err => { console.error(err && err.stack || err); process.exit(1); });
