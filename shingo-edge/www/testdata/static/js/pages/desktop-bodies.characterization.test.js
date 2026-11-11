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
// EVERY BUILDER NOW, NOT ONLY THE SURVIVORS (U0 of the process-setup UX run,
// 2026-11-11). That run reworks the sheets that call the rest of this file, so
// the second half below freezes every other builder the same way, before any
// source moves: each input copies the shape its caller in
// processes-desktop.js passes, named by function and file:line at the base
// (8e076d27). Line numbers drift; the function names are what to search for.
// Where a caller hands over a value raw (an untrimmed sheet field, a style
// read that lacks a column), the pin records what the body does with it
// today, because that is exactly what a rework changes without noticing.
//
// Run by www/desktop_bodies_characterization_test.go, like apply-loop.test.js.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');

// The page code under test. This file lives in www/testdata, outside the
// embedded static tree, so no binary carries or serves it
// (TestStaticTree_HoldsNoTestJS); the code it reads stays in www/static.
const SRC_DIR = path.join(__dirname, '..', '..', '..', '..', 'static', 'js', 'pages');

const nodeProcess = process;

const src = fs.readFileSync(path.join(SRC_DIR, 'desktop-bodies.js'), 'utf8');
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
        { name: 'Press 1 west', description: 'the west press', counter_plc_name: 'PLC1', counter_tag_name: 'CNT1', counter_enabled: true,
            identity_tag_name: 'ID1', identity_none: false, changeover_auto_arm: '', group_id: 2 }),
    {
        name: 'Press 1 west', description: 'the west press',
        counter_plc_name: 'PLC1', counter_tag_name: 'CNT1', counter_enabled: true,
        identity_tag_name: 'ID1', identity_none: false,
        changeover_auto_arm: '', group_id: 2,
    });

check('processSettings: identity none rides the draft, so a rename keeps it',
    B.processSettings({ id: 4 }, { name: 'Y', identity_none: true }),
    { name: 'Y', description: '', counter_plc_name: '', counter_tag_name: '', counter_enabled: false,
        identity_tag_name: '', identity_none: true, changeover_auto_arm: '', group_id: null });

check('processSettings: empty strings and an unset group are the null the handler spells',
    B.processSettings({ id: 1 }, { name: 'X', description: '', group_id: 0 }),
    { name: 'X', description: '', counter_plc_name: '', counter_tag_name: '', counter_enabled: false,
        identity_tag_name: '', identity_none: false, changeover_auto_arm: '', group_id: null });

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

// ═════════════════════════════════════════════════════════════════════════════
// U0 (process-setup UX, 2026-11-11): the rest of the builders, and the caller
// shapes the checks above do not reach. Every line cites the caller its input
// copies, at the base tree.
// ═════════════════════════════════════════════════════════════════════════════

// A station row as stationsOf(S.processID) holds it: domain.Station's JSON,
// joined and health columns included, because that is what the sheet passes.
const STATION_ROW = {
    id: 41, process_id: 3, code: 'SCR_A', name: 'Screen A', note: 'old note', area_label: 'Bay 2',
    sequence: 2, controller_node_id: 'CTL_A1', enabled: true, health_status: 'online',
    last_seen_at: '2026-11-10T08:00:00Z', created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-11-10T08:00:00Z', process_name: 'Press 1',
};

// ── stationWrite: openScreenSheet's Save and submitAddProcess's screen ───────
// openScreenSheet (processes-desktop.js:6962) → stationBody(st, editing,
// { name: sheetValue('name'), note: sheetValue('note') }), st from
// stationsOf(S.processID).find(…) || {}, editing = !!st.id.
check('stationWrite edit: the row as it stands, the sheet\'s name and note over it, the joined columns left behind',
    B.stationWrite(STATION_ROW, 3, true, { name: 'Screen A west', note: 'new note' }),
    { process_id: 3, code: 'SCR_A', name: 'Screen A west', note: 'new note', area_label: 'Bay 2',
        sequence: 2, controller_node_id: 'CTL_A1', enabled: true });

check('stationWrite edit: a switched-off screen stays off',
    B.stationWrite(Object.assign({}, STATION_ROW, { enabled: false, sequence: 0, controller_node_id: '' }), 3, true,
        { name: 'Screen A', note: '' }),
    { process_id: 3, code: 'SCR_A', name: 'Screen A', note: '', area_label: 'Bay 2',
        sequence: 0, controller_node_id: '', enabled: false });

check('stationWrite edit: a row with no process_id takes the page\'s process',
    B.stationWrite({ id: 42, name: 'Screen B', enabled: true }, 3, true, { name: 'Screen B', note: 'n' }),
    { process_id: 3, code: '', name: 'Screen B', note: 'n', area_label: '', sequence: 0,
        controller_node_id: '', enabled: true });

check('stationWrite edit: the sheet\'s fields ride untrimmed (sheetValue is raw)',
    B.stationWrite(STATION_ROW, 3, true, { name: '  Screen A  ', note: ' x ' }),
    { process_id: 3, code: 'SCR_A', name: '  Screen A  ', note: ' x ', area_label: 'Bay 2',
        sequence: 2, controller_node_id: 'CTL_A1', enabled: true });

// openScreenSheet's Add: st is {} and editing is false.
check('stationWrite create (screens sheet): the defaults, switched on, under the page\'s process',
    B.stationWrite({}, 3, false, { name: 'Screen C', note: 'a note' }),
    { process_id: 3, code: '', name: 'Screen C', note: 'a note', area_label: '', sequence: 0,
        controller_node_id: '', enabled: true });

// submitAddProcess (processes-desktop.js:6793): stationWrite(null, processID,
// false, { name: screen, note: '' }), screen trimmed, processID step 1's id.
check('stationWrite create (Add process): the new process\'s id, the trimmed screen name',
    B.stationWrite(null, 57, false, { name: 'Screen D', note: '' }),
    { process_id: 57, code: '', name: 'Screen D', note: '', area_label: '', sequence: 0,
        controller_node_id: '', enabled: true });

// Not a caller shape, the rule: create never reads the station it is handed.
check('stationWrite create: a station passed with editing=false is ignored',
    B.stationWrite(STATION_ROW, 3, false, { name: 'Screen E', note: '' }),
    { process_id: 3, code: '', name: 'Screen E', note: '', area_label: '', sequence: 0,
        controller_node_id: '', enabled: true });

// ── styleCreate: openNewStyle's one-off create ───────────────────────────────
// processes-desktop.js:6278: styleCreate(name, S.processID, sheetValue('catid'))
// with name trimmed and the CATID raw. (The mint path trims its CATID; this
// one does not, and apiCreateStyle trims it server-side.)
check('styleCreate: the name, the process, the typed CATID, an empty description',
    B.styleCreate('Part A', 3, 'CAT-01'),
    { name: 'Part A', description: '', process_id: 3, expected_catid: 'CAT-01' });

check('styleCreate: a blank CATID is the empty string',
    B.styleCreate('Part A', 3, ''),
    { name: 'Part A', description: '', process_id: 3, expected_catid: '' });

check('styleCreate: the CATID rides untrimmed',
    B.styleCreate('Part A', 3, ' CAT-01 '),
    { name: 'Part A', description: '', process_id: 3, expected_catid: ' CAT-01 ' });

check('styleCreate: a missing process is 0, which the handler refuses',
    B.styleCreate('Part A', undefined, 'CAT-01'),
    { name: 'Part A', description: '', process_id: 0, expected_catid: 'CAT-01' });

// ── styleWrite: rename, Expected CATID, and the mint's CATID ─────────────────
// styleAction (processes-desktop.js:6123 rename, :6130 catid) → styleBody(st,
// change) with st = styleByID(id): the list row (S.styles, domain.Style's
// JSON) when it has the id, else composerStyle(id) (domain.ComposerStyle,
// which carries `catid` and neither description nor process_id), else the
// stub { id, name: '' }.
const STYLE_ROW = { id: 12, name: 'Part A', description: 'left hand', process_id: 3,
    created_at: '2026-01-01T00:00:00Z', expected_catid: 'CAT-01' };

check('styleWrite rename (list row): the row as it stands, the new name over it',
    B.styleWrite(STYLE_ROW, 3, { name: 'Part A2' }),
    { name: 'Part A2', description: 'left hand', process_id: 3, expected_catid: 'CAT-01' });

check('styleWrite rename: the typed name rides untrimmed',
    B.styleWrite(STYLE_ROW, 3, { name: ' Part A2 ' }),
    { name: ' Part A2 ', description: 'left hand', process_id: 3, expected_catid: 'CAT-01' });

check('styleWrite catid (list row): the new CATID over the row',
    B.styleWrite(STYLE_ROW, 3, { expected_catid: 'CAT-02' }),
    { name: 'Part A', description: 'left hand', process_id: 3, expected_catid: 'CAT-02' });

check('styleWrite catid: a blanked field clears the CATID (the overlay wins)',
    B.styleWrite(STYLE_ROW, 3, { expected_catid: '' }),
    { name: 'Part A', description: 'left hand', process_id: 3, expected_catid: '' });

check('styleWrite (composer style): catid stands in for expected_catid, the description is blank, the process is the page\'s',
    B.styleWrite({ id: 12, name: 'Part A', catid: 'CAT-01', claim_count: 2, claim_modes: ['two_robot'] }, 3,
        { name: 'Part A2' }),
    { name: 'Part A2', description: '', process_id: 3, expected_catid: 'CAT-01' });

check('styleWrite (the stub styleByID falls back to): every base field blank',
    B.styleWrite({ id: 99, name: '' }, 3, { expected_catid: 'CAT-09' }),
    { name: '', description: '', process_id: 3, expected_catid: 'CAT-09' });

// mintNewStyle (processes-desktop.js:6471): st = { id: style_id, name:
// style_name, process_id: S.processID } from the mint's answer, change the
// trimmed CATID. The description is blank whether the mint made a style or
// resolved one that already runs (`Already runs as …`).
check('styleWrite (after a mint): the answer\'s id and name, a blank description, the CATID',
    B.styleWrite({ id: 77, name: 'Part M', process_id: 3 }, 3, { expected_catid: 'CAT-M' }),
    { name: 'Part M', description: '', process_id: 3, expected_catid: 'CAT-M' });

// ── styleClone ───────────────────────────────────────────────────────────────
// styleAction 'clone' (processes-desktop.js:6137): styleClone(sheetValue('name')),
// raw and unchecked — the sheet opens on `<name> copy`.
check('styleClone: the name is the whole body',
    B.styleClone('Part A copy'),
    { name: 'Part A copy' });

check('styleClone: untrimmed, and an emptied field is sent empty',
    [B.styleClone(' Part A copy '), B.styleClone('')],
    [{ name: ' Part A copy ' }, { name: '' }]);

// ── flowMint: the New style sheet's recipe path ──────────────────────────────
// mintNewStyle (processes-desktop.js:6466): flowMint(recipe, picks,
// stationIDForSave(), moves) — recipe is sheetValue('recipe') (a string), each
// pick { code, node: moves[head] || head }, the station the process's first
// screen or 0, moves the Spots step's map ({} when none).
check('flowMint: the recipe id as a number, a node per part, the moves as overrides',
    B.flowMint('7', [{ code: 'PART-A', node: 'POS_A1' }, { code: 'PART-B', node: 'POS_B2' }], 41,
        { POS_B1: 'POS_B2' }),
    { preset_id: 7, parts: [{ code: 'PART-A', node: 'POS_A1' }, { code: 'PART-B', node: 'POS_B2' }],
        station_id: 41, overrides: { POS_B1: 'POS_B2' } });

check('flowMint: no moves is no overrides key, no screen is station 0',
    B.flowMint('7', [{ code: 'PART-A', node: 'POS_A1' }], 0, {}),
    { preset_id: 7, parts: [{ code: 'PART-A', node: 'POS_A1' }], station_id: 0 });

check('flowMint: a slot with no node (head \'\') sends the code alone',
    B.flowMint('7', [{ code: 'PART-A', node: '' }, { code: 'PART-B', node: 'POS_B1' }], 41, {}),
    { preset_id: 7, parts: [{ code: 'PART-A' }, { code: 'PART-B', node: 'POS_B1' }], station_id: 41 });

// ── flowPresetApplySave: the apply loop's save and its one retry ─────────────
// runPresetApply (processes-desktop.js:7915, and :7934 for the retry):
// flowPresetApplySave(st.id, row.cells, row.fingerprint, stationIDForSave(),
// { id: p.id, version: p.version }).
const APPLY_CELLS = [{ core_node_name: 'POS_A1', swap_mode: 'two_robot', role: 'consume',
    payload_code: 'PART-A', inbound_staging: 'STG_1' }];

check('flowPresetApplySave: the save\'s four fields and both provenance fields',
    B.flowPresetApplySave(12, APPLY_CELLS, 'fp-1', 41, { id: 5, version: 3 }),
    { to_style_id: 12, cells: APPLY_CELLS, fingerprint: 'fp-1', station_id: 41,
        source_preset_id: 5, source_preset_version: 3 });

check('flowPresetApplySave: the cells go as given — the part stays on them',
    B.flowPresetApplySave(12, APPLY_CELLS, 'fp-1', 41, { id: 5, version: 3 }).cells[0].payload_code,
    'PART-A');

check('flowPresetApplySave: a preset with no version sends neither provenance field',
    B.flowPresetApplySave(12, APPLY_CELLS, 'fp-1', 41, { id: 5, version: 0 }),
    { to_style_id: 12, cells: APPLY_CELLS, fingerprint: 'fp-1', station_id: 41 });

check('flowPresetApplySave: no fingerprint is the empty string, no screen is station 0',
    B.flowPresetApplySave(12, APPLY_CELLS, '', 0, { id: 5, version: 3 }),
    { to_style_id: 12, cells: APPLY_CELLS, fingerprint: '', station_id: 0,
        source_preset_id: 5, source_preset_version: 3 });

// ── flowPresetCreate: the naming modal's three sources ───────────────────────
// openPresetNaming (processes-desktop.js:7485) and saveGroupedRecipe (:7519),
// reached from 'save-preset' { styleID: st.id } (:8174), 'preset-name-cand'
// { shapeKey: c.shape_key } (:8166) and 'shape-save' { cells: toCells } or
// { cells, groups: groupsBody(…) } (:8150). The name is sheetValue('name'), raw.
check('flowPresetCreate (Save as preset…): the style it was saved from',
    B.flowPresetCreate('Two up', { styleID: 12 }),
    { name: 'Two up', from_style_id: 12 });

check('flowPresetCreate (an offered shape): the candidate\'s key',
    B.flowPresetCreate('Two up', { shapeKey: 'two_robot:2' }),
    { name: 'Two up', from_candidate_shape: 'two_robot:2' });

check('flowPresetCreate (a drawn shape): the cells, every part blanked, and no groups key',
    B.flowPresetCreate('Two up', { cells: [
        { core_node_name: 'POS_A1', swap_mode: 'two_robot', role: 'consume', payload_code: 'PART-A', inbound_staging: 'STG_1' },
        { core_node_name: 'POS_A2', swap_mode: 'two_robot', role: 'produce', payload_code: '' },
    ] }),
    { name: 'Two up', cells: [
        { core_node_name: 'POS_A1', swap_mode: 'two_robot', role: 'consume', payload_code: '', inbound_staging: 'STG_1' },
        { core_node_name: 'POS_A2', swap_mode: 'two_robot', role: 'produce', payload_code: '' },
    ] });

check('flowPresetCreate (a drawn shape with groups): the groups ride beside the cells, as drawn',
    B.flowPresetCreate('Two up', {
        cells: [{ core_node_name: 'POS_A1', swap_mode: 'two_robot_press_index', payload_code: 'PART-A' }],
        groups: [{ of: 'POS_A1', name: 'Left', spots: [{ core_node_name: 'POS_A1' }], advanced: { index_robot_supplies: true } }],
    }),
    { name: 'Two up',
        cells: [{ core_node_name: 'POS_A1', swap_mode: 'two_robot_press_index', payload_code: '' }],
        groups: [{ of: 'POS_A1', name: 'Left', spots: [{ core_node_name: 'POS_A1' }], advanced: { index_robot_supplies: true } }] });

// 'shape-save' builds { cells, groups: [] } when the draft has a groups list
// that is empty, and openPresetNaming routes any `from.groups` to
// saveGroupedRecipe; the builder drops an empty list.
check('flowPresetCreate: an empty groups list sends no groups key',
    B.flowPresetCreate('Two up', { cells: [{ core_node_name: 'POS_A1', payload_code: 'PART-A' }], groups: [] }),
    { name: 'Two up', cells: [{ core_node_name: 'POS_A1', payload_code: '' }] });

check('flowPresetCreate: the name rides untrimmed',
    B.flowPresetCreate(' Two up ', { styleID: 12 }),
    { name: ' Two up ', from_style_id: 12 });

// Not a caller shape, the rule: one source, and a style outranks the others.
check('flowPresetCreate: given several sources, the style wins',
    B.flowPresetCreate('Two up', { styleID: 12, shapeKey: 'two_robot:2', cells: [] }),
    { name: 'Two up', from_style_id: 12 });

// ── processGate / processContainment / nodeCurtain: saveSettings's doors ─────
// saveSettings (processes-desktop.js:5315): processGate(S.settings.flow_composer_enabled).
check('processGate: the switch, either way',
    [B.processGate(true), B.processGate(false)],
    [{ flow_composer_enabled: true }, { flow_composer_enabled: false }]);

// saveSettings (processes-desktop.js:5329): processContainment(
// S.settings.quality_hold_enabled, S.settings.quality_hold_destination).
check('processContainment: on, with somewhere to send bins',
    B.processContainment(true, 'QH_LANE'),
    { enabled: true, destination: 'QH_LANE' });

check('processContainment: switched off, the destination still rides',
    B.processContainment(false, 'QH_LANE'),
    { enabled: false, destination: 'QH_LANE' });

check('processContainment: off with nothing picked',
    B.processContainment(false, ''),
    { enabled: false, destination: '' });

// saveSettings (processes-desktop.js:5354): nodeCurtain(d.enabled, d.plc,
// d.tag, d.safe), d one node's curtainDraft entry — plc/tag the raw input
// values, safe true/false from 'cu-val' or null when never chosen.
check('nodeCurtain: on, both pointers, released on TRUE',
    B.nodeCurtain(true, 'PLC1', 'CURTAIN_A1', true),
    { enabled: true, plc_name: 'PLC1', tag_name: 'CURTAIN_A1', safe_value: true });

check('nodeCurtain: released on FALSE is false, not null',
    B.nodeCurtain(true, 'PLC1', 'CURTAIN_A1', false),
    { enabled: true, plc_name: 'PLC1', tag_name: 'CURTAIN_A1', safe_value: false });

check('nodeCurtain: switched off and never set up',
    B.nodeCurtain(false, '', '', null),
    { enabled: false, plc_name: '', tag_name: '', safe_value: null });

check('nodeCurtain: the pointers ride untrimmed (the page trims only to check)',
    B.nodeCurtain(false, ' PLC1 ', ' CURTAIN_A1 ', null),
    { enabled: false, plc_name: ' PLC1 ', tag_name: ' CURTAIN_A1 ', safe_value: null });

check('nodeCurtain: a release value that is not a boolean is null, never coerced',
    B.nodeCurtain(true, 'PLC1', 'CURTAIN_A1', 'true'),
    { enabled: true, plc_name: 'PLC1', tag_name: 'CURTAIN_A1', safe_value: null });

// ── processActiveStyle: "Mark as running" ────────────────────────────────────
// openRunningSheet (processes-desktop.js:6189): processActiveStyle(id), id
// Number(btn.dataset.style) by way of styleAction.
check('processActiveStyle: the style id, as a number',
    [B.processActiveStyle(12), B.processActiveStyle('12')],
    [{ style_id: 12 }, { style_id: 12 }]);

// ── processGroupCreate: P0's Add group ───────────────────────────────────────
// openNewGroup (processes-desktop.js:6490): processGroupCreate(name,
// sheetValue('description')), the name trimmed and the description raw.
check('processGroupCreate: the name and the description',
    B.processGroupCreate('North bay', 'presses on the north wall'),
    { name: 'North bay', description: 'presses on the north wall' });

check('processGroupCreate: a blank description, and one with spaces rides untrimmed',
    [B.processGroupCreate('North bay', ''), B.processGroupCreate('North bay', '  wall  ')],
    [{ name: 'North bay', description: '' }, { name: 'North bay', description: '  wall  ' }]);

// ── caller shapes the first half's checks do not reach ───────────────────────

// saveSettings (processes-desktop.js:5300): processSettings(process(),
// S.settings), S.settings being settingsDraftFor(p) — which carries three keys
// that ride their own doors (flow_composer_enabled, quality_hold_*) and opens
// changeover_auto_arm on 'auto'. The inputs write S.settings[key] = el.value,
// so the name is whatever was typed.
const SETTINGS_DRAFT = {
    name: '  Press 1 west ', description: 'the west press', group_id: 0,
    counter_plc_name: 'PLC1', counter_tag_name: 'CNT1', counter_enabled: true,
    identity_tag_name: '', identity_none: false, changeover_auto_arm: 'prompt',
    flow_composer_enabled: true, quality_hold_enabled: true, quality_hold_destination: 'QH_LANE',
};
const SETTINGS_BODY = {
    name: '  Press 1 west ', description: 'the west press',
    counter_plc_name: 'PLC1', counter_tag_name: 'CNT1', counter_enabled: true,
    identity_tag_name: '', identity_none: false, changeover_auto_arm: 'prompt', group_id: null,
};
check('processSettings: the full draft — its own-door keys stay off the PUT, the name untrimmed',
    B.processSettings({ id: 3, name: 'Press 1' }, SETTINGS_DRAFT),
    SETTINGS_BODY);

check('processSettings: the process argument is not read (process() may be null)',
    B.processSettings(null, SETTINGS_DRAFT),
    SETTINGS_BODY);

// submitAddProcess (processes-desktop.js:6783): processCreate({ name,
// description: sheetValue('description'), group_id: S.add.groupID }) — the
// name trimmed, the description raw, groupID 0 until the picker sets one.
check('processCreate: a picked group rides, the description untrimmed',
    B.processCreate({ name: 'Press 2', description: '  a press  ', group_id: 5 }),
    { name: 'Press 2', description: '  a press  ', counter_plc_name: '', counter_tag_name: '',
        counter_enabled: false, changeover_auto_arm: 'auto', group_id: 5 });

check('processCreate: a blank description is the empty string',
    B.processCreate({ name: 'Press 2', description: '', group_id: 0 }).description,
    '');

// openScreenSheet (processes-desktop.js:6946) reads the picker through
// stationNodes; submitAddPositions (:4998) sends current.concat(picked);
// openRemovePosition / submitRemovePosition (:5018, :5030) send
// current.filter(n => n !== name); submitAddProcess (:6801) the picker as is.
check('stationNodes: whitespace-only and empty entries are dropped',
    B.stationNodes(['   ', '', 'POS_A1']),
    { nodes: ['POS_A1'] });

check('stationNodes: duplicates that differ only by whitespace are one name',
    B.stationNodes([' POS_A1', 'POS_A1 ', 'POS_A1']),
    { nodes: ['POS_A1'] });

check('stationNodes (Add positions): a pick the screen already claims keeps its place',
    B.stationNodes(['POS_A1', 'POS_A2'].concat(['POS_A2', 'POS_A3'])),
    { nodes: ['POS_A1', 'POS_A2', 'POS_A3'] });

check('stationNodes (Remove): the list without the one name',
    B.stationNodes(['POS_A1', 'POS_A2', 'POS_A3'].filter(n => n !== 'POS_A2')),
    { nodes: ['POS_A1', 'POS_A3'] });

check('stationNodes: nothing at all is the empty set',
    [B.stationNodes(undefined), B.stationNodes(null)],
    [{ nodes: [] }, { nodes: [] }]);

// desktop_write_paths_test.go calls routingAdd with no `after`; the page
// always passes routingRowsFor(role).length (processes-desktop.js:3713).
check('routingAdd: no count is the first in its role',
    B.routingAdd('SMN_010', 'source'),
    { core_node_name: 'SMN_010', role: 'source', label: 'SMN_010', enabled: true, sequence: 1 });

// submitAddProcess (processes-desktop.js:6824): one row per picked name per
// ROUTING_GROUPS role, so one name picked under two roles is two rows.
check('routingSet: one name under two roles is two rows, neither dropped',
    B.routingSet([
        { core_node_name: 'GRP_1', role: 'source' },
        { core_node_name: 'GRP_1', role: 'destination' },
    ]),
    { nodes: [
        { core_node_name: 'GRP_1', role: 'source' },
        { core_node_name: 'GRP_1', role: 'destination' },
    ] });

check('routingSet: a row missing a field sends it empty, and no list is the empty set',
    [B.routingSet([{ core_node_name: 'GRP_1' }, { role: 'staging' }]), B.routingSet(null)],
    [{ nodes: [{ core_node_name: 'GRP_1', role: '' }, { core_node_name: '', role: 'staging' }] }, { nodes: [] }]);

// writePartSet (processes-desktop.js:2917) and submitAddProcess (:6835):
// every caller de-duplicates before it calls, so the builder never has to.
check('processPayloads: the builder keeps duplicates and order as given',
    B.processPayloads(['PART-B', 'PART-A', 'PART-B']),
    { payloads: ['PART-B', 'PART-A', 'PART-B'] });

console.log('OK: ' + checks + ' bodies frozen');
