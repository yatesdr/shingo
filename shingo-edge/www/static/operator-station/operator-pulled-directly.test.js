// Unit tests for the board's note (operator-board-model.js boardModel): the card's
// sentence at a window of a two-stage unloader's stage 2 pulled directly by the
// process. Its finished cart is not sent anywhere; it waits on the window for
// the line, and the card says so, so a cart standing there reads as done.
//
// Runs under plain Node (no npm): the shipping modules in a vm.
// Exit 0 = pass, 1 = any failure. Run via operator_pulled_directly_test.go.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0, failed = 0;
function eq(got, want, label) {
    if (got === want) { passed++; return; }
    failed++;
    console.error('FAIL: ' + label + '\n   got:  ' + JSON.stringify(got) + '\n   want: ' + JSON.stringify(want));
}

// The sentence is the window card's note on the board (operator-board-model.js
// boardModel): both modules are pure and run here as they ship.
const ctx = vm.createContext({});
['operator-window-state.js', 'operator-board-model.js'].forEach(function (f) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, f), 'utf8')
        .replace(/^import\s[^;]*;/gm, '').replace(/^export\s+/gm, ''), ctx);
});

// note is the note on the card the board draws for a window, or '' with no card.
function note(tile) {
    const t = Object.assign({ node: { id: 1, name: 'ST2_A1', core_node_name: 'ST2_A1' }, orders: [] }, tile);
    t.loader = Object.assign({ role: 'consume', layout: 'shared_window', parts: [{ code: 'PART-X' }] }, tile.loader);
    const m = ctx.boardModel({ station: {}, nodes: [t], serves: [] });
    const cards = m.zones.find(z => z.key === 'windows').cards;
    return cards.length ? cards[0].reason : '';
}

const cart = { occupied: true, payload_code: '' };
eq(note({ loader: { pulled_directly: true }, bin_state: cart }), 'The cart waits here for the line',
    'a finished cart on a pulled-directly window waits for the line');
eq(note({ loader: { pulled_directly: false }, bin_state: cart }), '',
    'the option off: no sentence, the window sends its cart on as today');
eq(note({ loader: { pulled_directly: true }, bin_state: { occupied: false } }), '',
    'an empty window says nothing');
eq(note({ loader: { pulled_directly: true }, bin_state: { occupied: true, payload_code: 'PART-X' } }), '',
    'a cart still carrying parts is not finished');
eq(note({ loader: { pulled_directly: true } }), '', 'no bin state: nothing to say');

console.log('operator pulled directly: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
