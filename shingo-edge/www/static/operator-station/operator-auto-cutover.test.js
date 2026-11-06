// operator-auto-cutover.test.js <event.json> — the automatic cutover notice
// reads every field of CATIDAutoCutoverEvent. The JSON is the Go event,
// marshalled by www/operator_auto_cutover_test.go, so a renamed field on
// either side fails here. Extracts the shipping function out of operator.js
// (it touches the DOM at module scope), as operator-modal-departed.test.js does.
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');

function extractFn(src, name) {
    const start = src.indexOf('function ' + name + '(');
    if (start < 0) throw new Error('function ' + name + ' not found');
    const open = src.indexOf('{', start);
    let depth = 0;
    for (let j = open; j < src.length; j++) {
        if (src[j] === '{') depth++;
        else if (src[j] === '}') { depth--; if (depth === 0) return src.slice(start, j + 1); }
    }
    throw new Error('unbalanced braces in ' + name);
}

const src = fs.readFileSync(path.join(__dirname, 'operator.js'), 'utf8');
const ctx = {};
vm.createContext(ctx);
vm.runInContext(extractFn(src, 'autoCutoverWords'), ctx);

const events = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
let failed = 0;
function check(name, fn) {
    try { fn(); console.log('ok   ' + name); } catch (e) { failed++; console.log('FAIL ' + name + '\n     ' + e.message); }
}

check('the gated notice names the changeover, the process and the part', () => {
    const s = ctx.autoCutoverWords(events.gated);
    assert.ok(s.indexOf('#41') >= 0, s);
    assert.ok(s.indexOf('CELL-A') >= 0, s);
    assert.ok(s.indexOf('1002') >= 0, s);
    assert.ok(s.indexOf('past its gate') < 0, 'a gated cutover says nothing about a gate: ' + s);
});

check('the forced notice says it completed past its gate and what it left running', () => {
    const s = ctx.autoCutoverWords(events.forced);
    assert.ok(s.indexOf('past its gate') >= 0, s);
    assert.ok(s.indexOf('task at node N1 in staging_requested') >= 0, s);
    assert.ok(s.indexOf('order 7 in staged') >= 0, s);
});

if (failed) { console.log('FAILED: ' + failed); process.exit(1); }
console.log('auto cutover notice: all passed');
