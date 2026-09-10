// Unit tests for pairWaitingLabel (operator-modal.js) — the label a person
// reads while a two-robot pair is stopped.
//
// WHAT THIS IS ABOUT. A held pair is ONE object at the release click and TWO
// objects at rest, and the rest state is what somebody is looking at when a cell
// has stopped. The board took a single leg — `pair.find(o => o.status !==
// 'staged')` — and rendered only that leg's cause. So:
//
//   - a pair parked for TWO DIFFERENT reasons showed one of them, picked by list
//     order, and the operator's actionable half could be the one not shown;
//   - nothing said where the other leg was, so "WAITING FOR OTHER ROBOT" was the
//     whole story for a cell that was stopped;
//   - distinctQueueCauses already existed for exactly this pooling and was not
//     being called at this site.
//
// IT DECIDES NOTHING — this is a label. The >=2 guard, the release affordance
// and every admission gate are untouched.
//
// Runs under plain Node (no npm), extracting the SHIPPING functions out of
// operator-modal.js and operator-util.js in a vm so they cannot drift from a
// copy. Exit 0 = pass. Run via the Go wrapper operator_modal_pair_wait_test.go.

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
    if (open < 0) throw new Error('no body for ' + name);
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
const utilSrc = fs.readFileSync(path.join(__dirname, 'operator-util.js'), 'utf8');
const ctx = vm.createContext({ console: console, formatETA: () => ({ empty: true, text: '' }) });
vm.runInContext(modalSrc.match(/const WAITING_BASE = '[^']*';/)[0].replace('const ', 'var '), ctx);
vm.runInContext(extractFn(utilSrc, 'distinctQueueCauses').replace('export function', 'function'), ctx);
for (const fn of ['blockerPhrase', 'waitingLabel', 'statusWordOf', 'pairWaitingLabel']) {
    vm.runInContext(extractFn(modalSrc, fn), ctx);
}
vm.runInContext('this.pairWaitingLabel = pairWaitingLabel;', ctx);
const pairWaitingLabel = ctx.pairWaitingLabel;

const BASE = 'WAITING FOR OTHER ROBOT';

// ── Not a pair: unchanged, and that matters ────────────────────────────
// Everything operator-modal-waiting.test.js pins about a single blocker must
// keep rendering identically, because this only replaced the call site.
eq(pairWaitingLabel(null), BASE, 'no pair → bare base');
eq(pairWaitingLabel([]), BASE, 'empty pair → bare base');
eq(pairWaitingLabel([{ status: 'faulted' }]), BASE + ' — faulted, recovering',
    'half a pair falls through to the single-blocker wording');

// ── THE INCIDENT SHAPE: two legs, two different causes ─────────────────
// Before this, one of these sentences was shown and the other was not, decided
// by list order. The material wait is the actionable one and it could be the
// one dropped.
const twoCauses = [
    { id: 3994, status: 'staged', queue_reason: 'Holding this leg until partner order 4023cd47 secures a bin' },
    { id: 3993, status: 'sourcing', queue_reason: 'Waiting for material: 74577-6SA0A.06' },
];
eq(pairWaitingLabel(twoCauses),
    BASE + ' — 1 of 2 parked; order 3993 sourcing'
    + ' — Holding this leg until partner order 4023cd47 secures a bin; Waiting for material: 74577-6SA0A.06',
    'both causes reach the operator, with the position and the leg to watch');

// ── One cause, twice: pooled, not doubled ──────────────────────────────
const sameCause = [
    { id: 10, status: 'queued', queue_reason: 'Waiting for material: PANEL-A' },
    { id: 11, status: 'queued', queue_reason: 'Waiting for material: PANEL-A' },
];
eq(pairWaitingLabel(sameCause),
    BASE + ' — 0 of 2 parked — Waiting for material: PANEL-A',
    'a pair parked for the same reason has one reason, not two identical lines');

// Two legs moving → no single releaser to name. Naming two names none.
eq(pairWaitingLabel([
    { id: 10, status: 'queued' },
    { id: 11, status: 'sourcing' },
]), BASE + ' — 0 of 2 parked', 'two movers: position only, no releaser named');

// ── The ordinary live pair: one parked, one on its way ─────────────────
eq(pairWaitingLabel([
    { id: 3994, status: 'staged' },
    { id: 3993, status: 'in_transit' },
]), BASE + ' — 1 of 2 parked; order 3993 in transit',
    'the leg still moving is named, with what it is doing');

// A leg with no id still gets described rather than dropped.
eq(pairWaitingLabel([
    { status: 'staged' },
    { status: 'faulted' },
]), BASE + ' — 1 of 2 parked; the other leg faulted, recovering',
    'an id-less leg is still positioned and explained');

// Both parked and nothing said about why: the position alone is still more than
// the bare base, and no reason is invented.
eq(pairWaitingLabel([
    { id: 1, status: 'staged' },
    { id: 2, status: 'staged' },
]), BASE + ' — 2 of 2 parked', 'both parked, no cause invented');

console.log('operator-modal pairWaitingLabel: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed === 0 ? 0 : 1);
