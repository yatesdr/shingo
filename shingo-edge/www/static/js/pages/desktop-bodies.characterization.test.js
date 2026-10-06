// desktop-bodies.characterization.test.js — every request body the page's
// surviving sheets send, frozen.
//
// The list-row Edit sheet is coming out: its four facts each already have
// their own editor (the name/description/group on Settings, the positions on
// Operator screens, the routing set on Settings' routing tab, the part set on
// the flows tab and Add process). One fact, one editor; a second door onto
// the same PUT is a second place to disagree with the first.
//
// THIS PIN EXISTS SO THE DELETION CANNOT DRIFT THE SURVIVORS. Before the
// sheet goes, the exact bytes every surviving write sends are frozen here —
// processSettings (Settings' PUT), stationNodes (the screens sheet's set-to),
// routingEnable/routingAdd (the routing tab's POST/PATCH), processPayloads
// (the flows tab's part set) — over inputs drawn from the sheet's own calls.
// After the deletion the bodies must read the same. If a body had to change
// for any reason, that is a ruling to write down, not a silent edit.
//
// The builders are pure functions of their arguments, so the pin needs no
// DOM, no fetch, no page module — just the file and its documented inputs.
//
// Run by www/desktop_bodies_characterization_test.go, like apply-loop.test.js.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');

const nodeProcess = process;

const src = fs.readFileSync(path.join(__dirname, 'desktop-bodies.js'), 'utf8');
const mod = { exports: {} };
const factory = vm.runInThisContext(
    '(function(module, exports, console, window){' + src + '\nreturn module.exports;})',
    { filename: 'desktop-bodies.js' });
const B = factory(mod, mod.exports, console, {});

let checks = 0;
function check(name, got, want) {
    assert.deepStrictEqual(got, want, name);
    checks++;
}

// ── the PUT the Settings tab sends (and the Edit sheet used to send) ─────────
// Inputs = settingsDraftFor(p)'s shape: the process row as the list carries it
// plus the draft the settings form holds. The counter rides untouched; the
// Edit sheet leaned on that exact property — "the fields this sheet does not
// ask about must survive it".
check('processSettings: the counter rides untouched, the name and group are the draft\'s',
    B.processSettings(
        { id: 3, name: 'Press 1', counter_plc_name: 'PLC1', counter_tag_name: 'CNT1', counter_enabled: true },
        { name: 'Press 1 west', description: 'the west press', counter_plc_name: 'PLC1', counter_tag_name: 'CNT1', counter_enabled: true, changeover_auto_arm: '', group_id: 2 }),
    {
        name: 'Press 1 west', description: 'the west press',
        counter_plc_name: 'PLC1', counter_tag_name: 'CNT1', counter_enabled: true,
        changeover_auto_arm: '', group_id: 2,
    });

check('processSettings: empty strings and an unset group are the null the handler spells',
    B.processSettings({ id: 1 }, { name: 'X', description: '', group_id: 0 }),
    { name: 'X', description: '', counter_plc_name: '', counter_tag_name: '', counter_enabled: false, changeover_auto_arm: '', group_id: null });

// ── the screens sheet's set-to (the other claimed-nodes writer) ──────────────
check('stationNodes: trimmed, deduplicated, in the order picked',
    B.stationNodes(['PLN_03', ' PLN_01 ', 'PLN_03', '']),
    { nodes: ['PLN_03', 'PLN_01'] });

check('stationNodes: an empty pick is the empty set, spelled',
    B.stationNodes([]),
    { nodes: [] });

// ── the routing tab's writes ─────────────────────────────────────────────────
check('routingEnable: the switch and nothing else',
    B.routingEnable(true),
    { enabled: true });

check('routingAdd: a name lands at the end of its role',
    B.routingAdd('SMN_010', 'source', 2),
    { core_node_name: 'SMN_010', role: 'source', label: 'SMN_010', enabled: true, sequence: 3 });

check('routingAdd: the first name in a role is sequence 1',
    B.routingAdd('DN_001', 'destination', 0),
    { core_node_name: 'DN_001', role: 'destination', label: 'DN_001', enabled: true, sequence: 1 });

// ── the part set ─────────────────────────────────────────────────────────────
check('processPayloads: every code, spelled, in order',
    B.processPayloads(['PART-A', 'PART-B']),
    { payloads: ['PART-A', 'PART-B'] });

check('processPayloads: an empty set is a real answer',
    B.processPayloads([]),
    { payloads: [] });

// ── the bodies the OTHER sheets in this deletion's blast radius send ─────────
// Add process keeps processCreate (its first write) and routingSet (its whole-
// list write). They are frozen here because the Edit sheet's deletion shares
// this file's subject — the page's write vocabulary — and a drift in either
// would be the same class of accident.
check('processCreate: the defaults are decisions, spelled',
    B.processCreate({ name: 'Press 2', description: '', group_id: 0 }),
    { name: 'Press 2', description: '', counter_plc_name: '', counter_tag_name: '', counter_enabled: false, changeover_auto_arm: 'auto', group_id: null });

check('routingSet: the list order is the sequence, and the body does not number it',
    B.routingSet([
        { core_node_name: 'SMN_010', role: 'source' },
        { core_node_name: 'DN_001', role: 'destination' },
        { core_node_name: 'SMN_012', role: 'source' },
    ]),
    { nodes: [
        { core_node_name: 'SMN_010', role: 'source' },
        { core_node_name: 'DN_001', role: 'destination' },
        { core_node_name: 'SMN_012', role: 'source' },
    ] });

console.log('OK: ' + checks + ' bodies frozen');
