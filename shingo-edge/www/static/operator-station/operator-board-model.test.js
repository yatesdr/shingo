// Unit tests for operator-board-model.js: the board's layout from its tiles,
// the parts a loader tile draws, coverage, the changeover instruction, the
// CLEAR panel's highlight and a window's words.
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

const ctx = vm.createContext({});
vm.runInContext(fs.readFileSync(path.join(__dirname, 'operator-board-model.js'), 'utf8')
    .replace(/^export\s+/gm, ''), ctx);
const M = ctx;

function tile(role, o) {
    o = o || {};
    const t = {
        node: { id: o.id || 1, name: o.name || 'W1' },
        loader: {
            role: role, layout: o.dedicated ? 'dedicated_positions' : 'shared_window',
            replenishment: o.operator ? 'operator' : 'threshold', outbound_dest: '',
            parts: (o.parts || ['PART-A']).map(c => ({ code: c })),
            directive: !!o.directive, leaves_bare: false, pulled_directly: false,
            board_hides_idle: !!o.hidesIdle, board_shows_all_parts: !!o.allParts,
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

// partState: ACTIVE, the downtime scope, lineside summed over running rows, short by coverage.
let view = { nodes: [t2], serves: [row('PART-A', 30), row('PART-A', 5, { cell: 'CELL-2' }), row('PART-A', 9, { incoming: true, waiting: true })] };
eq(M.partState(t2, 'PART-A', view), { active: true, inScope: true, lineside: 35, short: true },
    'a running part with no stocked bin on the board: lineside summed, short');
eq(M.partState(t2, 'PART-B', view), { active: false, inScope: false, lineside: 0, short: false },
    'a part no running style uses: not ACTIVE, a queued call is not downtime');
eq(M.partState(t2, 'PART-B', { nodes: [t2] }), { active: false, inScope: true, lineside: 0, short: false },
    'no serves: a queued call is downtime (the board could not look)');
const held = tile('produce', { parts: ['PART-A'], bin: stocked('PART-A') });
eq(M.partState(held, 'PART-A', { nodes: [held], serves: [row('PART-A', 30)] }).short, false,
    'a stocked bin of the part on the board: covered, not short');

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
const waitingTile = tile('produce', { parts: ['PART-B'] });
eq(M.partState(waitingTile, 'PART-B', { nodes: [waitingTile], serves: [row('PART-B', 4, { waiting: true })] }).short, true,
    'a running part waiting at a served cell is marked short');
eq(M.coverage({ nodes: [], serves: [row('PART-P', 3, { role: 'produce', carrier: 'TOTE-1' })] }).map(c => c.state), ['exposed'],
    'a produce row on an unloader: ready from the empties, absent here');
eq(M.coverage({ nodes: [], serves: [row('PART-P', 3, { role: 'produce', carrier: 'TOTE-1' })],
    empties: { rows: [{ bin_type: 'TOTE-1', have: 2 }] } }).map(c => c.state), ['covered'],
'a produce row on an unloader with an empty of its type: covered');
eq(M.coverage({}), [], 'no serves: no coverage');

// ── the changeover instruction ──
const ld = tile('produce', { directive: true });
view = { serves: [
    row('PART-B', 0, { waiting: true, carrier: 'TOTE-2', node: 'WLN_2', incoming: true }),
    row('PART-A', 3, { waiting: true, carrier: 'TOTE-1', node: 'WLN_1' }),
    row('PART-C', 3, { carrier: 'TOTE-3', node: 'WLN_3', incoming: true }),
    row('PART-D', 0, { waiting: true, carrier: '', node: 'WLN_4' }),
    row('PART-E', 0, { waiting: true, carrier: 'TOTE-5', node: 'PLN_5', role: 'produce' }),
] };
eq(M.changeoverInstruction(ld, view), { bin_type_codes: ['TOTE-1', 'TOTE-2'], payload_codes: ['PART-A', 'PART-B'], for_nodes: ['WLN_1', 'WLN_2'] },
    'instruction: the waiting rows of the board\'s role with a bin type, sorted; one rule for running and incoming rows');
eq(M.changeoverInstruction(tile('produce'), view), null, 'instruction: the directive setting off draws nothing');
eq(M.changeoverInstruction(ld, { serves: [row('PART-C', 3, { carrier: 'TOTE-3' })] }), null, 'instruction: nothing waiting draws nothing');
eq(M.changeoverInstruction(tile('consume', { directive: true }), view), { bin_type_codes: ['TOTE-5'], payload_codes: ['PART-E'], for_nodes: ['PLN_5'] },
    'instruction on an unloader: the waiting produce rows');

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

if (failed) { console.error(passed + ' passed, ' + failed + ' FAILED'); process.exit(1); }
console.log('operator board model: ' + passed + ' passed');
