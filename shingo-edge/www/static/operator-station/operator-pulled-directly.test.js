// Unit tests for pulledDirectlyNote (operator-render.js): the loader card's
// sentence at a window of a two-stage unloader's stage 2 pulled directly by the
// process. Its finished cart is not sent anywhere; it waits on the window for
// the line, and the card says so, so a cart standing there reads as done.
//
// Runs under plain Node (no npm): extracts the shipping function in a vm.
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
vm.runInContext(extractFn(src, 'pulledDirectlyNote'), ctx);
const note = ctx.pulledDirectlyNote;

const cart = { occupied: true, payload_code: '' };
eq(note({ pulled_directly: true, bin_state: cart }), 'The cart waits here for the line',
    'a finished cart on a pulled-directly window waits for the line');
eq(note({ pulled_directly: false, bin_state: cart }), '',
    'the option off: no sentence, the window sends its cart on as today');
eq(note({ pulled_directly: true, bin_state: { occupied: false } }), '',
    'an empty window says nothing');
eq(note({ pulled_directly: true, bin_state: { occupied: true, payload_code: 'PART-X' } }), '',
    'a cart still carrying parts is not finished');
eq(note({ pulled_directly: true }), '', 'no bin state: nothing to say');

console.log('operator pulled directly: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
