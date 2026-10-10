// Unit tests for loaderOffersClear (operator-modal.js): when a loader window's
// modal offers CLEAR BIN.
//
// WHY THIS IS PINNED. A carrier can come back to its loader still labelled but
// reading 0 (a final release at 1, one tick before the pickup). Core will not
// send it anywhere and LOAD refuses it, telling the operator to clear it first
// (row L item 5). The modal used to offer CLEAR BIN only when the window's count
// was above 0, so the one way out was hidden on exactly that carrier.
//
// Runs under plain Node (no npm), extracting the shipping function in a vm.
// Exit 0 = pass, 1 = any failure. Run via operator_modal_loader_clear_test.go.

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

const src = fs.readFileSync(path.join(__dirname, 'operator-modal.js'), 'utf8');
const ctx = {};
vm.createContext(ctx);
vm.runInContext(extractFn(src, 'loaderOffersClear'), ctx);
const offers = (bin, remaining) => vm.runInContext('loaderOffersClear', ctx)(bin, remaining);

eq(offers({ occupied: true, payload_code: 'SHIM', uop_remaining: 0 }, 0), true,
    'a labelled carrier at 0: CLEAR is offered (the only way out)');
eq(offers({ occupied: true, payload_code: 'SHIM', uop_remaining: -2 }, 0), true,
    'a labelled carrier below 0: CLEAR is offered');
eq(offers({ occupied: true, payload_code: 'SHIM', uop_remaining: 12 }, 12), true,
    'a carrier with units: CLEAR is offered, as before');
eq(offers({ occupied: true, payload_code: '', uop_remaining: 0 }, 0), false,
    'an empty unlabelled carrier: nothing to clear, LOAD is the action');
eq(offers({ occupied: false }, 0), false, 'no carrier: no CLEAR');
eq(offers(null, 5), false, 'no bin state: no CLEAR');

console.log(passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
