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
const statusSrc = fs.readFileSync(path.join(__dirname, 'order-status.js'), 'utf8');
// TERMINAL_STATUSES from the shipping order-status.js, as the departed-leg test
// takes it, so cellCardAction's `active` filter sees the real terminal set.
const terminal = JSON.parse(
    statusSrc.match(/export const TERMINAL_STATUSES = (\[[^\]]*\])/)[1].replace(/'/g, '"'));
const ctx = vm.createContext({
    console: console,
    JSON: JSON,
    Number: Number,
    Array: Array,
    isActive: (s) => !terminal.includes(s),
    esc: (s) => String(s),
    withQueueCause: (base) => base,
    formatETA: () => ({ empty: true, text: '' }),
});
vm.runInContext(modalSrc.match(/const WAITING_BASE = '[^']*';/)[0].replace('const ', 'var '), ctx);
vm.runInContext(modalSrc.match(/const TO_MARKET_LABEL = '[^']*';/)[0].replace('const ', 'var '), ctx);
vm.runInContext(extractFn(utilSrc, 'distinctQueueCauses').replace('export function', 'function'), ctx);
for (const fn of ['blockerPhrase', 'waitingLabel', 'statusWordOf', 'pairWaitingLabel',
    'isStationReleasable', 'swapPair', 'orderStatusChip', 'cellCardAction']) {
    vm.runInContext(extractFn(modalSrc, fn), ctx);
}
vm.runInContext('this.pairWaitingLabel = pairWaitingLabel; this.cellCardAction = cellCardAction;', ctx);
const pairWaitingLabel = ctx.pairWaitingLabel;
const cellCardAction = ctx.cellCardAction;

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

// ── WHICH ARM THE CARD TAKES (census 18 and 19) ─────────────────────────
//
// The label is half of it. cellCardAction decides whether a pair gets this
// label at all, and the pair arm sits ABOVE the per-order RELEASE / CONFIRM /
// in-flight arms, so the arm chosen is what a person can or cannot act on. The
// arm tested swap_mode === 'two_robot'; it now keys on two linked live legs and
// on releases_as_pair, the server's declared answer to "is this pair released
// together". Each case compares the whole button, so it pins the arm, not only
// the words on it.

const CELL = { id: 7, name: 'CELL-7' };
function cardFor(orders, releasesAsPair, claim) {
    return cellCardAction({ node: CELL, orders: orders, swap_ready: false, releases_as_pair: releasesAsPair },
        claim, 40);
}
function pairArm(pair) {
    return JSON.stringify({ label: pairWaitingLabel(pair), cls: 'close', enabled: false, action: '' });
}

// A pair held on two different causes: one leg waiting for its partner, the
// partner digging its own bin out. Neither leg is parked.
const digPair = [
    { id: 601, status: 'sourcing', sibling_order_id: 602,
        queue_reason: 'Waiting for partner order 4023cd47 to dig out its bin — the two legs go together' },
    { id: 602, status: 'reshuffling', sibling_order_id: 601,
        queue_reason: 'Rearranging lane L12 to reach 74577-6SA0A.06' },
];

// 18. two_robot: the card takes the PAIR arm, with the whole pair on it. The
// in-flight arm beneath it shows one leg's cause.
const TWO_ROBOT = { swap_mode: 'two_robot', role: 'consume', payload_code: '74577-6SA0A.06' };
eq(JSON.stringify(cardFor(digPair, true, TWO_ROBOT)), pairArm(digPair),
    'census 18: a held two_robot pair takes the pair arm — disabled, both causes on it');

// And the arm's reason to exist: a leg parked while its partner is still on the
// way gets no RELEASE of its own. That is the per-order button, and it would
// send one leg of a pair released as one without the other.
const parkedLeg = [
    { id: 611, status: 'staged', sibling_order_id: 612 },
    { id: 612, status: 'in_transit', sibling_order_id: 611 },
];
eq(JSON.stringify(cardFor(parkedLeg, true, TWO_ROBOT)), pairArm(parkedLeg),
    'census 18: a parked leg of a pair released as one does not get a RELEASE of its own');

// 19. two_robot_press_index: released as one just the same, so the same arm.
// Keyed on the mode name, this pair fell to the in-flight arm and read as a
// single leg's wait.
const PRESS_INDEX = { swap_mode: 'two_robot_press_index', role: 'produce', payload_code: 'WIDGET-A' };
eq(JSON.stringify(cardFor(digPair, true, PRESS_INDEX)), pairArm(digPair),
    'census 19: a held press-index pair renders as one wait');

// Two linked legs is NOT the test on its own. sequential and single_robot link
// their legs and release them one at a time; a staged leg there keeps its own
// RELEASE, and the pair arm must not take it.
const SEQUENTIAL = { swap_mode: 'sequential', role: 'consume', payload_code: 'PANEL-A' };
const seqPair = [
    { id: 621, status: 'staged', sibling_order_id: 622, lane_held: false },
    { id: 622, status: 'queued', sibling_order_id: 621 },
];
const seqBtn = cardFor(seqPair, false, SEQUENTIAL);
eq(seqBtn.label, 'RELEASE', 'sequential: a staged leg of a linked pair keeps its own RELEASE');
eq(seqBtn.action, 'release-prompt:/api/orders/621/release', 'and it releases that leg alone');

const SINGLE = { swap_mode: 'single_robot', role: 'consume', payload_code: 'PANEL-A' };
const relay = [
    { id: 631, status: 'delivered', sibling_order_id: 632, auto_confirm: true },
    { id: 632, status: 'staged', sibling_order_id: 631, lane_held: false },
];
const relayBtn = cardFor(relay, false, SINGLE);
eq(relayBtn.label, 'RELEASE', 'single_robot relay: the swap leg keeps its own RELEASE beside its stage leg');
eq(relayBtn.action, 'release-prompt:/api/orders/632/release', 'and it releases the swap leg');

console.log('operator-modal pairWaitingLabel: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed === 0 ? 0 : 1);
