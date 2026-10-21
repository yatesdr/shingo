// Unit tests for the loader board's two station settings: idleCardHidden
// (operator-render.js, loader.board_hides_idle) and cardParts
// (operator-board-model.js, loader.board_shows_all_parts). Both used to be
// read off the loader's replenishment, so an operator-ordered station could
// never hide a loaded home; Hopkinsville's dedicated loader showed all seven
// homes after every load.
//
// Runs under plain Node (no npm): the model runs as it ships in a vm with its
// `export`s dropped, and idleCardHidden is extracted from the render file into
// the same context. Exit 0 = pass, 1 = any failure. Run via
// operator_board_settings_test.go.

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

const ctx = vm.createContext({});
vm.runInContext(fs.readFileSync(path.join(__dirname, 'operator-board-model.js'), 'utf8')
    .replace(/^import[^\n]*\n/gm, '').replace(/^export\s+/gm, ''), ctx);
const render = fs.readFileSync(path.join(__dirname, 'operator-render.js'), 'utf8');
vm.runInContext(extractFn(render, 'idleCardHidden'), ctx);
const hidden = ctx.idleCardHidden, cardParts = ctx.cardParts;

function tile(o) {
    return {
        node: { id: 1, name: 'H1' },
        loader: {
            role: 'produce', layout: 'dedicated_positions',
            replenishment: o.operator ? 'operator' : 'threshold',
            parts: (o.parts || ['PART-A']).map(c => ({ code: c })),
            board_hides_idle: !!o.hidesIdle, board_shows_all_parts: !!o.allParts,
        },
        bin_state: o.bin,
        orders: [],
    };
}

const idle = { cls: 'os-board-nodemand' };
const busy = { cls: 'os-board-queued' };
const loaded = { occupied: true, payload_code: 'PART-A' };
const none = { occupied: false };

// ── idleCardHidden ──
eq(hidden(tile({ hidesIdle: true, operator: true, bin: loaded }), idle), true,
    'operator-ordered station set to hide: a loaded home with nothing ordered is left off');
eq(hidden(tile({ hidesIdle: true, operator: true, bin: none }), idle), false,
    'operator-ordered: an empty spot stays, it is where the operator orders the next empty');
eq(hidden(tile({ hidesIdle: true, operator: true }), idle), false,
    'operator-ordered: no bin state reads as an empty spot and stays');
eq(hidden(tile({ hidesIdle: true, operator: false, bin: none }), idle), true,
    'threshold station set to hide: an empty idle spot is left off, Core orders its empty');
eq(hidden(tile({ hidesIdle: true, operator: true, bin: loaded }), busy), false,
    'a card with something to do is never hidden');
eq(hidden(tile({ hidesIdle: false, operator: false, bin: loaded }), idle), false,
    'setting off: every card stays, whatever the station orders by');
eq(hidden({ node: { id: 2 } }, idle), false, 'a tile that is no loader\'s is never hidden by the setting');

// ── cardParts: board_shows_all_parts ──
const row = (part) => ({ role: 'consume', part: part, incoming: false });
eq(cardParts(tile({ allParts: true, parts: ['C', 'A', 'B'] }), [row('B')]), ['B', 'A', 'C'],
    'every part listed, the running style\'s first, then alphabetical');
eq(cardParts(tile({ allParts: false, parts: ['C', 'A', 'B'] }), [row('B')]), ['B'],
    'setting off: only what a running style needs');
eq(cardParts(tile({ allParts: false, parts: ['C', 'A'] }), []), ['C', 'A'],
    'setting off, nothing running: every part, in the loader\'s order');

console.log('operator board settings: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
