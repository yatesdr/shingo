// composer-fields.characterization.test.js — the claim editor's field rules,
// on the surface that replaced it.
//
// REPLACES processes.characterization.test.js. That suite pinned, for every
// (role, swap_mode), which of the claim modal's twenty-seven controls were on
// screen and what saveClaim POSTed. The modal is gone (U9d); the questions it
// answered are not, and they are answered now by the desktop composer — the
// positions table for the fields the picture draws, the Advanced sheet for the
// twelve it does not, and toCells + Expand for the write.
//
// SAME MATRIX, SAME ORACLE, NEW SUBJECT. Two roles by six modes, walked in
// full, against the flowspec block — which is the same bytes
// TestStationFlowspecFileMatchesGo holds to flowspec.ExportJSON(), so the
// expectations are Go's table and never a second copy of it. What changed is
// that the old suite asserted about element ids and this one asserts about the
// model, which is where the rule now lives and is the reason the modal could
// go at all.
//
// The old suite's own count is the floor: it is printed at the end and the Go
// runner refuses a run that pins fewer facts than the editor's did.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');

const HERE = __dirname;
const STATION = path.resolve(HERE, '..', '..', 'operator-station');

// The count the retired suite reported at the commit it was deleted:
//   go test ./www -run TestProcessesJSClaimEditorCharacterization
//   characterization: PASS: 528 assertions across 12 (role,swap) cells
const RETIRED_SUITE_ASSERTIONS = 528;

// runInThisContext, not createContext: a fresh vm context is a fresh realm and
// deepStrictEqual compares prototypes. Same reason as composer-model.test.js.
function loadModel() {
    const src = fs.readFileSync(path.join(STATION, 'composer-model.js'), 'utf8');
    const factory = vm.runInThisContext(
        '(function(module, exports, console){' + src + '\nreturn module.exports;})',
        { filename: 'composer-model.js' });
    const mod = { exports: {} };
    return factory(mod, mod.exports, console);
}

// The flowspec the composer actually reads: window.FLOWSPEC, from the
// generated file. One copy on this tree since the admin page retired.
function loadFlowspec() {
    const src = fs.readFileSync(path.join(STATION, 'flowspec-data.js'), 'utf8');
    const sandbox = { window: {} };
    vm.runInNewContext(src, sandbox, { filename: 'flowspec-data.js' });
    if (!sandbox.window.FLOWSPEC) throw new Error('flowspec-data.js set no window.FLOWSPEC');
    return sandbox.window.FLOWSPEC;
}

const M = loadModel();
const FLOWSPEC = loadFlowspec();

const ROLES = ['consume', 'produce'];
const SWAPS = ['simple', 'sequential', 'single_robot', 'two_robot', 'two_robot_press_index', 'manual_swap'];

// A press with two front positions and their back slots, the shape every
// composer screen is drawn over.
const POSITIONS = [
    { core_node_name: 'PLN_01', kind: 'front', sequence: 1 },
    { core_node_name: 'PLN_02', kind: 'back', sequence: 2 },
    { core_node_name: 'PLN_04', kind: 'front', sequence: 4 },
    { core_node_name: 'PLN_05', kind: 'back', sequence: 5 },
];
const ROUTING = [
    { core_node_name: 'Supermarket Empty Totes', role: 'source', label: 'Supermarket Empty Totes', enabled: true, sequence: 1 },
    { core_node_name: 'Supermarket Area', role: 'destination', label: 'Supermarket Area', enabled: true, sequence: 2 },
    { core_node_name: 'PLN_02', role: 'staging', label: 'PLN_02', enabled: true, sequence: 3 },
];

// The Advanced sheet's twelve, and the flowspec field each one follows. The
// same map composer-model.js holds; duplicated here on purpose, because a
// suite that read the map under test could not catch a key being dropped
// from it.
const ADVANCED_FIELDS = {
    allowed_payload_codes: 'allowed_payload_codes',
    reorder_point: 'reorder_point',
    reorder_point_source: 'reorder_point',
    auto_reorder: 'auto_reorder',
    lineside_soft_threshold: 'lineside_soft_threshold',
    auto_request_payload: 'auto_request_payload',
    auto_push: 'auto_push',
    evacuate_on_changeover: 'evacuate_on_changeover',
    changeover_carryover_disposition: 'changeover_carryover_disposition',
    index_robot_supplies: 'index_robot_supplies',
    auto_confirm: 'auto_confirm',
};

// The cell's own fields — what the picture draws and the positions table
// edits — and the FlowCell key each writes.
const CELL_FIELDS = {
    part: 'payload_code',
    paired: 'paired_core_node',
    secondPaired: 'second_paired_core_node',
    staging: 'inbound_staging',
    parkOld: 'outbound_staging',
    source: 'inbound_source',
    dest: 'outbound_destination',
    evacDest: 'changeover_evac_destination',
    evacNodes: 'changeover_evac_nodes',
    keyRoute: 'key_route',
};

// Every flowspec field the composer authors on one surface or the other. What
// is NOT here is the third assertion of every cell: a column no screen offers
// must never reach the wire, because Expand's carry-through is what keeps it,
// and a value the composer invents would overwrite an engineer's.
const AUTHORED = new Set(
    Object.keys(CELL_FIELDS).map(k => CELL_FIELDS[k])
        .concat(Object.keys(ADVANCED_FIELDS).map(k => ADVANCED_FIELDS[k]))
        .concat(['role', 'swap_mode']));

let failures = 0;
let passed = 0;

function check(label, actual, expected) {
    if (JSON.stringify(actual) === JSON.stringify(expected)) { passed++; return; }
    failures++;
    console.error('FAIL: ' + label);
    console.error('  expected: ' + JSON.stringify(expected));
    console.error('  actual:   ' + JSON.stringify(actual));
}

function need(role, swap, field) {
    const byRole = FLOWSPEC.steady && FLOWSPEC.steady[role];
    const row = byRole && byRole[swap];
    return row ? row[field] : undefined;
}

function stateFor(role, swap, node) {
    let s = M.init({
        styleId: 1, styleName: 'PART 40421-RVJ56.37',
        positions: POSITIONS, routing: ROUTING, parts: ['PIA26', 'PIA27'],
        claims: [], flowspec: FLOWSPEC, groups: {},
        processClaims: [{ core_node_name: node, role: role }],
    });
    s = M.reduce(s, { type: 'addPosition', node: node, mode: swap });
    s = M.reduce(s, { type: 'setMode', node: node, mode: swap });
    return s;
}

// ── 1. WHICH ADVANCED FIELDS THE SHEET DRAWS ────────────────────────────────
//
// The old suite's expectedVisibility table, for the twelve that moved into the
// sheet. A field this choreography FORBIDS is one the server refuses, so
// offering it is offering a save that cannot land; a field it does not READ is
// a control with no effect. Neither is drawn, and flowspec draws the line.
function runAdvancedVisibility() {
    for (const role of ROLES) {
        for (const swap of SWAPS) {
            const s = stateFor(role, swap, 'PLN_01');
            for (const key of Object.keys(ADVANCED_FIELDS)) {
                const n = need(role, swap, ADVANCED_FIELDS[key]);
                const want = n !== 'forbidden' && n !== 'unused';
                check('advancedShows[role=' + role + ', swap=' + swap + '] ' + key,
                    M.advancedShows(s, 'PLN_01', key), want);
            }
        }
    }
}

// ── 2. WHAT A MODE CHANGE CLEARS ─────────────────────────────────────────────
//
// claimForbiddenFields' drop list, in the model. Forbidden is cleared — a
// value left behind is a 422 the engineer did not ask for. UNUSED IS NOT: a
// column cleared at save because a screen stopped drawing it is a data change
// made by a UI decision, which is the argument flowspec.go makes about
// reuse_compatible_bins and it holds for every one of these.
function runForbiddenClearing() {
    const filled = {
        allowed_payload_codes: ['PIA26'], reorder_point: 40, reorder_point_source: 'manual',
        auto_reorder: true, lineside_soft_threshold: 7, auto_request_payload: 'PIA26',
        auto_push: true, evacuate_on_changeover: true,
        changeover_carryover_disposition: 'keep_lineside',
        index_robot_supplies: true, auto_confirm: true,
    };
    const blank = M.advancedDefaults();
    for (const role of ROLES) {
        for (const swap of SWAPS) {
            // Set every advanced field on a press-index cell (the mode that
            // forbids the fewest), then move to the mode under test.
            let s = stateFor(role, 'two_robot_press_index', 'PLN_01');
            s = M.reduce(s, { type: 'setAdvanced', node: 'PLN_01', advanced: filled });
            s = M.reduce(s, { type: 'setMode', node: 'PLN_01', mode: swap });
            const after = M.advancedFor(s, 'PLN_01');
            for (const key of Object.keys(ADVANCED_FIELDS)) {
                const field = ADVANCED_FIELDS[key];
                // Apply itself clears what the CURRENT mode forbids, so a
                // field press-index refuses never got a value to carry —
                // auto_push is the case, forbidden there and used by a
                // manual_swap unloader. Clearing at both ends is the same
                // rule applied twice, not two rules.
                const startN = need(role, 'two_robot_press_index', field);
                const n = need(role, swap, field);
                const want = (startN === 'forbidden' || n === 'forbidden') ? blank[key] : filled[key];
                check('after setMode(' + swap + ') as ' + role + ': ' + key +
                    ' (' + String(startN) + ' -> ' + String(n) + ')', after[key], want);
            }
        }
    }
}

// ── 3. WHAT A MODE CHANGE CLEARS ON THE CELL ────────────────────────────────
//
// The same rule over the fields the PICTURE draws. These were the claim
// modal's fieldsets; they are the positions table's columns now.
function runCellForbiddenClearing() {
    const empty = { part: null, paired: '', secondPaired: '', staging: '', parkOld: '', source: '', dest: '', evacDest: '', evacNodes: [], keyRoute: [] };
    for (const role of ROLES) {
        for (const swap of SWAPS) {
            let s = stateFor(role, 'two_robot_press_index', 'PLN_01');
            s = M.reduce(s, { type: 'setPart', node: 'PLN_01', payloadCode: 'PIA26' });
            s = M.reduce(s, { type: 'setPartner', node: 'PLN_01', partner: 'PLN_02' });
            s = M.reduce(s, { type: 'setStaging', node: 'PLN_01', staging: 'PLN_02' });
            s = M.reduce(s, { type: 'setParkOld', node: 'PLN_01', staging: 'PLN_05' });
            s = M.reduce(s, { type: 'setSource', node: 'PLN_01', source: 'Supermarket Empty Totes' });
            s = M.reduce(s, { type: 'setDest', node: 'PLN_01', dest: 'Supermarket Area' });
            s = M.reduce(s, { type: 'setVia', node: 'PLN_01', via: 'LM167' });
            s = M.reduce(s, { type: 'setEvacDest', node: 'PLN_01', dest: 'Supermarket Area' });
            s = M.reduce(s, { type: 'setEvacNodes', node: 'PLN_01', nodes: ['PLN_01'] });
            const before = s.cells.PLN_01;
            s = M.reduce(s, { type: 'setMode', node: 'PLN_01', mode: swap });
            const after = s.cells.PLN_01;
            for (const key of Object.keys(CELL_FIELDS)) {
                const n = need(role, swap, CELL_FIELDS[key]);
                const want = n === 'forbidden' ? empty[key] : before[key];
                check('cell after setMode(' + swap + ') as ' + role + ': ' + key +
                    ' (' + String(n) + ')', after[key], want);
            }
        }
    }
}

// ── 4. WHAT REACHES THE WIRE ─────────────────────────────────────────────────
//
// saveClaim's body, as toCells now writes it. The old suite pinned a POST to
// /api/style-node-claims with twenty keys; the composer's write goes through
// flow/save and carries a cell, and the difference is the point of the whole
// stream: the wire says what the picture shows and nothing else, and every
// other column comes back off the prior claim through Expand.
function runWireShape() {
    for (const role of ROLES) {
        for (const swap of SWAPS) {
            const s = stateFor(role, swap, 'PLN_01');
            const cells = M.toCells(M.reduce(s, { type: 'setPart', node: 'PLN_01', payloadCode: 'PIA26' }));
            const cell = cells.find(c => c.core_node_name === 'PLN_01');
            if (!cell) {
                check('toCells[' + role + '/' + swap + '] has PLN_01', false, true);
                continue;
            }
            // Every key on the wire is a field the composer authors. A key
            // that is not is a column being overwritten from a screen that
            // never showed it.
            for (const k of Object.keys(cell)) {
                check('toCells[' + role + '/' + swap + '] key ' + k + ' is authored',
                    k === 'core_node_name' || AUTHORED.has(k), true);
            }
            check('toCells[' + role + '/' + swap + '] role', cell.role, role);
            check('toCells[' + role + '/' + swap + '] swap_mode', cell.swap_mode, swap);
            // Advanced is ABSENT until the sheet is applied. This is the whole
            // meaning of the pointer on FlowCell: untouched leaves every one of
            // those twelve columns as Expand carried them.
            check('toCells[' + role + '/' + swap + '] speaks no advanced',
                'advanced' in cell, false);
        }
    }
}

// ── 5. WHAT THE COMPOSER NEVER AUTHORS ───────────────────────────────────────
//
// The other half of the old suite's visibility table: every field flowspec
// knows about that no composer screen offers. Each one is a column an engineer
// or the store owns, and the composer's silence about it is what makes it
// survive a save.
function runUnauthoredFields() {
    const advancedKeys = new Set(Object.keys(ADVANCED_FIELDS).map(k => ADVANCED_FIELDS[k]));
    for (const role of ROLES) {
        for (const swap of SWAPS) {
            const s = stateFor(role, swap, 'PLN_01');
            const cells = M.toCells(M.reduce(s, { type: 'setPart', node: 'PLN_01', payloadCode: 'PIA26' }));
            const cell = cells.find(c => c.core_node_name === 'PLN_01') || {};
            for (const field of FLOWSPEC.fields) {
                if (AUTHORED.has(field)) continue;
                check('unauthored[' + role + '/' + swap + '] ' + field + ' is not on the wire',
                    field in cell, false);
                check('unauthored[' + role + '/' + swap + '] ' + field + ' is not in the sheet',
                    advancedKeys.has(field), false);
            }
        }
    }
}

// ── 6. THE SAVE'S SHAPE FOR ONE CONCRETE CLAIM ───────────────────────────────
//
// The old suite's runSaveClaimSchemaCase, as the composer writes it: a
// consume / single_robot cell with every field the picture draws filled in.
function runSaveShapeCase() {
    let s = stateFor('consume', 'single_robot', 'PLN_01');
    s = M.reduce(s, { type: 'setPart', node: 'PLN_01', payloadCode: 'PIA26' });
    s = M.reduce(s, { type: 'setStaging', node: 'PLN_01', staging: 'PLN_02' });
    s = M.reduce(s, { type: 'setParkOld', node: 'PLN_01', staging: 'PLN_05' });
    s = M.reduce(s, { type: 'setSource', node: 'PLN_01', source: 'Supermarket Empty Totes' });
    s = M.reduce(s, { type: 'setDest', node: 'PLN_01', dest: 'Supermarket Area' });
    const cell = M.toCells(s).find(c => c.core_node_name === 'PLN_01');
    const want = {
        core_node_name: 'PLN_01',
        role: 'consume',
        swap_mode: 'single_robot',
        payload_code: 'PIA26',
        inbound_source: 'Supermarket Empty Totes',
        inbound_staging: 'PLN_02',
        outbound_staging: 'PLN_05',
        outbound_destination: 'Supermarket Area',
    };
    for (const k of Object.keys(want)) check('save shape ' + k, cell[k], want[k]);
    // Absent, each for its own reason. The three partner/routing fields this
    // cell does not use are absent because blank IS absent on both sides now
    // (Go's FlowCell carries omitempty on all seven; Expand reads a missing
    // key and a blank one as the same blank column) — a third of every cell's
    // bytes were empty keys. An empty list is an absent key for the same
    // reason. And advanced is absent because the sheet was never opened,
    // which is the one of these whose absence carries meaning: it is Expand's
    // carry-through rather than an opinion.
    for (const k of ['paired_core_node', 'second_paired_core_node', 'changeover_evac_destination',
        'changeover_evac_nodes', 'key_route', 'advanced']) {
        check('save shape ' + k + ' is absent', k in cell, false);
    }
    // With the sheet applied, exactly one more key, carrying exactly the
    // twelve — no more, so a save cannot reach a column the sheet does not
    // draw.
    const applied = M.reduce(s, {
        type: 'setAdvanced', node: 'PLN_01',
        advanced: Object.assign(M.advancedDefaults(), { reorder_point: 12 }),
    });
    const withAdv = M.toCells(applied).find(c => c.core_node_name === 'PLN_01');
    check('save shape advanced present after Apply', 'advanced' in withAdv, true);
    check('save shape advanced keys', Object.keys(withAdv.advanced).sort(),
        Object.keys(M.advancedDefaults()).sort());
    check('save shape advanced value', withAdv.advanced.reorder_point, 12);
}

runAdvancedVisibility();
runForbiddenClearing();
runCellForbiddenClearing();
runWireShape();
runUnauthoredFields();
runSaveShapeCase();

if (failures) {
    console.error('\n' + failures + ' failed, ' + passed + ' passed');
    process.exit(1);
}
if (passed < RETIRED_SUITE_ASSERTIONS) {
    console.error('WEAKENED: ' + passed + ' assertions, and the claim editor\'s suite pinned ' +
        RETIRED_SUITE_ASSERTIONS + '. A replacement that checks less than what it replaced is not a replacement.');
    process.exit(1);
}
console.log('PASS: ' + passed + ' assertions across ' + (ROLES.length * SWAPS.length) +
    ' (role,swap) cells + the save-shape case (the retired claim editor suite pinned ' +
    RETIRED_SUITE_ASSERTIONS + ')');
