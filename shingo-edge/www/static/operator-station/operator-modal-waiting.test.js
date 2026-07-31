// Unit tests for waitingLabel (operator-modal.js) — the label on the disabled
// two-robot "waiting" button.
//
// WHY THIS IS PINNED. The two-robot waiting arm sits ABOVE the inFlight arm in
// renderModal's if/else chain, and inFlight is the only other place that renders
// queue_reason / ACKNOWLEDGED / SOURCING / ROBOT IN TRANSIT. Before this label
// existed the chain terminated on a bare "WAITING FOR OTHER ROBOT" for exactly
// the case where the operator cannot act and most needs to know why. Springfield
// ALN_003 2026-07-31: 32 minutes of that bare label while the supply leg fought
// three navigation faults the Edge had already recorded, then the abandon sweep
// cancelled both legs. Same day, next pair: 38 minutes reading "acknowledged"
// while Core held it queued on waiting_for_material.
//
// Runs under plain Node (no npm): loads the real formatETA out of
// operator-util.js and the real waitingLabel out of operator-modal.js, both in a
// vm. Exit 0 = pass, 1 = any failure. Run via the Go wrapper
// operator_modal_waiting_test.go.

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

// Extract a top-level `function NAME(...) {...}` block by brace matching, so the
// test exercises the SHIPPING source rather than a copy that can drift.
// operator-modal.js can't be loaded wholesale — it imports and touches the DOM
// at module scope — but waitingLabel is pure apart from formatETA.
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

const here = __dirname;
// operator-util.js is import-free but reads document.body at module scope.
// Strip the `export` keyword in ALL its forms — `export function`, `export
// async function`, `export const`. A narrower pattern misses `export async`
// and dies on fetchWithTimeout.
const utilSrc = fs.readFileSync(path.join(here, 'operator-util.js'), 'utf8')
    .replace(/^export\s+/gm, '');
const modalSrc = fs.readFileSync(path.join(here, 'operator-modal.js'), 'utf8');

const ctx = vm.createContext({
    document: { body: { dataset: { stationId: '1' } }, getElementById: () => null },
    console: console,
});
vm.runInContext(utilSrc, ctx);
vm.runInContext(extractFn(modalSrc, 'waitingLabel') + '\nthis.waitingLabel = waitingLabel;', ctx);
const waitingLabel = ctx.waitingLabel;

const BASE = 'WAITING FOR OTHER ROBOT';

// No blocker resolvable — never invent a reason.
eq(waitingLabel(null), BASE, 'null blocker → bare base');
eq(waitingLabel(undefined), BASE, 'undefined blocker → bare base');

// queue_reason is Core's whole sentence and always wins over the status word.
// These two are the literal strings observed on SPR ALN_003 2026-07-31.
eq(waitingLabel({ status: 'acknowledged', queue_reason: 'Waiting for material: 74577-6SA0A.06' }),
    BASE + ' — Waiting for material: 74577-6SA0A.06',
    'queue_reason wins over status (SPR waiting_for_material)');
eq(waitingLabel({ status: 'acknowledged', queue_reason: 'Holding this leg until partner order 4023cd47 secures a bin' }),
    BASE + ' — Holding this leg until partner order 4023cd47 secures a bin',
    'queue_reason wins over status (SPR waiting_for_partner)');

// The incident case: the supply leg faulted three times and the operator was
// shown nothing at all.
eq(waitingLabel({ status: 'faulted' }), BASE + ' — faulted, recovering',
    'faulted is surfaced, not hidden');

// Pre-fleet family — "acknowledged" must not read as a robot on the move.
eq(waitingLabel({ status: 'acknowledged' }), BASE + ' — acknowledged, not yet dispatched',
    'acknowledged is distinguished from in transit');
eq(waitingLabel({ status: 'queued' }), BASE + ' — queued', 'queued without a reason');
eq(waitingLabel({ status: 'sourcing' }), BASE + ' — sourcing', 'sourcing without a reason');

// in_transit: ETA when Core stamped one, plain wording when it did not. Absent
// and unparseable ETAs must both degrade to the plain phrase, never "undefined".
eq(waitingLabel({ status: 'in_transit' }), BASE + ' — in transit', 'in_transit, no eta stamped');
eq(waitingLabel({ status: 'in_transit', eta: '' }), BASE + ' — in transit', 'in_transit, empty eta');
eq(waitingLabel({ status: 'in_transit', eta: 'not-a-date' }), BASE + ' — in transit', 'in_transit, unparseable eta');
eq(waitingLabel({ status: 'in_transit', eta: new Date(Date.now() + 5 * 60 * 1000).toISOString() }),
    BASE + ' — ETA: ~5 min', 'in_transit with a real eta');

// Unknown//future statuses degrade to the base rather than rendering junk.
eq(waitingLabel({ status: 'reshuffling' }), BASE, 'unmapped status → bare base');
eq(waitingLabel({}), BASE, 'blocker with no status → bare base');

console.log('operator-modal waitingLabel: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed === 0 ? 0 : 1);
