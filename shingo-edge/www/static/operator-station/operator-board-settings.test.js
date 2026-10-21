// Unit tests for the loader board's all-parts setting: cardParts
// (operator-board-model.js) reads loader.board_shows_all_parts. It used to be
// read off the loader's replenishment, so a station could not list every part
// without being operator-ordered. The other setting, board_hides_idle, has no
// reader on this board: it draws a card only for work, so nothing is idle.
//
// Runs under plain Node (no npm): the model runs as it ships in a vm with its
// `export`s and imports dropped. Exit 0 = pass, 1 = any failure. Run via
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

const ctx = vm.createContext({});
vm.runInContext(fs.readFileSync(path.join(__dirname, 'operator-board-model.js'), 'utf8')
    .replace(/^import[^\n]*\n/gm, '').replace(/^export\s+/gm, ''), ctx);
const cardParts = ctx.cardParts;

function tile(o) {
    return {
        node: { id: 1, name: 'H1' },
        loader: {
            role: 'produce', layout: 'dedicated_positions',
            replenishment: o.operator ? 'operator' : 'threshold',
            parts: (o.parts || ['PART-A']).map(c => ({ code: c })),
            board_shows_all_parts: !!o.allParts,
        },
        orders: [],
    };
}

const row = (part) => ({ role: 'consume', part: part, incoming: false });
eq(cardParts(tile({ allParts: true, parts: ['C', 'A', 'B'] }), [row('B')]), ['B', 'A', 'C'],
    'every part listed, the running style\'s first, then alphabetical');
eq(cardParts(tile({ allParts: false, parts: ['C', 'A', 'B'] }), [row('B')]), ['B'],
    'setting off: only what a running style needs');
eq(cardParts(tile({ allParts: false, operator: true, parts: ['C', 'A', 'B'] }), [row('B')]), ['B'],
    'setting off on an operator-ordered station: only what a running style needs');
eq(cardParts(tile({ allParts: true, operator: false, parts: ['C', 'A'] }), []), ['A', 'C'],
    'setting on at a threshold station: every part, by code');
eq(cardParts(tile({ allParts: false, parts: ['C', 'A'] }), []), ['C', 'A'],
    'setting off, nothing running: every part, in the loader\'s order');

console.log('operator board settings: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
