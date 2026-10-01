// Unit tests for the board's per-purpose RELEASE (SHAPE 3.8): the button a
// changeover tile shows (cellCardAction via changeoverReleasePurpose) and the
// blue glow (isReleaseReady, operator-render.js).
//
// WHY THIS IS PINNED. A changeover tile used to show one RELEASE whatever the
// leg was parked at, so a press meant "ready" could carry an evac through
// "tooling done". The button now names the station wait's purpose and posts
// it. And a node the changeover leaves unchanged used to lose its glow: the
// glow took the changeover branch whenever the tile had a task, found no evac
// on an unchanged task, and went dark mid-shift (L10).
//
// Runs under plain Node (no npm): extracts the real functions out of the
// shipping sources in a vm. Exit 0 = pass, 1 = any failure. Run via the Go
// wrapper operator_release_purpose_test.go.

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

const modalSrc = fs.readFileSync(path.join(__dirname, 'operator-modal.js'), 'utf8');
const renderSrc = fs.readFileSync(path.join(__dirname, 'operator-render.js'), 'utf8');
const ctx = { console };
vm.createContext(ctx);
for (const fn of ['changeoverReleasePurpose', 'changeoverReleaseAction']) {
    vm.runInContext(extractFn(modalSrc, fn), ctx);
}
vm.runInContext(extractFn(renderSrc, 'isReleaseReady'), ctx);
const { changeoverReleasePurpose, changeoverReleaseAction, isReleaseReady } = ctx;

const NODE = { id: 12, process_id: 3 };
const tile = (situation, purposes, extra) => Object.assign({
    node: NODE,
    changeover_task: situation ? { situation: situation } : null,
    release_purposes: purposes,
}, extra || {});

// The button names the wait the leg is parked at.
let t = tile('evacuate', [{ purpose: 'tooling_done', ready: true }]);
eq(changeoverReleasePurpose(t), 'tooling_done', 'an evac parked at its tooling wait owes tooling done');
let a = changeoverReleaseAction(t, 'tooling_done');
eq(a.label, 'RELEASE: TOOLING DONE', 'the button says which decision it makes');
eq(a.action, 'release-prompt:/api/processes/3/changeover/release?node_id=12&purpose=tooling_done',
    'and posts that purpose for that node');
eq(changeoverReleaseAction(tile('swap', []), 'ready').label, 'RELEASE: READY', 'a ready wait reads READY');

// The earliest owed decision first; a purpose present but not parked offers none.
t = tile('swap', [{ purpose: 'ready', ready: false }, { purpose: 'tooling_done', ready: true }]);
eq(changeoverReleasePurpose(t), 'tooling_done', 'only a purpose a leg is parked at is offered');
eq(changeoverReleasePurpose(tile('swap', [{ purpose: 'ready', ready: false }])), null,
    'a leg still driving to its ready wait offers no RELEASE yet');

// The production arms keep what is theirs.
eq(changeoverReleasePurpose(tile('unchanged', [{ purpose: 'swap', ready: true }])), null,
    'an unchanged node keeps its production button (L10)');
eq(changeoverReleasePurpose(tile(null, [{ purpose: 'swap', ready: true }])), null,
    'no changeover: no changeover button');
eq(changeoverReleasePurpose(tile('swap', undefined)), null,
    'a leg with no stored purposes (created before Edge v8) falls through to the old arms');

// The glow.
eq(isReleaseReady(tile('unchanged', [], { swap_ready: true })), true,
    'L10: an unchanged pair keeps its glow during another node\'s changeover');
eq(isReleaseReady(tile('evacuate', [{ purpose: 'tooling_done', ready: true }])), true,
    'a changeover leg parked at its decision glows');
eq(isReleaseReady(tile('evacuate', [{ purpose: 'tooling_done', ready: false }])), false,
    'a changeover leg not yet parked does not');
eq(isReleaseReady(tile(null, [{ purpose: 'swap', ready: true }])), false,
    'production without swap_ready does not glow: a single-robot release has no ready moment');

console.log('operator release purpose: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
