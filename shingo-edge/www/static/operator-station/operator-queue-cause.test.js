// Unit tests for withQueueCause / distinctQueueCauses (operator-util.js) — the
// shared shape behind "a parked order says why".
//
// WHAT THESE PIN. Core generates a typed cause sentence at set-time and pushes
// it onto the Edge order row; several operator surfaces showed only the status
// word beside it. "retrieve: queued" cannot tell a capacity gate from a missing
// bin. The sentence can, and it was already on the row.
//
// THE STATUS WORD STAYS. `queued` and `sourcing` are distinct lifecycles and
// are not merged, renamed or collapsed. Every assertion below keeps the label
// it was handed and only appends — a helper that ever REPLACED the status would
// be the collapse this work is explicitly not doing, so it is pinned here.
//
// Runs under plain Node (no npm). Exit 0 = pass, 1 = any failure. Run via the
// Go wrapper operator_queue_cause_test.go.

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
function deepEq(got, want, label) {
    eq(JSON.stringify(got), JSON.stringify(want), label);
}

// operator-util.js is import-free but reads document.body at module scope.
const utilSrc = fs.readFileSync(path.join(__dirname, 'operator-util.js'), 'utf8')
    .replace(/^export\s+/gm, '');
const ctx = vm.createContext({
    document: { body: { dataset: { stationId: '1' } }, getElementById: () => null },
    console: console,
});
vm.runInContext(utilSrc, ctx);
const { withQueueCause, distinctQueueCauses } = ctx;

const DASH = ' — ';
const REASON = 'Waiting for material: 74577-6SA0A.06';

// ── withQueueCause ────────────────────────────────────────────────────────

// The cause is APPENDED. The label the surface built is still there in full.
eq(withQueueCause('retrieve: queued', { queue_reason: REASON }),
    'retrieve: queued' + DASH + REASON,
    'cause is appended to the label, not substituted for it');

// The status word survives for BOTH parked lifecycles, separately. These two
// are not merged, and a helper that normalised them would show up here.
eq(withQueueCause('retrieve: queued', { queue_reason: REASON }),
    'retrieve: queued' + DASH + REASON, 'queued keeps its own word');
eq(withQueueCause('retrieve: sourcing', { queue_reason: REASON }),
    'retrieve: sourcing' + DASH + REASON, 'sourcing keeps its own word');
eq(withQueueCause('retrieve: queued', { queue_reason: REASON })
    === withQueueCause('retrieve: sourcing', { queue_reason: REASON }),
    false, 'queued and sourcing do not render identically');

// No cause: the label comes back untouched. No dangling dash on an order Core
// said nothing about.
eq(withQueueCause('retrieve: queued', { queue_reason: '' }), 'retrieve: queued', 'empty reason → bare label');
eq(withQueueCause('retrieve: queued', {}), 'retrieve: queued', 'absent reason → bare label');
eq(withQueueCause('retrieve: queued', null), 'retrieve: queued', 'null order → bare label');
eq(withQueueCause('retrieve: queued', undefined), 'retrieve: queued', 'undefined order → bare label');

// The separator is the em-dash the other surfaces already use, so the two new
// sites read the same as waitingLabel.
eq(withQueueCause('X', { queue_reason: 'Y' }), 'X — Y', 'separator is a spaced em-dash');

// ── distinctQueueCauses ───────────────────────────────────────────────────

deepEq(distinctQueueCauses([
    { queue_reason: 'A' },
    { queue_reason: 'B' },
]), ['A', 'B'], 'two distinct causes come back in order');

// A swap pair parked for one reason is ONE line, not two identical ones.
deepEq(distinctQueueCauses([
    { queue_reason: 'A' },
    { queue_reason: 'A' },
]), ['A'], 'duplicate causes collapse to one line');

deepEq(distinctQueueCauses([
    { queue_reason: 'A' },
    { queue_reason: 'B' },
    { queue_reason: 'A' },
]), ['A', 'B'], 'first-seen order is preserved across a repeat');

// An order Core said nothing about contributes nothing — not a blank line that
// reads as a cause nobody can name. The count line already said it is parked.
deepEq(distinctQueueCauses([
    { queue_reason: '' },
    { status: 'queued' },
    null,
    { queue_reason: 'A' },
]), ['A'], 'causeless orders contribute nothing');

deepEq(distinctQueueCauses([]), [], 'empty set → no causes');
deepEq(distinctQueueCauses(null), [], 'null set → no causes');
deepEq(distinctQueueCauses(undefined), [], 'undefined set → no causes');

if (failed > 0) {
    console.error('\n' + failed + ' failure(s), ' + passed + ' passed');
    process.exit(1);
}
console.log('OK: ' + passed + ' assertions passed');
