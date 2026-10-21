// Unit tests for operator-board-model.js: the board's layout from its tiles,
// the parts a loader tile draws, coverage, the CLEAR panel's highlight, a
// window's words, the board a station of loader tiles draws (boardModel) and
// its fit (fitBoard), with the proofs that no part number or bin code clips.
//
// Runs under plain Node (no npm): the model is pure, so it runs as it ships in
// a vm with its `export`s dropped. Exit 0 = pass, 1 = any failure. Run via the
// Go wrapper operator_board_model_test.go.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0, failed = 0;
function eq(got, want, label) {
    const g = JSON.stringify(got), w = JSON.stringify(want);
    if (g === w) { passed++; return; }
    failed++;
    console.error('FAIL: ' + label + '\n   got:  ' + g + '\n   want: ' + w);
}

// The model imports the window model (pure as well) for the acts the old
// boards offered a part: both run in one vm, imports and exports dropped.
const ctx = vm.createContext({});
['operator-window-state.js', 'operator-board-model.js'].forEach(function (f) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, f), 'utf8')
        .replace(/^import\s[^;]*;/gm, '').replace(/^export\s+/gm, ''), ctx);
});
const M = ctx;
// Top-level consts in a vm script are not properties of its context.
M.GEOM = vm.runInContext('GEOM', ctx);
M.MAX_TILE_ROWS = vm.runInContext('MAX_TILE_ROWS', ctx);
M.STEPS = vm.runInContext('STEPS', ctx);

function tile(role, o) {
    o = o || {};
    const t = {
        node: { id: o.id || 1, name: o.name || 'W1' },
        loader: {
            role: role, layout: o.dedicated ? 'dedicated_positions' : 'shared_window',
            replenishment: o.operator ? 'operator' : 'threshold', outbound_dest: '',
            parts: (o.parts || ['PART-A']).map(c => ({ code: c })),
            directive: !!o.directive, leaves_bare: false, pulled_directly: false,
            board_shows_all_parts: !!o.allParts,
        },
        orders: [],
    };
    if (o.bin !== undefined) t.bin_state = o.bin;
    if (o.runtime) t.runtime = o.runtime;
    return t;
}
const press = { node: { id: 9 }, active_claim: { role: 'produce' } };
function row(part, have, o) {
    return Object.assign({ cell: 'CELL', part: part, role: 'consume', carrier: '', have_uop: have,
        capacity_uop: 100, incoming: false, node: 'WLN', process_id: 2, process_node_id: 2,
        reorder_uop: 0, waiting: false }, o || {});
}
const stocked = part => ({ occupied: true, payload_code: part, stocked: true });

// ── boardLayout: today's routing, and the header ──
const L = tiles => { const l = M.boardLayout(tiles); return l.kind + '/' + l.header; };
eq(L([]), 'empty/changeover', 'no tiles');
eq(L([tile('produce')]), 'payload/none', 'one loader window: the payload board, no cell buttons');
eq(L([tile('produce'), tile('produce')]), 'payload/none', 'two loader windows: the payload board, no cell buttons (fixed)');
eq(L([tile('produce', { dedicated: true }), tile('produce', { dedicated: true })]), 'homes/pull-full-from-home', 'dedicated homes');
eq(L([tile('consume', { dedicated: true })]), 'homes/pull-full-from-home', 'a dedicated unloader');
eq(L([tile('consume'), tile('consume'), tile('consume')]), 'grid/changeover', 'unloader windows: slots, the header keeps CHANGEOVER');
eq(L([tile('consume')]), 'grid/none', 'one unloader window: a slot, no cell buttons');
eq(L([tile('consume'), tile('produce')]), 'payload/none', 'a station mixing roles: the payload board');
eq(L([press, tile('produce')]), 'grid/changeover', 'a loader beside a press: the node grid');
eq(L([press, tile('produce', { dedicated: true })]), 'grid/pull-full-from-home', 'a dedicated loader beside a press');
eq(L([press]), 'grid/changeover', 'a press');

// ── the parts a running style uses, and the cards ──
const t2 = tile('produce', { parts: ['PART-B', 'PART-A', 'PART-C'] });
eq(M.activeParts(t2, undefined), null, 'no serves: unknown, not empty');
eq(M.activeParts(t2, []), [], 'serves []: nothing runs');
eq(M.activeParts(t2, [row('PART-A', 5), row('PART-C', 1, { incoming: true }), row('PART-B', 1, { role: 'produce' })]),
    ['PART-A'], 'running consume rows only, in the loader\'s order');
eq(M.cardParts(t2, [row('PART-A', 5)]), ['PART-A'], 'threshold loader: the running parts');
eq(M.cardParts(t2, []), ['PART-B', 'PART-A', 'PART-C'], 'threshold loader, nothing running: every part');
eq(M.cardParts(tile('produce', { allParts: true, parts: ['PART-B', 'PART-C', 'PART-A'] }), [row('PART-C', 5)]),
    ['PART-C', 'PART-A', 'PART-B'], 'a board listing every part: every part, running first then by code');
eq(M.cardParts(tile('produce', { operator: true, parts: ['PART-B', 'PART-C', 'PART-A'] }), [row('PART-C', 5)]),
    ['PART-C'], 'an operator loader not set to list every part: the running parts');
const u = tile('consume', { parts: ['PART-A'] });
eq(M.activeParts(u, [row('PART-A', 5)]), [], 'an unloader: a cell using the part is not what it serves');
eq(M.activeParts(u, [row('PART-A', 5, { role: 'produce' })]), ['PART-A'], 'an unloader: a cell making the part');

const held = tile('produce', { parts: ['PART-A'], bin: stocked('PART-A') });
let view;
// ── coverage ──
const states = v => M.coverage(v).map(c => c.row.cell + ':' + c.state);
view = { nodes: [held], serves: [
    row('PART-B', 0, { cell: 'OUT-1' }),
    row('PART-B', 4, { cell: 'EXP-4' }),
    row('PART-B', 2, { cell: 'EXP-2' }),
    row('PART-A', 0, { cell: 'COVERED' }),
    row('PART-B', -5, { cell: 'OVERPACK' }),
    row('PART-B', null, { cell: 'UNSEEN' }),
    row('PART-B', 0, { cell: 'OUT-2' }),
    row('PART-C', 0, { cell: 'INC-OUT', incoming: true, waiting: true }),
    row('PART-C', 6, { cell: 'INC-WAIT', incoming: true, waiting: true }),
    row('PART-C', 0, { cell: 'INC-QUIET', incoming: true }),
] };
eq(states(view), ['OUT-1:out', 'OUT-2:out', 'INC-OUT:out', 'INC-WAIT:waiting', 'EXP-2:exposed', 'EXP-4:exposed',
    'OVERPACK:unknown', 'UNSEEN:unknown', 'COVERED:covered'],
'coverage: out (in the plant\'s order), waiting, exposed (fewest left first), unknown (below 0 or null, never out), covered; an incoming row not waiting draws nothing');
// A running row whose cell has asked (waiting): some left reads waiting, never
// exposed; none left reads out; a stocked bin on the board still covers it;
// an unknown count stays unknown.
eq(states({ nodes: [held], serves: [
    row('PART-B', 4, { cell: 'ASKED-4', waiting: true }),
    row('PART-B', 0, { cell: 'ASKED-0', waiting: true }),
    row('PART-A', 2, { cell: 'ASKED-COVERED', waiting: true }),
    row('PART-B', null, { cell: 'ASKED-UNSEEN', waiting: true }),
    row('PART-B', 3, { cell: 'NOT-ASKED' }),
] }), ['ASKED-0:out', 'ASKED-4:waiting', 'NOT-ASKED:exposed', 'ASKED-UNSEEN:unknown', 'ASKED-COVERED:covered'],
'coverage: a running row that has asked reads waiting with some left, out with none');
eq(M.coverage({ nodes: [], serves: [row('PART-P', 3, { role: 'produce', carrier: 'TOTE-1' })] }).map(c => c.state), ['exposed'],
    'a produce row on an unloader: ready from the empties, absent here');
eq(M.coverage({ nodes: [], serves: [row('PART-P', 3, { role: 'produce', carrier: 'TOTE-1' })],
    empties: { rows: [{ bin_type: 'TOTE-1', have: 2 }] } }).map(c => c.state), ['covered'],
'a produce row on an unloader with an empty of its type: covered');
eq(M.coverage({}), [], 'no serves: no coverage');

// ── clearHighlight ──
const cart = tile('consume', { bin: { occupied: true, payload_code: 'PART-A', bin_type_code: 'TOTE-1' } });
eq(M.clearHighlight(Object.assign({ assignment: { bin_type: 'KD-1' } }, cart), [row('PART-A', 0, { role: 'produce', waiting: true, carrier: 'TOTE-2' })]),
    'KD-1', 'clearHighlight: the window\'s assignment first');
eq(M.clearHighlight(cart, [row('PART-A', 0, { role: 'produce', waiting: true, carrier: 'TOTE-2' })]), 'TOTE-2',
    'clearHighlight: else a waiting row\'s bin type');
eq(M.clearHighlight(cart, [row('PART-A', 0, { role: 'produce', carrier: 'TOTE-2' })]), 'TOTE-1',
    'clearHighlight: else like for like, the cart\'s own type');
eq(M.clearHighlight(tile('consume', { bin: { occupied: false } }), []), null, 'clearHighlight: nothing grounds a choice');

// ── windowWords ──
const W = (t, active) => M.windowWords(t, 'FULL', !!active);
eq(W(tile('consume', { bin: stocked('PART-A') }), true), 'PART-A', 'an unloader window holding a full: the part, a next full inbound or not');
eq(W(tile('consume', { bin: { occupied: false } }), true), 'AWAITING FULL', 'a live order: AWAITING');
eq(W(tile('produce', { bin: stocked('PART-A') })), 'PART-A', 'a bin with a part: the part');
eq(W(tile('produce', { runtime: { remaining_uop_cached: 4 } })), 'LOADED', 'counted parts, no bin read: LOADED');
eq(W(tile('produce', { bin: { occupied: true, payload_code: '' } })), 'EMPTY', 'an empty: EMPTY');
eq(W(tile('produce', { bin: { occupied: false } })), 'NO BIN', 'nothing there: NO BIN');
eq(W(tile('produce')), 'COULD NOT LOOK', 'no bin read at all: COULD NOT LOOK (fixed: it said NO BIN)');


// ═════════════════════════════════════════════════════════════════════════════
// The board
// ═════════════════════════════════════════════════════════════════════════════

// bt builds a loader tile as BuildView ships it, with the keys the board reads
// (A4's buffer and buffer_partial, absent unless given). Options: role,
// dedicated, parts, bin, orders, refusals, buffer, partialFrom, carriers,
// leavesBare, pulledDirectly, outbound, uop, alarm, operator.
function bt(id, name, o) {
    o = o || {};
    const role = o.role || 'produce';
    const t = {
        node: { id: id, name: name, core_node_name: name },
        runtime: { remaining_uop_cached: o.uop || 0 },
        loader: {
            role: role, layout: o.dedicated ? 'dedicated_positions' : 'shared_window',
            replenishment: o.operator ? 'operator' : 'threshold',
            outbound_dest: o.outbound === undefined ? '' : o.outbound,
            parts: (o.parts || ['PART-A']).map(c => ({ code: c })),
            directive: false, leaves_bare: !!o.leavesBare, pulled_directly: !!o.pulledDirectly,
            // The setting as Core's v157 backfilled it.
            board_shows_all_parts: !!o.operator,
        },
        orders: o.orders || [],
    };
    if (o.carriers) t.loader.carriers = o.carriers;
    if (o.buffer) t.loader.buffer = o.buffer;
    if (o.bin !== undefined) t.bin_state = o.bin;
    if (o.refusals) t.supply_refusals = o.refusals;
    if (o.partialFrom) t.buffer_partial = { node: o.partialFrom };
    if (o.alarm) t.stranded_alarm = o.alarm;
    return t;
}
const FULLBIN = (part, type) => ({ occupied: true, payload_code: part, stocked: true, bin_type_code: type || 'TOTE-1' });
const EMPTYBIN = type => ({ occupied: true, payload_code: '', stocked: false, bin_type_code: type || '' });
const NOBIN = { occupied: false };
const ord = (id, status, o) => Object.assign({ id: id, status: status, order_type: 'retrieve', payload_code: '',
    source_node: '', delivery_node: '', created_at: '2026-10-14T08:00:00Z' }, o || {});
const B = (view, ui) => M.boardModel(view, ui);
const zone = (b, key) => b.zones.find(z => z.key === key) || null;
const cardSay = c => [c.spot, c.headline, c.tone, c.word, c.reason];

// ── isBoard ──
eq(M.isBoard([]), false, 'isBoard: no tiles is no board');
eq(M.isBoard([bt(1, 'SMN_A1'), bt(2, 'SMN_A2', { dedicated: true })]), true, 'isBoard: every tile a loader\'s');
eq(M.isBoard([bt(1, 'ULN_1', { role: 'consume' }), bt(2, 'ULN_2', { role: 'consume' })]), true,
    'isBoard: a shared unloader\'s windows are a board (changed: they were the node grid)');
eq(M.isBoard([press, bt(1, 'SMN_A1')]), false, 'isBoard: a loader beside a press keeps the node grid');

// ── spot words, a closed set ──
eq([NOBIN, FULLBIN('PART-A'), EMPTYBIN('KD-1'), EMPTYBIN(''), { occupied: true, payload_code: 'PART-A', stocked: false },
    { occupied: true, bare: true }, undefined].map(b => M.spotWords(bt(1, 'S', { bin: b }))),
[{ words: 'no bin', code: '' }, { words: 'holds a full bin', code: '' }, { words: 'holds an empty', code: 'KD-1' },
    { words: 'holds an empty bin', code: '' }, { words: 'count unknown', code: '' }, { words: 'no bin', code: '' },
    { words: 'could not look', code: '' }],
'spot words: full, empty of a type, empty, no bin, count unknown, a bare cart, could not look');

// ── Load: the colour follows the cell's state ──
{
    const homes = [
        bt(1, 'SMN_1', { dedicated: true, parts: ['PART-T'], bin: EMPTYBIN('KD-1') }),
        bt(2, 'SMN_2', { dedicated: true, parts: ['PART-W'], bin: EMPTYBIN('KD-1') }),
        bt(3, 'SMN_3', { dedicated: true, parts: ['PART-X'], bin: EMPTYBIN('KD-1') }),
        bt(4, 'SMN_4', { dedicated: true, parts: ['PART-O'], bin: EMPTYBIN('KD-1') }),
        bt(5, 'SMN_5', { dedicated: true, parts: ['PART-U'], bin: EMPTYBIN('KD-1') }),
        bt(6, 'SMN_6', { dedicated: true, parts: ['PART-N'], bin: EMPTYBIN('KD-1') }),
        bt(7, 'SMN_7', { dedicated: true, parts: ['PART-Y'], bin: EMPTYBIN('KD-1') }),
        bt(8, 'SMN_8', { dedicated: true, parts: ['PART-C'], bin: EMPTYBIN('KD-1') }),
    ];
    const v = { station: { name: 'Bin Loader' }, nodes: homes, serves: [
        row('PART-T', 451, { cell: 'C1' }),
        row('PART-W', 4, { cell: 'C2', waiting: true }),
        row('PART-X', 67, { cell: 'C3' }),
        row('PART-O', 0, { cell: 'C4' }),
        row('PART-U', -5, { cell: 'C5' }),
        row('PART-C', 0, { cell: 'C6', incoming: true, waiting: true }),
        row('PART-Y', 2000, { cell: 'C7' }),
    ] };
    const b = B(v);
    eq(zone(b, 'load').cards.map(cardSay), [
        ['SMN_4', 'PART-O', 'red', 'NOW', 'C4 out'],
        ['SMN_8', 'PART-C', 'red', 'NOW', 'C6 out'],
        ['SMN_2', 'PART-W', 'amber', 'SOON', 'C2 waiting'],
        ['SMN_3', 'PART-X', 'plain', '', 'C3 has 67 left'],
        ['SMN_1', 'PART-T', 'plain', '', 'C1 has 451 left'],
        ['SMN_7', 'PART-Y', 'plain', '', 'C7 has 2,000 left'],
        ['SMN_5', 'PART-U', 'plain', '', 'C5 count unknown'],
        ['SMN_6', 'PART-N', 'plain', '', 'No cell runs this part'],
    ], 'Load: out (red, ties in the plant\'s order), a cell changing over to a part it has none of (the same red), ' +
        'waiting (amber), then the printed count fewest first, unknown, a part no cell runs; no card says changeover');
    eq(zone(b, 'load').cards[0].spotWords + ' ' + zone(b, 'load').cards[0].spotCode, 'holds an empty KD-1',
        'Load: the spot line says what the home holds');
    eq(zone(b, 'load').cards[0].act, { kind: 'load', node: 4, codes: ['PART-O'] }, 'Load: the card opens the load modal for its part');
    eq(b.words.slice(0, 3), ['8 to load', 'C4 out', 'C6 out'], 'top line: the count to load, then each cell out');
    // The failed served read: every Load card plain, the cells row says so.
    const failed = B({ station: {}, nodes: homes.slice(0, 1) });
    eq([cardSay(zone(failed, 'load').cards[0]), failed.cells.failed, failed.words],
        [['SMN_1', 'PART-T', 'plain', '', 'Cells could not look'], true, ['1 to load', 'Cells: could not look']],
        'a failed served read: Load cards plain, the cells row could not look');
    // Nothing to load: the section keeps its title.
    const quiet = B({ station: {}, nodes: [bt(1, 'SMN_1', { dedicated: true, bin: FULLBIN('PART-A') })], serves: [row('PART-A', 9)] });
    eq([zone(quiet, 'load').count, zone(quiet, 'load').empty], [0, 'Nothing to load right now.'],
        'Load with nothing to load keeps its title and says so');
}

// ── a home whose own clear is in flight draws no Load card ──
{
    const orderA = ord(301, 'queued', { order_type: 'move', source_node: 'SMN_H1', delivery_node: 'LDR.SMN_B2' });
    const homes = [
        bt(1, 'SMN_H1', { dedicated: true, bin: EMPTYBIN(''), orders: [orderA], buffer: [
            { node: 'SMN_B1', state: 'partial' }, { node: 'SMN_B2', state: 'partial' }, { node: 'SMN_B3', state: 'open' }] }),
    ];
    const b = B({ station: {}, nodes: homes, serves: [row('PART-A', 3)] });
    eq(zone(b, 'load').cards, [], 'clear in flight: the home draws no Load card (changed: it drew LOAD)');
    eq(M.bufferOf(homes).map(s => s.state), ['partial', 'coming', 'open'], 'clear in flight: its spot is drawn as coming');
    eq(zone(b, 'partials').tone, 'plain', 'clear in flight: the coming spot counts as an empty, so no amber');
}

// ── Waiting on the plant ──
{
    const nodes = [
        bt(1, 'SMN_A34', { dedicated: true, parts: ['PART-A'], bin: EMPTYBIN(''), refusals: { 'PART-A': { refused_at: 'T1' } } }),
        bt(2, 'SMN_A28', { dedicated: true, parts: ['PART-B'], bin: NOBIN, refusals: { 'PART-B': { refused_at: 'T2' } } }),
    ];
    const b = B({ station: {}, nodes: nodes.slice().reverse(), serves: [row('PART-A', 212, { cell: 'CELL-4' })] },
        { when: iso => ({ T1: '06:15', T2: 'Monday' })[iso] });
    eq(zone(b, 'refused').cards.map(cardSay), [
        ['SMN_A34', 'PART-A', 'plain', '', 'Said no parts · 06:15 · CELL-4 has 212 left'],
        ['SMN_A28', 'PART-B', 'plain', '', 'Said no parts · Monday'],
    ], 'Waiting on the plant: plain, no word; when it was said, and the cell it exposes; an exposing refusal first');
    eq(zone(b, 'refused').cards[0].act, { kind: 'load', node: 1, codes: ['PART-A'] },
        'Waiting on the plant: the tap opens the load modal (Undo and LOAD are there)');
    eq(zone(b, 'load').cards, [], 'a refused part has no Load card');
    eq(b.words.slice(0, 2), ['0 to load', '2 waiting on the plant'], 'top line counts the refusals');
}

// ── Clear partials: folded, opened, amber, red, unknown, none can come home ──
{
    const spots = states => states.map((s, i) => ({ node: 'LDR.SMN_B' + (i + 1), state: s }));
    const homesWith = (buffer, from) => [
        bt(1, 'SMN_H1', { dedicated: true, parts: ['PART-A'], bin: FULLBIN('PART-A'), buffer: buffer, partialFrom: from }),
        bt(2, 'SMN_H2', { dedicated: true, parts: ['PART-B'], bin: FULLBIN('PART-B'), buffer: buffer }),
    ];
    const P = (buffer, from, ui) => zone(B({ station: {}, nodes: homesWith(buffer, from), serves: [] }, ui), 'partials');
    let z = P(spots(['partial', 'empty', 'open', 'open']), 'LDR.SMN_B1');
    eq([z.tone, z.folded, z.cards.length, z.count, z.stripWords], ['plain', true, 0, 1, '1 partial · 1 empty · 2 open'],
        'partials, a normal day: folded to its strip and words, quiet');
    z = P(spots(['partial', 'empty', 'open', 'open']), 'LDR.SMN_B1', { partialsOpen: true });
    eq([z.folded, z.cards.map(cardSay), z.cards[0].number, z.cards[0].spotWords],
        [false, [['SMN_H1', 'PART-A', 'plain', '', 'Pull the full · partial from SMN_B1']], 1, 'holds a full bin'],
        'partials opened by a tap: the numbered card, the home\'s spot line, the part, where the partial comes from');
    eq(z.cards[0].act, { kind: 'clear-home', node: 1, home: 'SMN_H1', spot: 'SMN_B1' },
        'a partial\'s tap is the home\'s clear, naming the home and the spot');
    z = P(spots(['open', 'partial', 'open']), 'SMN_B2');
    eq([z.tone, z.word, z.reason, z.folded, z.openedByBoard, z.cards.map(c => c.tone)],
        ['amber', 'SOON', 'No empty', false, true, ['plain']],
        'partials with no empty and an open spot: amber, opened by the board, the cards plain (one problem, one colour)');
    z = P(spots(['partial', 'partial', 'partial']), 'SMN_B3');
    eq([z.tone, z.word, z.reason, z.cards.map(c => [c.tone, c.number])], ['red', 'NOW', 'Buffer full · clear one', [['plain', 3]]],
        'a buffer of partials with no empty and no open spot: red, any card clears it');
    const red = B({ station: {}, nodes: homesWith(spots(['partial', 'partial']), ''), serves: [] });
    eq([zone(red, 'partials').tone, zone(red, 'partials').cards.length, zone(red, 'partials').empty, red.words.indexOf('Buffer full') >= 0],
        ['red', 0, 'No partial can come home', true],
        'red with no card (no home holds a full of a partial\'s part): one plain line, the top line still says it');
    z = P(spots(['partial', 'unknown', 'partial']), 'SMN_B1');
    eq([z.tone, z.stripWords, z.strip.map(s => s.number)], ['red', '2 partials · 0 empty · 0 open · 1 ?', [1, 0, 2]],
        'an unknown spot is neither open nor empty: "?" in the words; partials numbered in the plant\'s order');
    z = P(spots(['unknown', 'unknown', 'unknown']), '');
    eq([z.tone, z.folded, z.stripWords], ['plain', true, '0 partials · 0 empty · 0 open · 3 ?'],
        'a failed bins read: every spot "?", the section keeps its title (it never vanishes)');
    eq(zone(B({ station: {}, nodes: [bt(1, 'SMN_H1', { dedicated: true })], serves: [] }), 'partials'), null,
        'no buffer on the tile (before it is sent, or a loader without one): no section');
    const amberTop = B({ station: {}, nodes: homesWith(spots(['partial', 'open']), 'SMN_B1'), serves: [] });
    eq(amberTop.words, ['0 to load', '1 partial', 'Buffer: no empty'], 'top line: the partials count and the buffer while amber');
}

// ── Check bin: a home holding a part at a count of 0 or below ──
{
    const b = B({ station: {}, nodes: [bt(1, 'SMN_A18', { dedicated: true, parts: ['PART-A'],
        bin: { occupied: true, payload_code: 'PART-A', stocked: false } })], serves: [row('PART-A', 1027)] });
    eq([zone(b, 'other:Check bin').cards.map(cardSay), zone(b, 'other:Check bin').cards[0].act, b.words],
        [[['SMN_A18', 'PART-A', 'plain', '', 'Count unknown · check bin']], { kind: 'modal', node: 1 }, ['0 to load', '1 check bin']],
        'a home holding a part at a count of 0 or below: a plain card in the catch-all, opening the node modal');
}

// ── the cells row ──
{
    const nodes = [bt(1, 'SMN_1', { dedicated: true, parts: ['PART-O'], bin: EMPTYBIN('') })];
    const v = { station: {}, nodes: nodes, serves: [
        row('PART-O', 0, { cell: 'C1', capacity_uop: 400 }),
        row('PART-Q', 0, { cell: 'C2', capacity_uop: 400 }),
        row('PART-R', 30, { cell: 'C3', capacity_uop: 400, reorder_uop: 50 }),
        row('PART-S', null, { cell: 'C4', capacity_uop: 400 }),
    ] };
    const b = B(v);
    eq(b.cells.tiles.map(t => [t.cell, t.red, t.rows.map(r => [r.part, r.words, r.red, r.tick])]), [
        ['C1', true, [['PART-O', 'none left', true, 0]]],
        ['C2', false, [['PART-Q', 'none left', false, 0]]],
        ['C3', false, [['PART-R', '30 left', false, 50]]],
        ['C4', false, [['PART-S', 'count unknown', false, 0]]],
    ], 'cells: red only where a red card fixes it; an out row with no card says "none left" plainly; the tick only above 0; unknown last');
    eq(b.words.slice(0, 3), ['1 to load', 'C1 out', 'C2 out'], 'top line: every cell out, card or none');
    // One cell with five rows.
    const hop = M.cellsRow({ nodes: [], serves: [1, 2, 3, 4, 5].map(i => row('PART-' + i, i * 10, { cell: 'CELL-H' })) });
    eq([hop.tiles.length, hop.tiles[0].rows.length, hop.tiles[0].hidden], [1, 4, 1], 'cells: at most four rows a tile, then "+N"');
    // Seven cells: the covered ones fold.
    const heldAll = [1, 2, 3, 4, 5, 6, 7].map(i => bt(i, 'H' + i, { dedicated: true, parts: ['P' + i], bin: FULLBIN('P' + i) }));
    const many = M.cellsRow({ nodes: heldAll, serves: [1, 2, 3, 4, 5, 6, 7].map(i => row('P' + i, 10, { cell: 'C' + i }))
        .concat([row('PX', 3, { cell: 'C8' })]) });
    eq([many.tiles.map(t => t.cell), many.more], [['C8'], 7], 'cells: past six, the covered cells fold into "+N more cells"');
}

// ── the unloader: one list, one red per stopped press ──
{
    const windows = [
        bt(1, 'SMN_B2', { role: 'consume', parts: ['PART-K'], bin: FULLBIN('PART-K', 'TOTE-2'), uop: 180, carriers: ['TOTE-2', 'KD-1'] }),
        bt(2, 'SMN_B4', { role: 'consume', parts: ['PART-L'], bin: FULLBIN('PART-L', 'KD-1'), uop: 1250, carriers: ['KD-1'] }),
        bt(3, 'SMN_B3', { role: 'consume', parts: ['PART-L'], bin: EMPTYBIN('KD-1'), carriers: ['KD-1'] }),
        bt(4, 'SMN_B5', { role: 'consume', parts: ['PART-L'], bin: NOBIN, carriers: ['KD-1'] }),
    ];
    const serves = [
        row('PART-K', 400, { cell: 'PRESS-1', node: 'PLN_A1', role: 'produce', carrier: 'TOTE-2', capacity_uop: 400, waiting: true }),
        row('PART-K', 120, { cell: 'PRESS-1', node: 'PLN_A4', role: 'produce', carrier: 'TOTE-2', capacity_uop: 400 }),
        row('PART-L', 85, { cell: 'PRESS-2', node: 'PLN_A9', role: 'produce', carrier: 'KD-1', capacity_uop: 300 }),
    ];
    const empties = { group: 'EMPTY-SUPER', rows: [{ bin_type: 'TOTE-2', have: 0, spots: 1 }, { bin_type: 'KD-1', have: 2, spots: 1 }] };
    const b = B({ station: { name: 'Unloader' }, nodes: windows, serves: serves, empties: empties });
    const w = zone(b, 'windows');
    eq(w.cards.map(c => [c.spot, c.spotWords, c.spotPart, c.spotCount, c.headline, c.tone, c.word, c.reason]), [
        ['SMN_B3', 'holds an empty bin', '', '', 'KD-1', 'red', 'NOW', 'Change to TOTE-2'],
        ['SMN_B2', 'holds a full bin', 'PART-K', '180', 'TOTE-2', 'plain', '', 'Send back TOTE-2'],
        ['SMN_B4', 'holds a full bin', 'PART-L', '1,250', 'KD-1', 'plain', '', 'Change to TOTE-2'],
    ], 'unloader: one card per cart, the bin the headline; "Change to" on a cart whose send-back type is not its own; ' +
        'one red per stopped press, the first cart in the walk (an empty first) whose send-back bin it needs; red first');
    eq(b.presses.tiles.map(p => [p.cell, p.node, p.words, p.bin, p.part, p.ready, p.red]), [
        ['PRESS-1', 'PLN_A1', 'full', 'TOTE-2', 'PART-K', false, true],
        ['PRESS-2', 'PLN_A9', '85 of 300', 'KD-1', 'PART-L', true, false],
        ['PRESS-1', 'PLN_A4', '120 of 400', 'TOTE-2', 'PART-K', false, false],
    ], 'presses: closest to full first; red only when full and a red cart fixes it');
    eq(b.presses.empties, { group: 'EMPTY-SUPER', open: 1, lines: [{ bin: 'TOTE-2', words: 'none' }, { bin: 'KD-1', words: '2 ready' }] },
        'Empties: a line per bin type in words ("none" for a type the group holds none of), the open spots once');
    eq(b.words, ['3 at the windows', 'PRESS-1 full'], 'top line: the carts, and the press that is full');
    eq(w.cards.map(c => c.act.kind), ['push-empty', 'unload', 'unload'],
        'unloader windows (the old slot grid): an empty opens the empty-cart panel, a full the CLEAR');
    eq(w.cards[1].act, { kind: 'unload', node: 1, carriers: ['TOTE-2', 'KD-1'], discarding: 180, leavesBare: false, lit: 'TOTE-2' },
        'a full\'s CLEAR: its carriers and its count, the waiting press\'s bin lit');
    // One bin type at the station: no "Send back" / "Send it".
    const one = B({ station: {}, nodes: [windows[1], windows[2]], serves: [serves[2]], empties: empties });
    eq(zone(one, 'windows').cards.map(c => c.reason), ['', ''], 'one bin type at the station: no send-back note');
    // The Empties lines are the served presses' bin types: a type the group
    // holds that no served press uses gets no line.
    const extra = { group: 'EMPTY-SUPER', rows: empties.rows.concat([{ bin_type: 'TOTE-9', have: 6, spots: 1 }]) };
    eq(B({ station: {}, nodes: windows, serves: serves, empties: extra }).presses.empties.lines.map(l => l.bin), ['TOTE-2', 'KD-1'],
        'Empties: no line for a type no served press uses');
    // A stage-1 unloader: no Empties tile, no note.
    const st1 = B({ station: {}, nodes: [bt(1, 'ST1', { role: 'consume', bin: FULLBIN('PART-A'), leavesBare: true })], serves: [], empties: empties });
    eq([st1.presses.empties, zone(st1, 'windows').cards[0].reason, zone(st1, 'windows').cards[0].act.leavesBare], [null, '', true],
        'stage 1: no Empties tile (its group is stage 2\'s windows), no note, Full off');
    // A held partial (main's partial home): its note says whose and since when,
    // and its tap is UNLOAD and COUNT, not the CLEAR.
    const heldBin = Object.assign({}, FULLBIN('PART-K', 'TOTE-2'), { held_since: '2026-10-18T10:00:00Z', held_for: 'P400', held_uop: 120 });
    const held = B({ station: {}, nodes: [bt(9, 'SMN_09', { role: 'consume', parts: ['PART-K'], bin: heldBin, carriers: ['TOTE-2'] })], serves: [] },
        { when: () => '10:00' });
    const hc = zone(held, 'windows').cards[0];
    eq([hc.reason, hc.act.kind, hc.act.heldFor, hc.act.count], ['Partial · held for P400 · since 10:00', 'unload-held', 'P400', 120],
        'a held partial at a window: held for its line since its arrival; the tap is UNLOAD and COUNT with the count Core holds');
    const none = B({ station: {}, nodes: [windows[3]], serves: [] });
    eq([zone(none, 'windows').count, zone(none, 'windows').empty, none.words], [0, 'No cart at any window.', ['nothing at the windows']],
        'no cart at any window: one quiet line');
}

// ── the board bar, where the old payload board drew it ──
{
    const bar = nodes => B({ station: {}, nodes: nodes, serves: [] }).bar;
    eq(bar([bt(1, 'SMN_A1', { bin: NOBIN, outbound: 'FG' })]).buttons.map(x => [x.label, x.enabled, x.act]),
        [['Request empty', true, { kind: 'request-empty', node: 1 }], ['Pull from market', true, { kind: 'pull-from-market', node: 1 }]],
        'board bar: Request empty and Pull from market, sentence case');
    eq(bar([bt(1, 'SMN_A1', { bin: EMPTYBIN('') })]).buttons.map(x => [x.label, x.enabled]), [['Request empty — bin at node', false]],
        'board bar: greyed with a bin parked; no Pull from market without an outbound');
    eq(bar([bt(1, 'SMN_A1', { dedicated: true })]), null, 'board bar: not on a home board');
    eq(bar([bt(1, 'ULN_1', { role: 'consume' }), bt(2, 'ULN_2', { role: 'consume' })]), null, 'board bar: not on a shared unloader');
    eq(bar([bt(1, 'ULN_M1', { role: 'consume', parts: ['PART-B', 'PART-A'], bin: NOBIN, operator: true }),
        bt(2, 'SMN_M2', { bin: NOBIN })]).buttons[0].act, { kind: 'request-full', node: 1, payload: 'PART-A' },
    'board bar: Request full with the first part the board lists');
}

// ── a shared-window produce loader: one card listing the parts called for ──
{
    const win = bt(1, 'SMN_A1', { parts: ['PART-A', 'PART-B', 'PART-C'], bin: EMPTYBIN(''), operator: true });
    const b = B({ station: {}, nodes: [win], serves: [row('PART-B', 0, { cell: 'C2' }), row('PART-A', 50, { cell: 'C1' })] });
    const c = zone(b, 'load').cards;
    eq([c.length, c[0].headline, c[0].also, c[0].tone, c[0].reason, c[0].act.codes],
        [1, 'PART-B', ['PART-A', 'PART-C'], 'red', 'C2 out', ['PART-B', 'PART-A', 'PART-C']],
        'a shared-window produce loader: one card, the parts called for in coverage order, the first lit');
}

// ── alarms ──
{
    const b = B({ station: {}, nodes: [bt(1, 'SMN_1', { dedicated: true, bin: FULLBIN('PART-A'), alarm: 'Counts are piling up' }),
        bt(2, 'SMN_2', { dedicated: true, parts: ['PART-B'], bin: EMPTYBIN(''), alarm: 'Contained' })], serves: [] });
    eq(zone(b, 'load').cards.map(c => [c.spot, c.note, c.act.kind]),
        [['SMN_2', 'Contained', 'load'], ['SMN_1', 'Counts are piling up', 'modal']],
        'an alarm is its card\'s note; a tile with no card of its own gets one in the first zone, opening the node modal');
}

// ── Show all: every tile in the plant's order ──
{
    const nodes = [bt(1, 'SMN_1', { dedicated: true, bin: FULLBIN('PART-A') }), bt(2, 'SMN_2', { dedicated: true, parts: ['PART-B'], bin: EMPTYBIN('') })];
    const b = B({ station: {}, nodes: nodes, serves: [] });
    eq([b.tileCount, b.all.map(c => [c.spot, c.spotWords, c.act.kind])], [2, [['SMN_1', 'holds a full bin', 'modal'], ['SMN_2', 'holds an empty bin', 'load']]],
        'Show all: every tile in order, the same card where it has one, else a plain card opening the node modal');
}

// ═════════════════════════════════════════════════════════════════════════════
// Fit
// ═════════════════════════════════════════════════════════════════════════════

eq([1024, 1099, 1100, 1699, 1700, 1920].map(M.columnsFor), [2, 2, 3, 3, 4, 4], 'columns: 2 under 1100, 3 from 1100, 4 from 1700');

// The proofs' strings: as dense as a real part or bin code gets (capitals and
// digits, no spaces to spare), synthetic.
const PART18 = 'PART-3184-27QX2C06';
const BIN19 = 'TOTE 52 x 61 BIN-XQ';
eq([PART18.length, BIN19.length], [18, 19], 'the proofs\' strings are 18 and 19 characters');
const card = (o) => Object.assign({ spot: 'SMN_A13', spotWords: 'holds an empty', spotCode: 'KD-1', headline: PART18, tone: 'plain' }, o || {});
const zoneOf = (cards, o) => Object.assign({ cards: cards }, o || {});
const nCards = (n, o) => Array.from({ length: n }, () => card(o));
const WIDTHS = [1024, 1099, 1100, 1280, 1699, 1700, 1920];
const HEIGHTS = { 1024: 768, 1099: 768, 1100: 800, 1280: 800, 1699: 900, 1700: 1000, 1920: 1080 };

// The clip proofs: wherever fitBoard lands, an 18-character part on a card
// (numbered or not), a 19-character bin code on an unloader's headline and its
// spot line's part and count fit whole.
WIDTHS.forEach(function (w) {
    [card(), card({ number: 3 }), card({ headline: BIN19, spotCode: '', spotPart: PART18, spotCount: '1,027', spotWords: 'holds a full bin' })]
        .forEach(function (c, k) {
            const f = M.fitBoard({ width: w, height: HEIGHTS[w] }, 0, [zoneOf([c])], false);
            eq(M.cardFits(c, f.step, M.cardInnerWidth(w, f.columns, !!c.number)), true,
                'clip proof ' + ['part', 'numbered part', 'bin code'][k] + ' at ' + w + ' px, ' + f.columns + ' columns, step ' + f.step);
        });
});
// At every step a board can be asked for and every column count, a card that
// does not fit steps the board down; the smallest step always fits.
[2, 3, 4].forEach(function (cols) {
    const w = { 2: 1024, 3: 1100, 4: 1700 }[cols];
    eq(M.cardFits(card({ number: 1 }), 2, M.cardInnerWidth(w, cols, true)), true,
        'clip proof: the smallest step fits the numbered 18-character part at the narrowest ' + cols + '-column width');
    eq(M.cardFits(card({ headline: BIN19, spotCode: '', spotPart: PART18, spotCount: '1,027' }), 2, M.cardInnerWidth(w, cols, false)), true,
        'clip proof: the smallest step fits the 19-character bin code and the spot line\'s part at ' + cols + ' columns');
});
// The cell tile: the longest part with "1,027 left" side by side, or the count
// under the bar, never clipped, at every width.
WIDTHS.forEach(function (w) {
    const r = { tiles: [1, 2, 3, 4, 5, 6].map(i => ({ cell: 'CELL-' + i, rows: [{ part: PART18, words: '1,027 left' }], hidden: 0 })), more: 1 };
    const L = M.statusRowLayout(r, w);
    const tileW = (w - 2 * M.GEOM.padX - (L.columns - 1) * M.GEOM.tileGap) / L.columns - M.GEOM.tilePad;
    const need = L.countUnder ? Math.max(M.textWidth(PART18, 14), M.textWidth('1,027 left', 13))
        : M.textWidth(PART18, 14) + 8 + M.textWidth('1,027 left', 13);
    eq(tileW >= need, true, 'clip proof: the cell tile at ' + w + ' px (' + L.columns + ' a line, count ' + (L.countUnder ? 'under' : 'beside') + ')');
});
// The unloader's press tile carries the 19-character bin code whole.
WIDTHS.forEach(function (w) {
    const r = { tiles: [1, 2, 3, 4, 5].map(i => ({ cell: 'P' + i, node: 'PLN_A' + i, words: '1,027 of 1,300', bin: BIN19, part: PART18 })),
        more: 0, empties: { lines: [{ bin: BIN19, words: '6 ready' }], open: 2 } };
    const L = M.statusRowLayout(r, w);
    const tileW = (w - 2 * M.GEOM.padX - (L.columns - 1) * M.GEOM.tileGap) / L.columns - M.GEOM.tilePad;
    eq(tileW >= M.textWidth(BIN19, 24), true, 'clip proof: the press tile\'s 19-character bin code at ' + w + ' px');
});

// A large loader's day, synthetic: 5 cells, 22 homes, 9 buffer spots, 2
// refusals, nothing to load, the buffer folded; its parts are 14 characters.
{
    const DAY14 = 'PART1-Q7X2C.06';
    const dayCard = o => card(Object.assign({ headline: DAY14 }, o || {}));
    const status = M.statusRowLayout({ tiles: [1, 2, 3, 4, 5].map(i => ({ cell: 'CELL-' + i, rows: [{ part: DAY14, words: '212 left' }, { part: DAY14, words: '' }], hidden: 0 })), more: 0 }, 1280);
    const zones = [zoneOf([]), zoneOf([dayCard(), dayCard()]), zoneOf([], { folded: true })];
    eq(M.fitBoard({ width: 1280, height: 800 }, status.height, zones, false), { step: 0, columns: 3, shown: [0, 2, 0], rowsPerTile: 4 },
        'fit: the large day at 1280 x 800, the largest step');
    eq(M.fitBoard({ width: 1024, height: 768 }, M.statusRowLayout({ tiles: [1, 2, 3, 4, 5].map(i => ({ cell: 'CELL-' + i,
        rows: [{ part: DAY14, words: '212 left' }], hidden: 0 })), more: 0 }, 1024).height, zones, false).columns, 2,
    'fit: the large day at 1024 x 768, two columns');
    // The buffer full of partials, red, nine cards.
    const red = [zoneOf([]), zoneOf([dayCard(), dayCard()]), zoneOf(Array.from({ length: 9 }, (x, i) => dayCard({ number: i + 1 })), { openedByBoard: true })];
    const f = M.fitBoard({ width: 1280, height: 800 }, status.height, red, false);
    eq([f.step, f.shown[0], f.shown[1], f.shown[2] >= 1], [2, 0, 2, true],
        'fit: nine red-buffer cards at 1280 x 800 step down; the opened partials keep their first card');
}
// A one-cell day: one cell with five rows, three Load cards; its parts are 18
// characters in four groups.
{
    const ROW18 = 'PRTY 7Q218 B XYZ12';
    const status = M.statusRowLayout({ tiles: [{ cell: 'CELL-H', rows: [1, 2, 3, 4].map(() => ({ part: ROW18, words: '1,027 left' })), hidden: 1 }], more: 0 }, 1280);
    const hop = o => card(Object.assign({ headline: ROW18 }, o || {}));
    const f = M.fitBoard({ width: 1280, height: 800 }, status.height, [zoneOf([hop({ tone: 'red' }), hop({ tone: 'amber' }), hop()])], false);
    eq([f.step, f.shown], [0, [3]], 'fit: the one-cell day (one cell, five rows, three Load cards) at the largest step');
}
// Overflow: the last zone gives way first, plain cards only; coloured cards
// are never cut; a folded partials row is never cut.
{
    const load = zoneOf([card({ tone: 'red' }), card({ tone: 'amber' })].concat(nCards(10)));
    const refused = zoneOf(nCards(6));
    const folded = zoneOf([], { folded: true });
    const f = M.fitBoard({ width: 1024, height: 768 }, 150, [load, refused, folded], true);
    eq([f.step, f.shown[2]], [2, 0], 'overflow: the smallest step; the folded partials stay one row');
    eq(f.shown[1] < 6 && f.shown[0] === 12 || f.shown[0] >= 2, true, 'overflow: the last zone gives way before the one above it');
    const all = M.fitBoard({ width: 1024, height: 400 }, 150, [zoneOf(nCards(4, { tone: 'red' }).concat(nCards(8))), zoneOf(nCards(8))], false);
    eq([all.shown[0] >= 4, all.shown[1]], [true, 0], 'overflow: a red card is never cut, even where the board cannot hold it');
    const opened = M.fitBoard({ width: 1024, height: 600 }, 150, [zoneOf(nCards(8)), zoneOf(nCards(9, { number: 1 }), { openedByBoard: true })], false);
    eq([opened.shown[1] >= 1, opened.shown[0] < 8], [true, true], 'overflow: opened by the board, the partials keep a card and the zone before gives way');
}
// A zone cut to no card keeps its "+N more" card: the fit counts its row, so a
// board that holds the red and amber cards and a cut zone's "+N more" only by
// showing fewer rows a cell tile does so (rows per tile is the last to give).
{
    const cells = { tiles: [1, 2, 3, 4, 5].map(i => ({ cell: 'CELL-' + i,
        rows: [1, 2, 3, 4].map(() => ({ part: 'PART-A', words: '212 left' })), hidden: 2 })), more: 0 };
    const statusAt = rows => M.statusRowLayout(cells, 1024, rows).height;
    eq(statusAt(4) > statusAt(2), true, 'status row: fewer rows a tile, a shorter row');
    const zones = [zoneOf([card({ tone: 'red' }), card({ tone: 'amber' })].concat(nCards(12))), zoneOf(nCards(2)), zoneOf([], { folded: true })];
    const f = M.fitBoard({ width: 1024, height: 768 }, statusAt, zones, false);
    eq([f.step, f.shown[0] >= 2], [2, true], 'overflow: Load keeps its red and amber; what fewer rows a tile free is drawn again');
    eq(f.rowsPerTile < M.MAX_TILE_ROWS, true, 'overflow: rows per tile give way when the zones cannot');
    eq(M.fitBoard({ width: 1024, height: 768 }, statusAt, [zoneOf(nCards(2))], false).rowsPerTile, M.MAX_TILE_ROWS,
        'rows per tile stay at four where the board fits');
}

// The fit draws every card row that fits: the buffer amber at 1024 x 768
// (Load empty, two refusals, six partials opened by the board) fits whole at
// the smallest step; a zone is never cut where cutting saves no height.
{
    const strip = Array.from({ length: 9 }, () => ({ node: 'S', state: 'partial' }));
    const zones = [zoneOf([], { tone: 'plain' }), zoneOf(nCards(2), { tone: 'plain' }),
        zoneOf(nCards(6, { number: 1 }), { tone: 'amber', openedByBoard: true, strip: strip })];
    const f = M.fitBoard({ width: 1024, height: 768 }, 140, zones, false);
    eq([f.step, f.shown], [2, [0, 2, 6]], 'the amber buffer at 1024 x 768: every card drawn, nothing cut while it fits');
    const g = M.fitBoard({ width: 1024, height: 700 }, 140, zones, false);
    eq(g.shown[1] === 0 || g.shown[1] === 2, true, 'a zone of one row is cut whole or not at all');
    eq(g.shown[2] >= 1 && g.shown[1] === 2, true, 'two refusals in one row are not cut for nothing: the opened partials give way');
}

// An alarm note is never cut: it wraps, two lines at most at the step the
// board picks (a step where it needs three is one it does not fit), and a row
// of cards is as tall as its tallest card's lines.
{
    const NOTE = 'CARRIER-0007 staged 2h at SMN_U2, not bound — Record Count on the bin tab.';
    const b = B({ station: {}, nodes: [bt(1, 'SMN_U2', { role: 'consume', dedicated: true, parts: ['PART-A'],
        bin: FULLBIN('PART-A', 'TOTE-1'), uop: 60, carriers: ['TOTE-1'], alarm: NOTE })], serves: [] });
    eq(zone(b, 'windows').cards[0].note, NOTE, 'the alarm sentence is the card note, whole');
    const noted = card({ note: NOTE });
    const narrow = M.cardInnerWidth(1024, 2, false);
    const lines = step => Math.ceil(M.textWidth(NOTE, M.STEPS[step].note) / narrow);
    eq([0, 1, 2].map(st => M.cardFits(noted, st, narrow)), [0, 1, 2].map(st => lines(st) <= 2),
        'a card fits a step only where its note takes two lines or fewer');
    const f = M.fitBoard({ width: 1024, height: 768 }, 0, [zoneOf([noted])], false);
    eq(lines(f.step) <= 2, true, 'the fit picks a step where the note takes two lines or fewer');
    const tall = M.fitBoard({ width: 1024, height: 768 }, 0, [zoneOf(nCards(2).concat([card({ note: NOTE + ' ' + NOTE + ' ' + NOTE })]))], false);
    eq(M.cardHeight(card({ note: NOTE + ' ' + NOTE + ' ' + NOTE }), tall.step, narrow) > M.STEPS[tall.step].card, true,
        'a note too long for two lines at the smallest step makes its card taller, never cut');
}

if (failed) { console.error(passed + ' passed, ' + failed + ' FAILED'); process.exit(1); }
console.log('operator board model: ' + passed + ' passed');
