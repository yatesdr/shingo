// composer-model.test.js — characterization pins for the Flow Composer's pure model.
//
// Run by www/composer_model_characterization_test.go, the same way
// composer-fields.characterization.test.js is run by its own Go harness:
// self-contained, vm.runInContext, no npm, skipped when node is absent.
//
// THE FLOWSPEC IS THE ORACLE, AND IT IS THE DESKTOP'S COPY. The forbidden-field
// pins below walk every (role, mode) row of Steady out of flowspec-data.js —
// the same bytes TestStationFlowspecFileMatchesGo holds to
// flowspec.ExportJSON(). There is
// one flowspec on this tree and this test reads it, rather than a second copy
// that could drift.
//
// The style-7 / style-11 round-trip fixtures are written by the Go harness
// (Collapse() over the plant pull) and handed over as this script's first
// argument, so the expected cells are generated from Go and never hand-written.
// The path is passed rather than fixed beside this file because static/ is
// embedded and served: a generated file dropped in here is one the next build
// ships.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');

const HERE = __dirname;

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

// ── load the model under test ────────────────────────────────────────────────
//
// runInThisContext, NOT createContext: a fresh vm context is a fresh realm, so
// every array the model builds gets that realm's Array.prototype and
// deepStrictEqual — which compares prototypes — fails on identity while
// reporting "same structure". Sharing this realm is what lets the round-trip
// pin compare the model's cells to Go's JSON directly.
function loadModel() {
    const src = fs.readFileSync(path.join(HERE, 'composer-model.js'), 'utf8');
    const factory = vm.runInThisContext(
        '(function(module, exports, console){' + src + '\nreturn module.exports;})',
        { filename: 'composer-model.js' });
    const mod = { exports: {} };
    return factory(mod, mod.exports, console);
}

// ── the flowspec the composer reads, verbatim ────────────────────────────────
//
// window.FLOWSPEC out of the generated file — the same bytes
// TestStationFlowspecFileMatchesGo holds to flowspec.ExportJSON(). There was a
// second copy embedded in the admin page's processes.js; it went with the
// claim editor in U9d, and there is one copy on this tree now.
function loadFlowspec() {
    const src = fs.readFileSync(path.join(HERE, 'flowspec-data.js'), 'utf8');
    const sandbox = { window: {} };
    vm.runInNewContext(src, sandbox, { filename: 'flowspec-data.js' });
    if (!sandbox.window.FLOWSPEC) throw new Error('flowspec-data.js set no window.FLOWSPEC');
    return sandbox.window.FLOWSPEC;
}

const M = loadModel();
const FLOWSPEC = loadFlowspec();
const FIXTURES_PATH = process.argv[2];
if (!FIXTURES_PATH) {
    console.error('usage: node composer-model.test.js <fixtures.json>  (written by composer_model_characterization_test.go)');
    process.exit(2);
}
const FIXTURES = JSON.parse(fs.readFileSync(FIXTURES_PATH, 'utf8'));

// ── the HK press, as the station view carries it ─────────────────────────────
const POSITIONS = [
    { core_node_name: 'PLN_01', kind: 'front', sequence: 1 },
    { core_node_name: 'PLN_02', kind: 'back', sequence: 2 },
    { core_node_name: 'PLN_03', kind: 'front', sequence: 3 },
    { core_node_name: 'PLN_04', kind: 'front', sequence: 4 },
    { core_node_name: 'PLN_05', kind: 'back', sequence: 5 },
    { core_node_name: 'PLN_06', kind: 'front', sequence: 6 },
];
const ROUTING = [
    { core_node_name: 'Supermarket Empty Totes', role: 'source', label: 'Supermarket Empty Totes', enabled: true, sequence: 1 },
    { core_node_name: 'Supermarket Area', role: 'destination', label: 'Supermarket Area', enabled: true, sequence: 2 },
    { core_node_name: 'PLN_02', role: 'staging', label: 'PLN_02', enabled: true, sequence: 3 },
    { core_node_name: 'PLN_05', role: 'staging', label: 'PLN_05', enabled: true, sequence: 4 },
    { core_node_name: 'Retired Lane', role: 'source', label: 'Retired Lane', enabled: false, sequence: 5 },
];

function initStyle(styleId) {
    const f = FIXTURES.styles[String(styleId)];
    return M.init({
        styleId: styleId,
        styleName: f.name,
        positions: POSITIONS,
        routing: ROUTING,
        claims: f.claims,
        parts: f.parts,
        lastRun: f.last_run || null,
        flowspec: FLOWSPEC,
        groups: GROUPS,
        scene: SCENE,
        binWord: 'totes',
        advanced: f.advanced || {},
    });
}

// A travel network with two routes from the supermarket to the press: a short
// one through LM10/LM11/LM12 and a long detour through LM90. The waypoint walk
// has to pick the short one, and has to offer only LM points off it.
const SCENE = {
    edges: [
        { from: 'SMN_05', to: 'LM10', len: 5 },
        { from: 'LM10', to: 'LM11', len: 5 },
        { from: 'LM11', to: 'LM12', len: 5 },
        { from: 'LM12', to: 'PLN_02', len: 5 },
        { from: 'SMN_05', to: 'LM90', len: 40 },
        { from: 'LM90', to: 'PLN_02', len: 40 },
        { from: 'LM12', to: 'PLN_03', len: 5 },
    ],
};

// The routing groups' member nodes, as the station view's cell.groups carries
// them. The dock strip's third line is these, not the press positions.
const GROUPS = {
    'Supermarket Empty Totes': ['SMN_05', 'SMN_06'],
    'Supermarket Area': ['SMN_01', 'SMN_03', 'SMN_04'],
};

const MODES = ['two_robot_press_index', 'two_robot', 'single_robot', 'sequential'];

// ═══ 1. init / round-trip against Go's Collapse ══════════════════════════════

test('init(style 7) puts exactly the claimed positions in the flow', () => {
    const s = initStyle(7);
    const on = Object.keys(s.cells).filter(n => s.cells[n].on).sort();
    assert.deepStrictEqual(on, ['PLN_01', 'PLN_04']);
    assert.strictEqual(s.cells.PLN_01.mode, 'two_robot_press_index');
    assert.strictEqual(s.cells.PLN_01.paired, 'PLN_02');
});

test('toCells(init(style 7)) round-trips to what Go Collapse produced', () => {
    const got = M.toCells(initStyle(7));
    assert.deepStrictEqual(got, FIXTURES.styles['7'].cells);
});

test('toCells(init(style 11)) round-trips to what Go Collapse produced', () => {
    const got = M.toCells(initStyle(11));
    assert.deepStrictEqual(got, FIXTURES.styles['11'].cells);
});

test('toCells emits no back position that is only a partner', () => {
    const cells = M.toCells(initStyle(7));
    const names = cells.map(c => c.core_node_name);
    assert.ok(!names.includes('PLN_02'), 'PLN_02 is a paired back position, not a row');
    assert.ok(!names.includes('PLN_05'), 'PLN_05 is a paired back position, not a row');
});

// ═══ 2. actions ══════════════════════════════════════════════════════════════

test('addPosition on an unused index position adds its structural pair', () => {
    let s = initStyle(7);
    s = M.reduce(s, { type: 'removePosition', node: 'PLN_01' });
    s = M.reduce(s, { type: 'addPosition', node: 'PLN_01' });
    assert.ok(s.cells.PLN_01.on);
    // the pair comes from the prior claim if there is one
    assert.strictEqual(s.cells.PLN_01.paired, 'PLN_02');
});

test('addPosition with no prior claim takes the first free back position in sequence order', () => {
    let s = initStyle(7);
    s = M.reduce(s, { type: 'startBlank' });
    s = M.reduce(s, { type: 'addPosition', node: 'PLN_03' });
    s = M.reduce(s, { type: 'setMode', node: 'PLN_03', mode: 'two_robot_press_index' });
    assert.strictEqual(s.cells.PLN_03.paired, 'PLN_02');
});

test('removePosition clears the partner role, and the partner is not left in the flow', () => {
    let s = initStyle(7);
    s = M.reduce(s, { type: 'removePosition', node: 'PLN_01' });
    assert.strictEqual(s.cells.PLN_01.on, false);
    assert.ok(!s.cells.PLN_02.on);
    const cells = M.toCells(s);
    assert.deepStrictEqual(cells.map(c => c.core_node_name), ['PLN_04']);
});

test('reduce never mutates the state it is given', () => {
    const s = initStyle(7);
    const before = JSON.stringify(s);
    M.reduce(s, { type: 'setPart', node: 'PLN_01', payloadCode: '61477-ATD38.66' });
    assert.strictEqual(JSON.stringify(s), before);
});

test('a part is on at most one position per cell', () => {
    let s = initStyle(7);
    const part = s.cells.PLN_01.part;
    s = M.reduce(s, { type: 'setPart', node: 'PLN_04', payloadCode: part });
    assert.strictEqual(s.cells.PLN_04.part, part);
    assert.strictEqual(s.cells.PLN_01.part, null, 'the part moved, it did not duplicate');
});

test('addPart adds to the palette and never pre-places', () => {
    let s = initStyle(7);
    const before = Object.keys(s.cells).filter(n => s.cells[n].part).length;
    s = M.reduce(s, { type: 'addPart', payloadCode: '55544-DWC33.30' });
    assert.ok(s.parts.includes('55544-DWC33.30'));
    const after = Object.keys(s.cells).filter(n => s.cells[n].part).length;
    assert.strictEqual(after, before, 'a new part is unplaced');
});

test('the dock actions touch every active position and nothing else', () => {
    let s = initStyle(7);
    s = M.reduce(s, { type: 'setDockSource', source: 'Supermarket Area' });
    assert.strictEqual(s.cells.PLN_01.source, 'Supermarket Area');
    assert.strictEqual(s.cells.PLN_04.source, 'Supermarket Area');
    assert.ok(!s.cells.PLN_03.source, 'an inactive position is untouched');
    s = M.reduce(s, { type: 'setDockDest', dest: 'Supermarket Empty Totes' });
    assert.strictEqual(s.cells.PLN_01.dest, 'Supermarket Empty Totes');
    assert.ok(!s.cells.PLN_06.dest);
});

test('every action runs on every mode without throwing', () => {
    for (const mode of MODES) {
        let s = initStyle(7);
        s = M.reduce(s, { type: 'setMode', node: 'PLN_01', mode: mode });
        const acts = [
            { type: 'setPart', node: 'PLN_01', payloadCode: s.parts[0] },
            { type: 'setPartner', node: 'PLN_01', partner: 'PLN_02' },
            { type: 'setStaging', node: 'PLN_01', staging: 'PLN_02' },
            { type: 'setParkOld', node: 'PLN_01', staging: 'PLN_05' },
            { type: 'setSource', node: 'PLN_01', source: 'Supermarket Empty Totes' },
            { type: 'setVia', node: 'PLN_01', via: 'LM314' },
            { type: 'setDest', node: 'PLN_01', dest: 'Supermarket Area' },
        ];
        for (const a of acts) s = M.reduce(s, a);
        assert.ok(s.cells.PLN_01.mode === mode, mode + ': mode survived its own actions');
    }
});

// KEY ROUTE IS AN ORDERED LIST, NOT A WAYPOINT. `key_route` is
// []string on the wire and SEER drives the points in the order they are given
// — `LM167 › LM9` is a different route from `LM9 › LM167`. setVia used to take
// one string and write `[via]`, which silently truncated any route an engineer
// meant to have two points in it, and made the desktop's picker structurally
// incapable of expressing the field it edits.
test('setVia writes the whole ordered list, and order is preserved', () => {
    let s = initStyle(7);
    s = M.reduce(s, { type: 'setVia', node: 'PLN_01', via: ['LM167', 'LM9'] });
    assert.deepStrictEqual(s.cells.PLN_01.keyRoute, ['LM167', 'LM9'], 'two points, in order');
    s = M.reduce(s, { type: 'setVia', node: 'PLN_01', via: ['LM9', 'LM167'] });
    assert.deepStrictEqual(s.cells.PLN_01.keyRoute, ['LM9', 'LM167'], 'the reverse is a different route');
    assert.deepStrictEqual(M.toCells(s).find(c => c.core_node_name === 'PLN_01').key_route, ['LM9', 'LM167'],
        'and it is what toCells sends');
});

test('setVia still takes one string, and clears on an empty one', () => {
    let s = initStyle(7);
    s = M.reduce(s, { type: 'setVia', node: 'PLN_01', via: 'LM314' });
    assert.deepStrictEqual(s.cells.PLN_01.keyRoute, ['LM314'], 'a bare string is a one-point route');
    s = M.reduce(s, { type: 'setVia', node: 'PLN_01', via: [] });
    assert.deepStrictEqual(s.cells.PLN_01.keyRoute, [], 'an empty list clears it');
    s = M.reduce(s, { type: 'setVia', node: 'PLN_01', via: ['LM1'] });
    s = M.reduce(s, { type: 'setVia', node: 'PLN_01', via: '' });
    assert.deepStrictEqual(s.cells.PLN_01.keyRoute, [], 'so does an empty string');
});

test('the route the model holds is a copy, not the callers array', () => {
    let s = initStyle(7);
    const mine = ['LM167', 'LM9'];
    s = M.reduce(s, { type: 'setVia', node: 'PLN_01', via: mine });
    mine.push('LM216');
    assert.deepStrictEqual(s.cells.PLN_01.keyRoute, ['LM167', 'LM9'], 'the model did not alias the caller');
});

// ── the two mode-dependent columns ──────────────────────────────────────────
//
// FLOWSPEC IS THE ORACLE FOR BOTH SURFACES. The desktop draws Partner and
// Staging from rowColumns; the HMI's S5 draws its mode-dependent row from
// ROW_FIELDS. These pin that neither invents a field the spec forbids, and
// that every field S5 writes has a home on the desktop.
test('rowColumns draws exactly the fields flowspec marks used or required', () => {
    for (const mode of MODES) {
        let s = initStyle(7);
        s = M.reduce(s, { type: 'setMode', node: 'PLN_01', mode: mode });
        const role = s.cells.PLN_01.role;
        const row = FLOWSPEC.steady[role][mode];
        for (const column of ['partner', 'staging']) {
            for (const chip of M.rowColumns(s, 'PLN_01', column)) {
                const need = row[chip.field];
                assert.ok(need === 'used' || need === 'required',
                    mode + '/' + column + ': draws ' + chip.field + ', which flowspec marks ' + need);
                assert.strictEqual(chip.required, need === 'required',
                    mode + '/' + chip.field + ': required flag disagrees with flowspec');
                assert.ok(chip.label, mode + '/' + chip.field + ': a chip with no word in front of it');
            }
        }
    }
});

test('every field S5 writes has a chip on the desktop, in one column or the other', () => {
    const ROW_FIELD = M.rowFields();
    for (const mode of MODES) {
        const s5 = ROW_FIELD[mode] || {};
        let s = initStyle(7);
        s = M.reduce(s, { type: 'setMode', node: 'PLN_01', mode: mode });
        const drawn = M.rowColumns(s, 'PLN_01', 'partner').concat(M.rowColumns(s, 'PLN_01', 'staging'))
            .map(c => c.field);
        for (const field of Object.values(s5)) {
            assert.ok(drawn.indexOf(field) >= 0,
                mode + ': the HMI writes ' + field + ' and the desktop has nowhere to put it');
        }
    }
});

test('the press index carries a staging pair and a third position — the five that had no home', () => {
    let s = initStyle(7);
    s = M.reduce(s, { type: 'setMode', node: 'PLN_01', mode: 'two_robot_press_index' });
    const partner = M.rowColumns(s, 'PLN_01', 'partner').map(c => c.field);
    const staging = M.rowColumns(s, 'PLN_01', 'staging').map(c => c.field);
    assert.deepStrictEqual(partner, ['paired_core_node', 'second_paired_core_node']);
    assert.deepStrictEqual(staging, ['inbound_staging', 'outbound_staging']);

    s = M.reduce(s, { type: 'setMode', node: 'PLN_01', mode: 'single_robot' });
    assert.deepStrictEqual(M.rowColumns(s, 'PLN_01', 'partner'), [], 'single_robot pairs with nothing');
    assert.deepStrictEqual(M.rowColumns(s, 'PLN_01', 'staging').map(c => c.field),
        ['inbound_staging', 'outbound_staging'], 'and parks twice');

    s = M.reduce(s, { type: 'setMode', node: 'PLN_01', mode: 'two_robot' });
    assert.deepStrictEqual(M.rowColumns(s, 'PLN_01', 'partner'), [], 'a two-robot swap stages, it does not pair');
    assert.deepStrictEqual(M.rowColumns(s, 'PLN_01', 'staging').map(c => c.field), ['inbound_staging']);
});

// A CHIP IS A CONTROL, AND A CONTROL WITH NOTHING BEHIND IT IS NOISE. flowspec
// says which fields a choreography HAS; it cannot say whether this press has
// anywhere to put them. `secondPaired` is the only one where the two differ:
// Press 400 has two back positions and both are already somebody's pair, so
// there is no third position to choose and the desktop draws no `Third` chip.
test('the third position is possible only when the press has a free back slot', () => {
    let s = initStyle(7);
    s = M.reduce(s, { type: 'setMode', node: 'PLN_01', mode: 'two_robot_press_index' });
    const third = () => M.rowColumns(s, 'PLN_01', 'partner').find(c => c.key === 'secondPaired');

    // Style 7 pairs PLN_01→PLN_02 and PLN_04→PLN_05: both backs taken.
    assert.ok(third(), 'flowspec still says a press index HAS the field');
    assert.strictEqual(third().possible, false, 'and this press has nowhere to put it');

    // Free one up by dropping the other pair's cell, and it becomes possible.
    s = M.reduce(s, { type: 'removePosition', node: 'PLN_04' });
    assert.strictEqual(third().possible, true, 'a back position came free');

    // A value always counts as possible, whatever the press looks like now.
    s = M.reduce(s, { type: 'setSecondPartner', node: 'PLN_01', partner: 'PLN_05' });
    assert.strictEqual(third().possible, true);
    assert.strictEqual(third().value, 'PLN_05');
});

test('every other field is possible whether or not it is set', () => {
    for (const mode of MODES) {
        let s = initStyle(7);
        s = M.reduce(s, { type: 'setMode', node: 'PLN_01', mode: mode });
        for (const column of ['partner', 'staging']) {
            for (const chip of M.rowColumns(s, 'PLN_01', column)) {
                if (chip.key === 'secondPaired') continue;
                assert.strictEqual(chip.possible, true,
                    mode + '/' + chip.field + ': a routing name or a press position is always choosable');
            }
        }
    }
});

// ═══ 3. flowspec: a forbidden field is never present ═════════════════════════

test('changing mode clears every field Steady marks Forbidden — every mode, every field', () => {
    const ROW_FIELD = M.rowFields();   // {mode: {rowName: cellField}} — the §3.3 mapping
    for (const mode of MODES) {
        let s = initStyle(7);
        // fill everything the composer can author, then switch mode
        s = M.reduce(s, { type: 'setPartner', node: 'PLN_01', partner: 'PLN_02' });
        s = M.reduce(s, { type: 'setStaging', node: 'PLN_01', staging: 'PLN_02' });
        s = M.reduce(s, { type: 'setParkOld', node: 'PLN_01', staging: 'PLN_05' });
        s = M.reduce(s, { type: 'setMode', node: 'PLN_01', mode: mode });
        const cell = M.toCells(s).find(c => c.core_node_name === 'PLN_01');
        const role = cell.role;
        const row = FLOWSPEC.steady[role] && FLOWSPEC.steady[role][mode];
        assert.ok(row, 'flowspec has a Steady row for ' + role + '/' + mode);
        for (const field of Object.keys(row)) {
            if (row[field] !== 'forbidden') continue;
            const v = cell[field];
            const empty = v === '' || v === null || v === undefined ||
                (Array.isArray(v) && v.length === 0);
            assert.ok(empty, mode + ': ' + field + ' is forbidden but the cell carries ' + JSON.stringify(v));
        }
        assert.ok(ROW_FIELD[mode], 'rowFields() names the rows for ' + mode);
    }
});

// ═══ 4. derived views ════════════════════════════════════════════════════════

test('dockNotes(style 7) reads exactly as the spec S4 sentence', () => {
    const d = M.dockNotes(initStyle(7));
    // F3 (2026-09-12): the note names the FIELD, and the positions it drives.
    // It read `Robot 1 brings new stock here → …`, which is a sentence about
    // `inbound_source` that never says which field it means.
    assert.strictEqual(d.in.note, 'Inbound source · Robot 1 → PLN_02, PLN_05');
    assert.strictEqual(d.out.note, 'Outbound destination · Robot 2 ← PLN_01, PLN_04');
    assert.strictEqual(d.in.group, 'Supermarket Empty Totes');
    assert.strictEqual(d.out.group, 'Supermarket Area');
});

// The third line under the group name is the GROUP'S MEMBER NODES, which is
// what the read-only picture draws (operator-flow.js dockNotes: inMembers =
// groups[srcs[0]]). It used to be the positions — the same names the note
// above it already carries — so the strip said where the bins go twice and
// never said where they come from.
test('dockNotes members are the routing group nodes, not the positions', () => {
    const d = M.dockNotes(initStyle(7));
    // Against the GROUPS map, not against literals: this is the same rule
    // operator-flow.js's dockNotes applies (inMembers = groups[srcs[0]]), and
    // stating it that way is what makes the two drawings agree by construction.
    assert.deepStrictEqual(d.in.members, GROUPS['Supermarket Empty Totes']);
    assert.deepStrictEqual(d.out.members, GROUPS['Supermarket Area']);
    // the positions are still available, under their own name
    assert.deepStrictEqual(d.in.targets, ['PLN_02', 'PLN_05']);
    assert.deepStrictEqual(d.out.targets, ['PLN_01', 'PLN_04']);
});

// THE BIN WORD IS GONE (owner ruling R2, 2026-09-12), and with it the test
// that pinned it — `the bin word is the TARGET style s, and defaults to bins
// when unknown`, which asserted that the composer says the word of the style
// it is changing TO while the picture behind it says the running one's. The
// rule was right; the word it was about is no longer on any surface, and
// state.binWord, M.binWord and M.oneBinWord went with it.
//
// What replaces it is the pin below: no surface the model writes carries one.
// What replaces it is the pin below, stated as the PROPERTY rather than as a
// word hunt: `old bins to` is still a column heading on D1 and in the compare
// table's field words, and a test that banned the substring would be banning
// the headings this round left alone. The thing R2 removed is a word DERIVED
// FROM THE STYLE'S PAYLOAD, so that is what this holds.
test('nothing the model writes is derived from what a part travels in', () => {
    const f = FIXTURES.styles['11'];
    const mk = opts => M.init(Object.assign({
        styleId: 11, styleName: f.name, positions: POSITIONS, routing: ROUTING,
        claims: f.claims, parts: f.parts, flowspec: FLOWSPEC, groups: GROUPS,
    }, opts));
    const written = s => [
        M.dockNotes(s).in.note, M.dockNotes(s).out.note,
        M.bar(s).heading, M.bar(s).detail,
        ...M.cardLines(s, 'PLN_03'),
    ].join(' | ');
    // The option the composer used to be handed is ignored, whatever it says.
    assert.strictEqual(written(mk({ binWord: 'totes' })), written(mk({})));
    assert.strictEqual(written(mk({ binWord: 'racks' })), written(mk({})));
    // And the two functions that answered for it are gone, not renamed.
    assert.strictEqual(typeof M.binWord, 'undefined');
    assert.strictEqual(typeof M.oneBinWord, 'undefined');
    // The one word that could only have come from a payload catalog is absent
    // from the sentences that carried it. Checked over those and not over the
    // bar's detail, which names ROUTING GROUPS — `Supermarket Empty Totes` is
    // a lane's name at Hopkinsville, not a word for what a bin is.
    const t = mk({});
    const carried = [M.dockNotes(t).in.note, M.dockNotes(t).out.note, ...M.cardLines(t, 'PLN_03')].join(' | ');
    assert.ok(carried.toLowerCase().indexOf('tote') < 0, carried);
});

test('cardLines are the reference CARDLINE, verbatim', () => {
    const s = initStyle(7);
    assert.deepStrictEqual(M.cardLines(s, 'PLN_01'),
        ['Robot 1 supplies PLN_02', 'Robot 2 indexes']);
    const s11 = initStyle(11);
    assert.deepStrictEqual(M.cardLines(s11, 'PLN_03'),
        ['Robot 1 stages at PLN_02', 'Robot 2 pulls old']);
});

test('legs(style 7) is one Robot 2 index leg per paired position', () => {
    const l = M.legs(initStyle(7));
    assert.strictEqual(l.length, 2);
    assert.ok(l.every(x => x.robot === 2 && x.kind === 'index'));
    assert.deepStrictEqual(l.map(x => x.to).sort(), ['PLN_01', 'PLN_04']);
});

test('legs(style 11) is one Robot 1 move leg per staged position', () => {
    const l = M.legs(initStyle(11));
    assert.strictEqual(l.length, 2);
    assert.ok(l.every(x => x.robot === 1 && x.kind === 'move'));
});

// ═══ 5. the bar ══════════════════════════════════════════════════════════════

test('bar: nothing in the flow', () => {
    let s = initStyle(7);
    s = M.reduce(s, { type: 'startBlank' });
    const b = M.bar(s);
    assert.strictEqual(b.heading, 'Nothing in the flow yet');
    assert.strictEqual(b.detail, 'Tap a position to add it, or pick a flow above');
    assert.strictEqual(b.tone, 'empty');
    assert.strictEqual(b.button.enabled, false);
});

test('bar: preview OK', () => {
    let s = initStyle(7);
    s = M.applyPreview(s, {
        order_count: 4, findings: [], unresolved: [],
        preflight: { state: 'ok', missing: [] }, fingerprint: 'abc', actions: [],
    });
    const b = M.bar(s);
    assert.strictEqual(b.heading, 'Preview OK · 4 orders');
    assert.strictEqual(b.tone, 'ok');
    assert.strictEqual(b.button.label, 'Save and start');
    assert.strictEqual(b.button.enabled, true);
});

// TWO THINGS, and the second one is the point. Clearing PLN_01's swap mode
// takes it out of the DRAFT — toCells skips a cell with no choreography — so
// the part that was on it has nowhere to go, and the server says so
// (domain.ValidateFlowPartsPlaced reads the draft). The model used to count a
// part on an inactive cell as placed, so the bar read `1 thing to fix` and
// then the preview came back with a finding the bar had not counted.
test('bar: blocked names the findings and disables the button', () => {
    let s = initStyle(7);
    s = M.reduce(s, { type: 'setMode', node: 'PLN_01', mode: null });
    const b = M.bar(s);
    assert.strictEqual(b.heading, '2 things to fix before you can start');
    assert.strictEqual(b.tone, 'blocked');
    assert.strictEqual(b.button.enabled, false);
    assert.strictEqual(b.button.label, 'Fix the flow to continue');
    const fields = M.findings(s).map(f => f.field).sort();
    assert.deepStrictEqual(fields, ['swap_mode', 'unplaced_part'],
        'a position with no choreography, and the part it was carrying');
});

// F1. The seam answers 400 with the same body when a flow plans nothing, and
// with `error` alone when it refuses outright (Core unavailable for a
// press-index bin type is the one the shots hit). Neither is an OK preview.
test('bar: a preview that fires no orders is blocked, not OK', () => {
    let s = initStyle(7);
    s = M.applyPreview(s, {
        order_count: 0, findings: [], unresolved: [],
        preflight: { state: 'ok', missing: [] }, fingerprint: 'abc', actions: [],
    });
    const b = M.bar(s);
    assert.strictEqual(b.tone, 'blocked');
    assert.strictEqual(b.button.enabled, false);
    assert.strictEqual(b.button.label, 'Fix the flow to continue');
});

test('bar: a 400 with only an error shows the server sentence verbatim', () => {
    let s = initStyle(7);
    const err = 'changeover refused: Core unavailable; cannot determine bin types for press-index changeover at PLN_01';
    s = M.applyPreview(s, { error: err });
    const b = M.bar(s);
    assert.strictEqual(b.heading, err);
    assert.strictEqual(b.tone, 'blocked');
    assert.strictEqual(b.button.enabled, false);
});

test('bar: Core unreachable says so in the detail', () => {
    let s = initStyle(7);
    s = M.applyPreview(s, {
        order_count: 4, findings: [], unresolved: [],
        preflight: { state: 'unchecked', missing: [] }, fingerprint: 'abc', actions: [],
    });
    assert.strictEqual(M.bar(s).detail, 'Inventory not checked — Core is unreachable');
});

// ═══ 6. findings ═════════════════════════════════════════════════════════════

test('findingShort covers the four spec short texts', () => {
    assert.strictEqual(M.findingShort({ field: 'swap_mode', message: 'x' }), 'how does it swap?');
    assert.strictEqual(M.findingShort({ field: 'payload_code', message: 'x' }), 'which part?');
    // F3: a pill names its FIELD. `nowhere to send old bin` was a sentence
    // about outbound_destination that never said which field it meant, and a
    // pill an operator cannot map to a control is one they cannot act on.
    assert.strictEqual(M.findingShort({ field: 'inbound_staging', message: 'x' }), 'no inbound staging');
    assert.strictEqual(M.findingShort({ field: 'outbound_destination', message: 'x' }), 'no outbound destination');
});

test('findingShort falls back to the server message, never blank', () => {
    const s = M.findingShort({ field: 'something_new', message: 'the server said this' });
    assert.strictEqual(s, 'the server said this');
    assert.ok(M.findingShort({ field: 'x', message: '' }).length > 0, 'never blank');
});

// THREE LOCAL FINDINGS since owner ruling R8 (2026-09-12): the two positions
// the server cannot raise before a preview, and a part with no position, which
// this draft earns twice over — PLN_01 loses its choreography and PLN_04 loses
// its part, so both of style 7's parts end up on nothing.
test('the client raises the three local findings', () => {
    let s = initStyle(7);
    s = M.reduce(s, { type: 'setMode', node: 'PLN_01', mode: null });
    s = M.reduce(s, { type: 'setPart', node: 'PLN_04', payloadCode: null });
    const local = M.findings(s).filter(f => f.local);
    assert.deepStrictEqual(local.map(f => f.short).sort(),
        ['how does it swap?', 'needs a position', 'which part?']);
});

// R8: a part with no position, in one finding naming them all, and NO node —
// "which position?" is the question it is asking.
// THE TWO SURFACES HAND applyPreset TWO SHAPES, and it has to take both.
//
// The station's strip card carries ComposerPreset.Cells — a MAP keyed by
// position. The desktop's apply modal carries PresetRow.Shape — the server's
// ARRAY of FlowCells. `built['PLN_01']` on an array is undefined, so under the
// array every cell fell through to blank and the apply wrote an EMPTY flow:
// `Save to 1 part` would have deleted every claim of the part it was applied
// to, under a diff that said `PLN_01 · added to the flow` — because shapeDiff
// normalises through presetCells and the reducer did not.
test('applyPreset takes the array and the map, and lands the same flow', () => {
    const base = initStyle(7);
    const asMap = { cells: { PLN_03: { swap_mode: 'two_robot', inbound_staging: 'PLN_02',
        inbound_source: 'Supermarket Empty Totes', outbound_destination: 'Supermarket Area' } } };
    const asArray = { cells: [{ core_node_name: 'PLN_03', swap_mode: 'two_robot', inbound_staging: 'PLN_02',
        inbound_source: 'Supermarket Empty Totes', outbound_destination: 'Supermarket Area' }] };
    const fromMap = M.reduce(base, { type: 'applyPreset', preset: asMap });
    const fromArray = M.reduce(base, { type: 'applyPreset', preset: asArray });
    assert.deepStrictEqual(M.toCells(fromArray), M.toCells(fromMap));
    // AND IT IS NOT EMPTY. The failure being guarded is "every cell went
    // blank", which two empty flows would compare equal on.
    assert.strictEqual(M.toCells(fromArray).length, 1, 'the applied flow is empty');
    assert.strictEqual(M.toCells(fromArray)[0].core_node_name, 'PLN_03');
    // The diff and the apply read the same cells: the diff says a position
    // arrives, so the applied flow has it.
    for (const d of M.shapeDiff(base, asArray)) {
        if (d.label === 'added to the flow') {
            assert.ok(M.toCells(fromArray).some(c => c.core_node_name === d.node),
                d.node + ' is in the diff and not in the flow the apply built');
        }
    }
});

// F1 (owner ruling, 2026-09-12): the apply modal finishes the job itself, and
// the way it does is `applyPreset` then `setPart` on the row's own model. That
// only works if the pair lands the same cell a hand-built one does — the modal
// would otherwise be writing a flow the composer cannot make.
test('applyPreset then setPart equals a hand-placed cell', () => {
    const shape = [{ core_node_name: 'PLN_03', swap_mode: 'two_robot', inbound_staging: 'PLN_02',
        inbound_source: 'Supermarket Empty Totes', outbound_destination: 'Supermarket Area' }];
    const part = FIXTURES.styles['7'].parts[0];
    const code = part.payload_code || part;

    // the modal's way: apply the shape, then answer its one question
    let viaApply = M.reduce(initStyle(7), { type: 'applyPreset', preset: { cells: shape } });
    assert.ok(M.toCells(viaApply).every(c => !c.payload_code), 'the shape arrived carrying a part');
    viaApply = M.reduce(viaApply, { type: 'setPart', node: 'PLN_03', payloadCode: code });

    // the composer's way: build the same cell by hand from blank
    let byHand = M.reduce(initStyle(7), { type: 'startBlank' });
    byHand = M.reduce(byHand, { type: 'addPosition', node: 'PLN_03' });
    byHand = M.reduce(byHand, { type: 'setMode', node: 'PLN_03', mode: 'two_robot' });
    byHand = M.reduce(byHand, { type: 'setStaging', node: 'PLN_03', staging: 'PLN_02' });
    byHand = M.reduce(byHand, { type: 'setSource', node: 'PLN_03', source: 'Supermarket Empty Totes' });
    byHand = M.reduce(byHand, { type: 'setDest', node: 'PLN_03', dest: 'Supermarket Area' });
    byHand = M.reduce(byHand, { type: 'setPart', node: 'PLN_03', payloadCode: code });

    assert.deepStrictEqual(M.toCells(viaApply), M.toCells(byHand));
    // AND THE PICK IS IN THE WIRE'S OWN FIELD. `payload_code` is what the save
    // sends and what the server validates; a pick that landed anywhere else
    // would preview clean and save refused.
    const cell = M.toCells(viaApply).find(c => c.core_node_name === 'PLN_03');
    assert.strictEqual(cell.payload_code, code);
    // The flow is clean now: nothing left for the modal to ask.
    assert.strictEqual(M.toCells(viaApply).filter(c => !c.payload_code).length, 0);
});

test('a part with no position is one finding, naming every loose part', () => {
    let s = initStyle(7);
    s = M.reduce(s, { type: 'setPart', node: 'PLN_01', payloadCode: null });
    s = M.reduce(s, { type: 'setPart', node: 'PLN_04', payloadCode: null });
    const f = M.findings(s).find(x => x.field === 'unplaced_part');
    assert.ok(f, 'no unplaced-part finding');
    assert.strictEqual(f.node, '');
    assert.strictEqual(f.message, '2 parts need a position');
    assert.strictEqual(f.parts.length, 2);
    // One, and singular.
    let one = M.reduce(initStyle(7), { type: 'setPart', node: 'PLN_01', payloadCode: null });
    assert.strictEqual(M.findings(one).find(x => x.field === 'unplaced_part').message,
        '1 part needs a position');
    // AND NO ONE-TAP FIX: every fix-it writes to state.cells[node], and this
    // finding deliberately names no position.
    assert.strictEqual(M.fixItFor(s, f), null);
});

// A BLANK FLOW LEAVES NOTHING UNPLACED. Every part is loose by construction
// before a position is tapped, and `2 parts need a position` over an empty
// stage is the screen scolding an operator for not having started.
test('the blank state raises no unplaced-part finding', () => {
    const s = M.reduce(initStyle(7), { type: 'startBlank' });
    assert.deepStrictEqual(M.findings(s), []);
    assert.strictEqual(M.bar(s).heading, 'Nothing in the flow yet');
});

// R8 again, from the other side: the server raises the same fact from the
// STORED rows, and one problem may only produce one row.
test('the server copy of the unplaced-part finding is not counted twice', () => {
    let s = M.reduce(initStyle(7), { type: 'setPart', node: 'PLN_01', payloadCode: null });
    s = M.applyPreview(s, {
        order_count: 2, unresolved: [], actions: [], fingerprint: 'fp',
        preflight: { state: 'ok', missing: [] },
        findings: [{ core_node_name: '', side: 'to', field: 'payload_code', severity: 'error',
            message: '1 part needs a position: 55544-DWC33.21' }],
    });
    const loose = M.findings(s).filter(f => f.field === 'unplaced_part' ||
        (!f.node && f.field === 'payload_code'));
    assert.strictEqual(loose.length, 1, 'one problem, two authors, two rows: ' + JSON.stringify(loose));
});

test('applyPreview carries the server findings through unchanged', () => {
    let s = initStyle(7);
    const f = { core_node_name: 'PLN_04', side: 'to', field: 'outbound_destination', severity: 'error', message: 'no destination' };
    s = M.applyPreview(s, {
        order_count: 0, findings: [f], unresolved: [],
        preflight: { state: 'ok', missing: [] }, fingerprint: 'abc', actions: [],
    });
    const got = M.findings(s).find(x => !x.local);
    assert.strictEqual(got.message, 'no destination');
    assert.strictEqual(got.short, 'no outbound destination');
});

// ═══ 7. the vocabulary rule ══════════════════════════════════════════════════

// SPEC §0.7. Asserted over what the model RETURNS rather than over its source:
// the source carries CSS token names (--os-r1) and prose, and a regex over
// literals reports those as violations while missing a string the model builds
// by concatenation. Everything an operator can read comes out of these calls.
test('no operator-visible string says R1 or R2', () => {
    const s7 = initStyle(7), s11 = initStyle(11);
    const said = [];
    for (const s of [s7, s11]) {
        const d = M.dockNotes(s);
        said.push(d.in.note, d.out.note, d.in.group, d.out.group);
        for (const p of POSITIONS) said.push(...M.cardLines(s, p.core_node_name));
        for (const l of M.legs(s)) said.push(l.label);
        const b = M.bar(s);
        said.push(b.heading, b.detail, b.button.label, b.fixIt && b.fixIt.label);
        for (const f of M.findings(s)) said.push(f.short, f.message, f.detail);
    }
    said.push(...Object.values(M.modeLabels()), ...Object.values(M.modeHelp()));
    for (const lit of said.filter(Boolean)) {
        assert.ok(!/\bR[12]\b/.test(lit), 'operator-visible string says R1/R2: ' + lit);
    }
});

// Owner ruling 2026-09-10 (§9.3). The default is the FLOOR, not the common case:
// a style with no claims reads the press, not a constant.
test('role: a style with no claims of its own reads the process, not consume', () => {
    const seven = FIXTURES.styles['7'].claims;   // produce claims on this press
    const blank = M.init({
        styleId: 999, styleName: 'never run here', positions: POSITIONS, routing: ROUTING,
        claims: [], parts: [], flowspec: FLOWSPEC, groups: GROUPS,
        processClaims: seven,
    });
    assert.strictEqual(blank.cells.PLN_01.role, 'produce',
        'the press produces under every other style; a blank style says so too');
    assert.strictEqual(blank.roleSources.PLN_01, "majority of the process's other styles");
    // and with nothing to read anywhere, consume is still the floor
    const nothing = M.init({
        styleId: 998, styleName: 'empty process', positions: POSITIONS, routing: ROUTING,
        claims: [], parts: [], flowspec: FLOWSPEC, groups: GROUPS, processClaims: [],
    });
    assert.strictEqual(nothing.cells.PLN_01.role, 'consume');
    assert.strictEqual(nothing.roleSources.PLN_01, 'default');
});

test('role: the style s own claim still wins over the process', () => {
    const s = initStyle(7);
    assert.strictEqual(s.cells.PLN_01.role, 'produce');
    assert.strictEqual(s.roleSources.PLN_01, 'prior claim');
});

// ═══ 9. Advanced (U9b) ═══════════════════════════════════════════════════════
//
// The wire half of these is pinned in Go (domain/flow_advanced_test.go). What
// is here is the half only the model can answer: that a draft says nothing
// until Apply, that Apply says everything, and that "N set" counts what
// domain.ClaimHas counts.

test('Advanced is absent from every cell until Apply', () => {
    const s = initStyle(7);
    for (const cell of M.toCells(s)) {
        assert.ok(!('advanced' in cell),
            cell.core_node_name + ' spoke advanced with the modal never opened');
    }
});

test('the modal opens on the stored claim, and on the defaults for a position with none', () => {
    const s = initStyle(7);
    // The fixture is Go's own AdvancedOf, so this is the wire's answer with the
    // one translation the wire needs: Go writes an empty list as null and the
    // model reads null as the default.
    const stored = FIXTURES.styles['7'].advanced.PLN_01;
    const want = M.advancedDefaults();
    for (const k of Object.keys(want)) if (stored[k] !== null && stored[k] !== undefined) want[k] = stored[k];
    assert.deepStrictEqual(M.advancedFor(s, 'PLN_01'), want);

    // PLN_06 is free on style 7 — no claim, so no stored block.
    const added = M.reduce(s, { type: 'addPosition', node: 'PLN_06', mode: 'two_robot' });
    assert.deepStrictEqual(M.advancedFor(added, 'PLN_06'), M.advancedDefaults());
});

test('a null list on the wire reads as the empty default, not as null', () => {
    // Go marshals an empty []string as null. A merge that lets null through
    // puts it on the object the modal maps over, and the sheet does not open.
    const s = M.init({
        styleId: 1, styleName: 'x', positions: POSITIONS, routing: ROUTING,
        claims: [], parts: [], flowspec: FLOWSPEC, groups: GROUPS,
        advanced: { PLN_01: { allowed_payload_codes: null, reorder_point: 5 } },
    });
    const a = M.advancedFor(s, 'PLN_01');
    assert.deepStrictEqual(a.allowed_payload_codes, []);
    assert.strictEqual(a.reorder_point, 5);
    assert.strictEqual(a.changeover_carryover_disposition, 'replace');
});

test('Apply lands on exactly one cell and travels to the wire', () => {
    const s = initStyle(7);
    const adv = Object.assign(M.advancedFor(s, 'PLN_01'), { reorder_point: 12, reorder_point_source: 'manual' });
    const after = M.reduce(s, { type: 'setAdvanced', node: 'PLN_01', advanced: adv });

    const cells = M.toCells(after);
    const one = cells.find(c => c.core_node_name === 'PLN_01');
    assert.strictEqual(one.advanced.reorder_point, 12);
    assert.strictEqual(one.advanced.reorder_point_source, 'manual');
    for (const c of cells) {
        if (c.core_node_name === 'PLN_01') continue;
        assert.ok(!('advanced' in c), c.core_node_name + ' was dragged into another position s Apply');
    }
    // And the draft the modal edited is a copy: mutating it after Apply does
    // not reach the state.
    adv.reorder_point = 999;
    assert.strictEqual(M.toCells(after).find(c => c.core_node_name === 'PLN_01').advanced.reorder_point, 12);
});

test('Apply with null puts the position back on the carry-through', () => {
    const s = initStyle(7);
    const set = M.reduce(s, { type: 'setAdvanced', node: 'PLN_01', advanced: { reorder_point: 12 } });
    const cleared = M.reduce(set, { type: 'setAdvanced', node: 'PLN_01', advanced: null });
    assert.ok(!('advanced' in M.toCells(cleared).find(c => c.core_node_name === 'PLN_01')));
});

test('changing mode clears a Forbidden advanced field and leaves an Unused one alone', () => {
    const s = initStyle(7);
    // index_robot_supplies is press-index's alone; lineside_soft_threshold is
    // Unused on produce, not Forbidden, and flowspec.go says why that matters.
    // (keep_staged was this example until it came off the wire — it is Unused
    // in every mode and withheld at every door, so it is no longer a field the
    // block carries.)
    const adv = Object.assign(M.advancedFor(s, 'PLN_01'), { index_robot_supplies: true, lineside_soft_threshold: 7 });
    let after = M.reduce(s, { type: 'setAdvanced', node: 'PLN_01', advanced: adv });
    assert.strictEqual(M.advancedFor(after, 'PLN_01').index_robot_supplies, true);

    after = M.reduce(after, { type: 'setMode', node: 'PLN_01', mode: 'sequential' });
    const now = M.advancedFor(after, 'PLN_01');
    assert.strictEqual(now.index_robot_supplies, false, 'a Forbidden field survived the mode change');
    assert.strictEqual(now.lineside_soft_threshold, 7, 'an Unused field was wiped by a UI decision');
});

test('the modal draws a field only when flowspec neither forbids it nor ignores it', () => {
    const s = initStyle(7);   // PLN_01 is two_robot_press_index / produce
    assert.strictEqual(M.advancedShows(s, 'PLN_01', 'index_robot_supplies'), true);
    assert.strictEqual(M.advancedShows(s, 'PLN_01', 'lineside_soft_threshold'), false, 'produce has no lineside cap');
    assert.strictEqual(M.advancedShows(s, 'PLN_01', 'auto_push'), false, 'auto_push is the unloader s');

    const seq = M.reduce(s, { type: 'setMode', node: 'PLN_01', mode: 'sequential' });
    assert.strictEqual(M.advancedShows(seq, 'PLN_01', 'index_robot_supplies'), false);
    assert.strictEqual(M.advancedShows(seq, 'PLN_01', 'lineside_soft_threshold'), false);
});

test('"N set" counts what ClaimHas counts, and nothing flowspec hides', () => {
    const s = initStyle(7);
    const base = M.advancedDefaults();
    assert.deepStrictEqual(M.advancedSet(s, 'PLN_01', base), []);

    // replace is the absence of an opinion; the other two are opinions.
    assert.deepStrictEqual(
        M.advancedSet(s, 'PLN_01', Object.assign({}, base, { changeover_carryover_disposition: 'replace' })), []);
    assert.deepStrictEqual(
        M.advancedSet(s, 'PLN_01', Object.assign({}, base, { changeover_carryover_disposition: 'keep_lineside' })),
        ['changeover_carryover_disposition']);

    // reorder_point_source is a stamp on the number, never a value of its own.
    assert.deepStrictEqual(
        M.advancedSet(s, 'PLN_01', Object.assign({}, base, { reorder_point_source: 'manual' })), []);
    assert.deepStrictEqual(
        M.advancedSet(s, 'PLN_01', Object.assign({}, base, { reorder_point: 40, reorder_point_source: 'manual' })),
        ['reorder_point']);

    // A value in a field this choreography hides does not put a badge on the row.
    assert.deepStrictEqual(
        M.advancedSet(s, 'PLN_01', Object.assign({}, base, { auto_push: true })), []);
    assert.deepStrictEqual(
        M.advancedSet(s, 'PLN_01', Object.assign({}, base, { allowed_payload_codes: ['PIA27'] })),
        ['allowed_payload_codes']);
});

test('Advanced never mutates the state it is given', () => {
    const s = initStyle(7);
    const before = JSON.stringify(s);
    M.reduce(s, { type: 'setAdvanced', node: 'PLN_01', advanced: { reorder_point: 12 } });
    M.advancedFor(s, 'PLN_01').reorder_point = 999;
    M.advancedSet(s, 'PLN_01');
    assert.strictEqual(JSON.stringify(s), before);
});

// ═══ 8. presets (U10) ════════════════════════════════════════════════════════
//
// A PRESET IS A SHAPE, NEVER A PART (SYNTH R-L1). Both surfaces apply one
// through this reducer and no other path, so these are the pins for both: the
// HMI's strip card and the desktop's apply modal are the same action.

// presetFromStyle builds what a preset carries — the cells of a flow with the
// payload stripped, keyed by position, exactly as the server sends them
// (ComposerPreset.Cells / PresetRow.Shape).
function presetFromStyle(styleId) {
    const cells = {};
    for (const c of FIXTURES.styles[String(styleId)].claims) {
        cells[c.core_node_name] = {
            mode: c.swap_mode, source: c.inbound_source || '', dest: c.outbound_destination || '',
            staging: c.inbound_staging || '', parkOld: c.outbound_staging || '',
            paired: c.paired_core_node || '', secondPaired: c.second_paired_core_node || '',
            evacDest: c.changeover_evac_destination || '',
            evacNodes: (c.changeover_evac_nodes || []).slice(),
            keyRoute: (c.key_route || []).slice(),
        };
    }
    return { key: '1', name: 'Two up, index', version: 1, cells: cells };
}

// THE CASE THIS PIN EXISTS FOR: re-applying a preset to a style that drifted
// from it. Same positions, a corrected shape — and the parts must not move,
// because the preset has no opinion about them. Before U10 every one of them
// was wiped, silently, with no undo.
test('applyPreset keeps the parts the operator placed — a preset carries no part', () => {
    const s = initStyle(11);
    const before = {};
    for (const n of Object.keys(s.cells)) if (s.cells[n].part) before[n] = s.cells[n].part;
    assert.ok(Object.keys(before).length > 0, 'fixture has no placed part — the pin would prove nothing');

    // Style 11's own shape, with one field changed: what a drifted member
    // looks like when the engineer re-applies the preset it came from.
    const preset = presetFromStyle(11);
    const first = Object.keys(preset.cells)[0];
    preset.cells[first] = Object.assign({}, preset.cells[first], { dest: 'Supermarket Area' });

    const after = M.reduce(s, { type: 'applyPreset', preset: preset });
    const kept = {};
    for (const n of Object.keys(after.cells)) if (after.cells[n].part) kept[n] = after.cells[n].part;
    assert.deepStrictEqual(kept, before);
    assert.strictEqual(after.cells[first].dest, 'Supermarket Area', 'the shape did not land');
});

test('applyPreset drops none of the shape it was given', () => {
    const s = initStyle(11);
    const preset = presetFromStyle(7);
    const after = M.reduce(s, { type: 'applyPreset', preset: preset });
    for (const n of Object.keys(preset.cells)) {
        const want = preset.cells[n], got = after.cells[n];
        assert.ok(got && got.on, n + ' is not on after the apply');
        for (const k of ['mode', 'source', 'dest', 'staging', 'parkOld', 'paired', 'secondPaired']) {
            assert.strictEqual(got[k], want[k], n + ' · ' + k);
        }
    }
});

test('a position the preset does not name is cleared, not left on the old flow', () => {
    const s = initStyle(11);
    const preset = presetFromStyle(7);
    const after = M.reduce(s, { type: 'applyPreset', preset: preset });
    for (const n of Object.keys(after.cells)) {
        if (preset.cells[n]) continue;
        assert.strictEqual(after.cells[n].on, false, n + ' survived an apply that never named it');
        assert.strictEqual(after.cells[n].mode, null, n + ' kept its mode');
        // AND IT RELEASES ITS PART. The strip reads "placed" off any cell
        // carrying the part, off ones included (composer-render.js:382), so a
        // part left on a dropped position would show as placed while being
        // nowhere in the flow. Released, it goes amber and says so.
        assert.strictEqual(after.cells[n].part, null,
            n + ' kept a part on a position the new shape dropped — the strip will call it placed');
    }
});

test('a part whose position the new shape drops becomes unplaced, not silently placed', () => {
    const s = initStyle(11);
    // A shape that names ONE of style 11 s positions: every part on the others
    // has to come loose.
    const full = presetFromStyle(11);
    const one = Object.keys(full.cells)[0];
    const preset = { key: '9', name: 'One position', version: 1, cells: { [one]: full.cells[one] } };

    const partsBefore = (s.parts || []).filter(p => Object.keys(s.cells).some(n => s.cells[n].part === p));
    const after = M.reduce(s, { type: 'applyPreset', preset: preset });
    const partsAfter = (after.parts || []).filter(p => Object.keys(after.cells).some(n => after.cells[n].part === p));
    assert.ok(partsBefore.length > partsAfter.length,
        'a shape that dropped positions left every part reading as placed');
    // The palette itself is untouched: a part does not leave the style because
    // its position left the flow.
    assert.deepStrictEqual(after.parts, s.parts);
});

// AN APPLY THAT SKIPPED PREVIEW CANNOT EXIST. There is no path from the model
// to a save without a preview: applyPreset marks the draft stale, and the bar's
// button is the only save control either surface draws.
test('applyPreset leaves the bar asking for a preview, never offering a save', () => {
    let s = initStyle(7);
    s = M.applyPreview(s, { fingerprint: 'abc', order_count: 4, findings: [] });
    assert.strictEqual(M.bar(s).button.enabled, true, 'a previewed flow should be savable');

    s = M.reduce(s, { type: 'applyPreset', preset: presetFromStyle(7) });
    assert.strictEqual(s.previewStale, true);
    assert.strictEqual(M.bar(s).button.enabled, false,
        'the bar offered a save on a draft whose preview predates the apply');
});

// ONE MODEL, TWO SURFACES (SPEC §0.1). The station inits with a station's
// options and the desktop with the page's; the cells an apply produces are the
// flow, and the flow cannot depend on which screen is looking at it.
test('toCells after an apply is the same on the desktop as on the station', () => {
    const preset = presetFromStyle(7);
    const station = M.reduce(initStyle(11), { type: 'applyPreset', preset: preset });
    // The desktop's init differs only in what it knows about the press — no
    // bin word, the process s claims for the role read, no last run.
    const f = FIXTURES.styles['11'];
    const desktop = M.reduce(M.init({
        styleId: 11, styleName: f.name, positions: POSITIONS, routing: ROUTING,
        claims: f.claims, parts: f.parts, flowspec: FLOWSPEC, groups: GROUPS,
        processClaims: f.claims, advanced: f.advanced || {},
    }), { type: 'applyPreset', preset: preset });
    assert.deepStrictEqual(M.toCells(desktop), M.toCells(station));
});

// ═══ 9. the ORDERS sentences (U10 P1) ════════════════════════════════════════
//
// EXACT STRINGS, against the REAL planner's actions. The fixture's `actions`
// are the preview response for each style's own flow, generated by
// engine.PreviewFlow in the Go harness — so these assertions are what an
// operator reads at the press, not what a test author thought the planner
// would say.

function previewed(styleId) {
    const s = initStyle(styleId);
    return M.applyPreview(s, {
        fingerprint: 'fp',
        order_count: (FIXTURES.styles[String(styleId)].actions || []).length,
        actions: FIXTURES.styles[String(styleId)].actions || [],
        findings: [],
    });
}

test('the press index reads as four orders: two for Robot 1, two for Robot 2', () => {
    const s = previewed(7);
    const got = M.orderSentences(s).map(M.orderSentence);
    assert.deepStrictEqual(got, [
        'Robot 1 · PLN_02 → PLN_01',
        'Robot 2 · PLN_01 → PLN_02',
        'Robot 1 · PLN_05 → PLN_04',
        'Robot 2 · PLN_04 → PLN_05',
    ]);
    // THE PRESS'S OWN INDEX GETS NO ROW (ruled R2 on 09-12). Two positions,
    // four orders: the rows come from the planner's supply and evac orders and
    // from nothing else, so there is no row for a move no robot drives.
    const robots = got.map(x => x.slice(0, 7));
    assert.strictEqual(robots.filter(r => r === 'Robot 1').length, 2);
    assert.strictEqual(robots.filter(r => r === 'Robot 2').length, 2);
    for (const line of got) {
        assert.ok(!/index|press/i.test(line), 'a row mentions the press: ' + line);
    }
});

test('the two-robot swap names the supermarket at both ends, and the part on the way in', () => {
    const s = previewed(11);
    const got = M.orderSentences(s).map(M.orderSentence);
    assert.deepStrictEqual(got, [
        'Robot 2 · PLN_01 → Supermarket Area',
        'Robot 1 · Supermarket Empty Totes → PLN_03 · 70310-TVF51.71',
        'Robot 2 · PLN_04 → Supermarket Area',
        'Robot 1 · Supermarket Empty Totes → PLN_06 · 66059-MCH30.49',
        'Robot 2 · PLN_02 → Supermarket Area',
        'Robot 2 · PLN_05 → Supermarket Area',
    ]);
});

// `Robot N · from → to`, and `· what` only when there is something to name.
// A supply trip names the PART it carries; an evacuation takes out whatever is
// in there and stops (owner ruling R2, 2026-09-12 — the bin word it used to
// end on is gone from every surface).
test('every sentence is Robot N · from → to, and a part only when there is one', () => {
    for (const id of [7, 11]) {
        for (const line of M.orderSentences(previewed(id)).map(M.orderSentence)) {
            assert.ok(/^Robot [12] · [^→]+ → [^·]+( · .+)?$/.test(line), id + ': ' + line);
        }
    }
    // Robot 2's rows never carry one, and Robot 1's carry one when the spec
    // named a payload: the shape is not "sometimes there is a third field",
    // it is "the part, when a part is what is moving".
    for (const o of M.orderSentences(previewed(11))) {
        if (o.robot === 2 && o.what !== '') {
            assert.fail('an evacuation named what it carries: ' + M.orderSentence(o));
        }
    }
});

// REMOVED WITH THE WORD: `the bin word is the style s, singular, because an
// order moves one bin` pinned M.oneBinWord — that dropping a trailing s over
// a two-word vocabulary is exact rather than a guess at English. Owner ruling
// R2 took the word off every surface, so there is no singular to get right.

test('a leg with only one end is left out rather than half-printed', () => {
    let s = initStyle(7);
    s = M.applyPreview(s, {
        fingerprint: 'fp', order_count: 2, findings: [],
        actions: [
            { core_node_name: 'PLN_01', supply_order: { kind: 'complex', to: 'PLN_01' } },
            { core_node_name: 'PLN_04', supply_order: { kind: 'complex', from: 'PLN_05', to: 'PLN_04' } },
        ],
    });
    const got = M.orderSentences(s).map(M.orderSentence);
    assert.deepStrictEqual(got, ['Robot 1 · PLN_05 → PLN_04']);
});

test('no preview means no sentences, and it does not throw', () => {
    assert.deepStrictEqual(M.orderSentences(initStyle(7)), []);
});



// ═══ robot drives via ════════════════════════════════════════════════════════

// THE ONE KEY-ROUTE WALK. It was the station's, inside composer-render.js; the
// desktop had a second picker that filtered the routing set for
// `role === 'waypoint'` — a role the schema forbids on purpose — so it offered
// nothing and the desktop's key-route authoring was inert. The HMI's answers
// are the ones operators use, so the HMI's walk is the one that survived, and
// these pin what it offers.

test('via: the LM points along the short supply path, in order', () => {
    const s = initStyle(7);
    assert.deepStrictEqual(M.viaWaypoints(s, 'PLN_01'), ['LM10', 'LM11', 'LM12']);
});

test('via: the detour is not offered — the walk is shortest-path', () => {
    const s = initStyle(7);
    assert.ok(!M.viaWaypoints(s, 'PLN_01').includes('LM90'),
        'LM90 is on a route eight times longer than the one through LM10');
});

// A waypoint the vendor map does not have is a route the robot cannot drive,
// so nothing that is not a point of the scene may be offered.
test('via: every offered point is a point of the scene', () => {
    const s = initStyle(7);
    const known = new Set();
    for (const e of SCENE.edges) { known.add(e.from); known.add(e.to); }
    for (const w of M.viaWaypoints(s, 'PLN_01')) {
        assert.ok(known.has(w), w + ' is not a point on the map');
    }
});

test('via: no scene, no waypoints — the row offers "shortest way" alone', () => {
    const bare = M.init({ styleId: 7, positions: POSITIONS, routing: ROUTING, claims: [], groups: GROUPS });
    assert.deepStrictEqual(M.viaWaypoints(bare, 'PLN_01'), []);
});

// At most four, evenly spaced (SPEC S5). A press whose supply path crosses
// twenty LM points must not put twenty chips in a popover on a touch screen.
test('via: at most four, evenly spaced', () => {
    const long = { edges: [{ from: 'SMN_05', to: 'LM00', len: 1 }] };
    for (let i = 0; i < 19; i++) long.edges.push({ from: 'LM' + String(i).padStart(2, '0'), to: 'LM' + String(i + 1).padStart(2, '0'), len: 1 });
    long.edges.push({ from: 'LM19', to: 'PLN_02', len: 1 });
    const s = M.init({
        styleId: 7, positions: POSITIONS, routing: ROUTING, groups: GROUPS, scene: long,
        claims: FIXTURES.styles['7'].claims, flowspec: FLOWSPEC,
    });
    const got = M.viaWaypoints(s, 'PLN_01');
    assert.strictEqual(got.length, 4, 'got ' + got.join(', '));
    assert.deepStrictEqual(got, ['LM00', 'LM05', 'LM10', 'LM15']);
});

// ═══ one definition of "a part needs a position" ═════════════════════════════

// THE MODEL'S ANSWER MUST BE THE SERVER'S. The operator sees this finding
// without a round trip, which is the whole reason the model computes it — and
// two implementations of one rule is two rules. They had already drifted: the
// model counted a part sitting on a switched-OFF cell as placed, while the
// server reads the DRAFT and a cell with no choreography is not in it, so the
// bar said the flow was ready and the preview came back with a finding under
// it.
//
// The expectation is generated: composer_model_characterization_test.go runs
// domain.ValidateFlowPartsPlaced over the real Hopkinsville rows for the draft
// that drops the style's first position, and puts its sentences in the
// fixture. Nothing here describes the rule twice.
for (const id of ['7', '11']) {
    test('unplaced parts (style ' + id + ') match the server, dropping the first position', () => {
        const f = FIXTURES.styles[id];
        const first = f.claims[0].core_node_name;
        let s = initStyle(Number(id));
        s = M.reduce(s, { type: 'removePosition', node: first });
        const local = M.findings(s).filter(x => x.field === 'unplaced_part');

        const want = f.unplaced_dropping_first || [];
        if (!want.length) {
            assert.strictEqual(local.length, 0,
                'the server found nothing unplaced and the model found ' + JSON.stringify(local.map(x => x.parts)));
            return;
        }
        assert.strictEqual(local.length, 1, 'one finding for all loose parts, with no node');
        // The server's sentence names the parts after a colon; the model keeps
        // them as a list. Compare the SETS, not the prose: the two surfaces
        // word this for different screens and always have.
        const serverParts = want[0].split(': ')[1].split(', ').sort();
        assert.deepStrictEqual(local[0].parts.slice().sort(), serverParts,
            'the server says ' + JSON.stringify(serverParts) + ' and the model says ' +
            JSON.stringify(local[0].parts));
    });
}

// ── report ───────────────────────────────────────────────────────────────────
if (failures) {
    console.error('\n' + failures + ' failed, ' + checks + ' passed');
    process.exit(1);
}
console.log(checks + ' characterization checks passed');
