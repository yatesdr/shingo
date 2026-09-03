// Unit tests for cellCardAction (operator-modal.js) — which ONE button a swap
// cell's card offers, and specifically what a DEPARTED leg does to that choice.
//
// WHY THIS IS PINNED. The bug the departed-leg work exists for was a HIDDEN
// BUTTON, not a wrong backend answer. Springfield press trial 2026-09-02: under
// the IndexRobotSupplies flip, R1 clears the press and drives the full tote to
// the supermarket while R2 puts the fresh carrier on. Every position the cell
// needs was filled, the backend would have accepted the next swap — and the
// card computed `inFlight` over every non-terminal order at the node, found the
// departed R1, and rendered a disabled ROBOT IN TRANSIT for the length of a
// supermarket round trip.
//
// The rule this pins: a departed leg is still LISTED (the operator can see the
// tote leaving, labelled TO MARKET) and drives NOTHING. Control goes,
// information stays — the same split the lane-held test pins for RELEASE.
//
// Runs under plain Node (no npm): extracts the real functions out of
// operator-modal.js in a vm. Exit 0 = pass, 1 = any failure. Run via the Go
// wrapper operator_modal_departed_test.go.

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
function ok(cond, label) { eq(!!cond, true, label); }

// Same brace-matching extraction the lane-held and waitingLabel tests use, and
// for the same reason: exercise the SHIPPING source rather than a copy that can
// drift. operator-modal.js cannot be loaded wholesale — it touches the DOM at
// module scope — but these functions are pure.
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
const statusSrc = fs.readFileSync(path.join(__dirname, 'order-status.js'), 'utf8');

// TERMINAL_STATUSES / isActive come from the shipping order-status.js, so a
// status added there is a status this test sees.
const terminal = JSON.parse(
    statusSrc.match(/export const TERMINAL_STATUSES = (\[[^\]]*\])/)[1].replace(/'/g, '"'));

const ctx = {
    console,
    JSON,
    Number,
    Array,
    isActive: (s) => !terminal.includes(s),
    // Stubs for the collaborators cellCardAction reaches for. esc and
    // withQueueCause are the real shapes, minimal.
    esc: (s) => String(s),
    withQueueCause: (base) => base,
    formatETA: () => ({ empty: true, text: '' }),
};
vm.createContext(ctx);
// TO_MARKET_LABEL first, and as `var`: each runInContext call gets its own
// lexical scope, so a `const` would not survive to the next one or be visible
// to the extracted functions. `var` lands on the vm global, which is what they
// resolve against.
vm.runInContext(
    modalSrc.match(/const TO_MARKET_LABEL = '[^']*';/)[0].replace('const ', 'var '), ctx);
for (const fn of ['isStationReleasable', 'swapPair', 'waitingLabel', 'orderStatusChip', 'cellCardAction']) {
    vm.runInContext(extractFn(modalSrc, fn), ctx);
}

const { cellCardAction, orderStatusChip, TO_MARKET_LABEL } = ctx;

const NODE = { id: 42, name: 'PRESS' };
const PRODUCE = { swap_mode: 'two_robot_press_index', role: 'produce', payload_code: 'WIDGET-A' };

function card(orders, opts) {
    opts = opts || {};
    return cellCardAction(
        { node: NODE, orders, swap_ready: !!opts.swapReady, bin_state: opts.binState },
        opts.claim || PRODUCE,
        opts.remaining != null ? opts.remaining : 40);
}

// ── THE SEQUENCE THE BRIEF ASKS FOR ──────────────────────────────────────
//
// A departed in_transit leg (R1, driving the full tote to the market) beside a
// delivered placing leg (R2, which put the fresh carrier on the press).

const departedR1 = { id: 101, status: 'in_transit', departed: true, sibling_order_id: 102 };
const deliveredR2 = { id: 102, status: 'delivered', departed: false, sibling_order_id: 101 };

let btn = card([departedR1, deliveredR2]);
eq(btn.label, 'CONFIRM',
    'a departed leg beside a delivered placing leg offers CONFIRM — the departed one is not the cell\'s');
eq(btn.enabled, true, 'and it is tappable');
eq(btn.action, '/api/confirm-delivery/102',
    'CONFIRM targets the leg that PLACED the bin, not the one carrying one away');

// The operator taps it. R2 goes terminal (confirmed) and drops out of every
// list; R1 is still driving to the supermarket.
const confirmedR2 = Object.assign({}, deliveredR2, { status: 'confirmed' });
btn = card([departedR1, confirmedR2]);
eq(btn.label, 'REQUEST SWAP',
    'after CONFIRM the cell may order the next swap WHILE the departed leg is still driving — ' +
    'this is the whole feature');
eq(btn.enabled, true, 'and REQUEST SWAP is tappable');
eq(btn.action, '/api/process-nodes/42/finalize', 'and it fires the finalize route');

// ...and the departed leg is still LISTED, as TO MARKET.
eq(orderStatusChip(departedR1).indexOf(TO_MARKET_LABEL) >= 0, true,
    'the departed leg still renders on the card, labelled TO MARKET');
eq(TO_MARKET_LABEL, 'TO MARKET', 'the label is the one the floor was told to expect');
ok(orderStatusChip(departedR1).indexOf('in_transit') < 0,
    'a departed leg must NOT render its raw status — it is leaving, not arriving');
ok(orderStatusChip(deliveredR2).indexOf('delivered') >= 0,
    'a live leg keeps its status word and its cause');

// ── THE REGRESSION THIS REPLACES ─────────────────────────────────────────

btn = card([departedR1]);
eq(btn.label, 'REQUEST SWAP',
    'a departed leg ALONE at the node must not hold the button — this is the exact trial symptom');
ok(btn.label !== 'ROBOT IN TRANSIT',
    'ROBOT IN TRANSIT means a cell waiting on a delivery; a departed leg is the opposite state');

// The dual: an UNDEPARTED in_transit leg still holds the button. The fix must
// not degenerate into "never show ROBOT IN TRANSIT".
const undepartedR1 = Object.assign({}, departedR1, { departed: false });
btn = card([undepartedR1]);
eq(btn.label, 'ROBOT IN TRANSIT',
    'an undeparted in_transit leg still blocks the button — a robot IS working this cell');
eq(btn.enabled, false, 'and it is not tappable');

// An order with no `departed` field at all — an older Edge, or a row written
// before the column existed — must read as NOT departed, i.e. exactly today's
// behaviour. The suppression can never be caused by a MISSING signal.
btn = card([{ id: 101, status: 'in_transit' }]);
eq(btn.label, 'ROBOT IN TRANSIT',
    'absent `departed` degrades to blocking, not to admitting');

// ── DEPARTED IS NOT UNCONFIRMABLE ────────────────────────────────────────
//
// single_robot's one leg places the fresh bin on the press (step 7), departs
// when it lifts the old bin off outbound staging (step 8), and reaches
// `delivered` at the supermarket (step 9). The filter above would take its
// CONFIRM away — and that CONFIRM is the cell's only count receipt for the
// cycle. The `delivered` fallback is the one thing a departed leg may still
// drive, and only when it is not auto-confirming.

const SINGLE = { swap_mode: 'single_robot', role: 'produce', payload_code: 'WIDGET-A' };
const singleRobotLeg = { id: 401, status: 'delivered', departed: true, auto_confirm: false };

btn = card([singleRobotLeg], { claim: SINGLE });
eq(btn.label, 'CONFIRM',
    'single_robot departs at step 8 and is delivered at step 9 — its CONFIRM is the cycle\'s only ' +
    'count receipt and the departed filter must not swallow it');
eq(btn.enabled, true, 'and it is tappable');
eq(btn.action, '/api/confirm-delivery/401', 'and it targets that leg');

// The dual, and the pre-existing hazard it closes: an AUTO-confirm leg passes
// through `delivered` for about a second on its way to confirmed. Nobody signs
// for it — it is going to the supermarket — and a card refreshed inside that
// window must not arm CONFIRM on it.
const autoConfirmDeparted = { id: 402, status: 'delivered', departed: true, auto_confirm: true };
btn = card([autoConfirmDeparted]);
ok(btn.label.indexOf('CONFIRM') < 0,
    'a departed AUTO-confirm leg in its delivered window offers no CONFIRM — there is no receipt ' +
    'to take, and the tap would close an order nobody looked at');
eq(btn.label, 'REQUEST SWAP', 'the cell is balanced, so it offers the next swap instead');

// A live (undeparted) leg still reaches the arm through `active`, unchanged.
btn = card([{ id: 403, status: 'delivered', departed: false, auto_confirm: false }]);
eq(btn.action, '/api/confirm-delivery/403', 'an undeparted delivered leg is unaffected by the fallback');

// ── THE PAIR COUNT ───────────────────────────────────────────────────────
//
// The two_robot waiting arm counts the PAIR. A departed leg leaves `active`
// before the count is taken, so a pair whose evac has left reads as one leg,
// the arm steps aside, and the survivor's own branch offers a button.

const twoRobot = { swap_mode: 'two_robot', role: 'produce', payload_code: 'WIDGET-A' };
const supplyInTransit = { id: 201, status: 'in_transit', departed: false, sibling_order_id: 202 };
const evacUndeparted = { id: 202, status: 'in_transit', departed: false, sibling_order_id: 201 };
btn = card([supplyInTransit, evacUndeparted], { claim: twoRobot });
ok(btn.label.indexOf('WAITING FOR OTHER ROBOT') === 0,
    'two live legs still raise the two-robot waiting label');

const evacDeparted = Object.assign({}, evacUndeparted, { departed: true });
btn = card([supplyInTransit, evacDeparted], { claim: twoRobot });
eq(btn.label, 'ROBOT IN TRANSIT',
    'once the evac has departed the pair reads as one leg, and the survivor drives the card');

// ── THE ARMS ABOVE MUST STILL WIN ────────────────────────────────────────
//
// swap_ready and a station-releasable staged leg both sit above everything the
// departed filter touches, and a departed leg must not disturb either.

btn = card([departedR1, deliveredR2], { swapReady: true });
eq(btn.label, 'RELEASE', 'swap_ready still wins over everything, departed leg present or not');

const stagedLeg = { id: 301, status: 'staged', lane_held: false, departed: false };
btn = card([departedR1, stagedLeg]);
eq(btn.label, 'RELEASE', 'a station-releasable staged leg still offers RELEASE');
eq(btn.action, 'release-prompt:/api/orders/301/release', 'and it targets that leg');

// A DEPARTED staged leg offers nothing — it is not the station's to release.
// (Not a shape that occurs today; the pin is that the filter is applied before
// isStationReleasable, not after.)
const stagedDeparted = Object.assign({}, stagedLeg, { departed: true });
btn = card([stagedDeparted]);
eq(btn.label, 'REQUEST SWAP', 'a departed staged leg does not offer RELEASE');

// ── CONFIRM's manifest label survives the extraction ──────────────────────

btn = card([deliveredR2], { binState: { manifest: '[{"quantity":12},{"quantity":8}]' } });
eq(btn.label, 'CONFIRM: 2 parts, qty 20', 'the manifest-aware CONFIRM label is unchanged');

btn = card([{ id: 0, status: 'delivered', departed: false }]);
eq(btn.label, 'CONFIRM (refresh)', 'a half-built order with no id still renders the refresh guard');
eq(btn.enabled, false, 'and it is not tappable');

// ── AND THE IDLE ARMS ────────────────────────────────────────────────────

btn = card([], { remaining: 0 });
eq(btn.label, 'REQUEST EMPTY', 'an empty produce cell asks for a carrier');
btn = card([], { remaining: 0, claim: { swap_mode: 'two_robot', role: 'produce' } });
eq(btn, null, 'a produce cell with no configured payload has nothing to ask for');
btn = card([], { claim: { swap_mode: 'two_robot', role: 'consume' } });
eq(btn.label, 'REQUEST MATERIAL', 'a consume cell asks for material');

console.log('operator-modal cellCardAction (departed legs): ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed === 0 ? 0 : 1);
