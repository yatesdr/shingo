// Unit tests for the loader board's two station settings (operator-render.js):
// idleCardHidden (board_hides_idle) and allowedPayloadsFor (board_shows_all_parts).
// Both used to be read off operator_driven, so an operator-ordered station
// could never hide a loaded home; Hopkinsville's dedicated loader showed all
// seven homes after every load.
//
// Runs under plain Node (no npm): extracts the shipping functions in a vm.
// Exit 0 = pass, 1 = any failure. Run via operator_board_settings_test.go.

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

function extractFn(src, name) {
    const start = src.indexOf('function ' + name + '(');
    if (start < 0) throw new Error('function ' + name + ' not found');
    const open = src.indexOf('{', start);
    let depth = 0;
    for (let j = open; j < src.length; j++) {
        if (src[j] === '{') depth++;
        else if (src[j] === '}') {
            depth--;
            if (depth === 0) return src.slice(start, j + 1);
        }
    }
    throw new Error('unbalanced braces in ' + name);
}

const src = fs.readFileSync(path.join(__dirname, 'operator-render.js'), 'utf8');
const ctx = {};
vm.createContext(ctx);
vm.runInContext(extractFn(src, 'idleCardHidden') + '\n' + extractFn(src, 'allowedPayloadsFor'), ctx);
const hidden = ctx.idleCardHidden, allowed = ctx.allowedPayloadsFor;

const idle = { cls: 'os-board-nodemand' };
const busy = { cls: 'os-board-queued' };
const loaded = { occupied: true, payload_code: 'PART-A' };
const none = { occupied: false };

// ── idleCardHidden ──
eq(hidden({ board_hides_idle: true, operator_driven: true, bin_state: loaded }, idle), true,
    'operator-ordered station set to hide: a loaded home with nothing ordered is left off');
eq(hidden({ board_hides_idle: true, operator_driven: true, bin_state: none }, idle), false,
    'operator-ordered: an empty spot stays, it is where the operator orders the next empty');
eq(hidden({ board_hides_idle: true, operator_driven: true }, idle), false,
    'operator-ordered: no bin state reads as an empty spot and stays');
eq(hidden({ board_hides_idle: true, operator_driven: false, bin_state: none }, idle), true,
    'threshold station set to hide: an empty idle spot is left off, Core orders its empty');
eq(hidden({ board_hides_idle: true, operator_driven: true, bin_state: loaded }, busy), false,
    'a card with something to do is never hidden');
eq(hidden({ board_hides_idle: false, operator_driven: false, bin_state: loaded }, idle), false,
    'setting off: every card stays, whatever the station orders by');

// ── allowedPayloadsFor ──
const claim = { allowed_payload_codes: ['CLAIM-X'] };
const lists = { active_claim: claim, active_style_payloads: ['B'], all_style_payloads: ['C', 'A', 'B'] };
eq(allowed(Object.assign({ board_shows_all_parts: true }, lists)), ['B', 'A', 'C'],
    'every part listed, the running style\'s first, then alphabetical');
eq(allowed(Object.assign({ board_shows_all_parts: false }, lists)), ['B'],
    'setting off: only what a running style needs');
eq(allowed({ active_claim: claim, board_shows_all_parts: false, active_style_payloads: [] }), ['CLAIM-X'],
    'no style list: falls back to the claim');

console.log('operator board settings: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
