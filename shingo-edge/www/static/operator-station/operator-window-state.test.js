// Unit tests for operator-window-state.js — the loader/unloader window state model.
//
// Pins cardModel + headerModel for BOTH roles and the encoded incident cases, so the
// state machine can be refactored underneath without silently changing what the board
// shows or which tap fires. Runs under plain Node (no npm): strips `export` and loads
// the module in a vm. Exit 0 = pass, 1 = any failure. Run via the Go wrapper
// operator_window_state_test.go.

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

// ── Load the import-free module by stripping `export` and running it in a vm ──
const src = fs.readFileSync(path.join(__dirname, 'operator-window-state.js'), 'utf8')
    .replace(/export\s+function\s+/g, 'function ')
    .replace(/export\s+const\s+/g, 'const ');
const ctx = vm.createContext({});
vm.runInContext(src + '\nthis.cardModel = cardModel; this.headerModel = headerModel; this.nodeFacts = nodeFacts;', ctx);
const cardModel = ctx.cardModel, headerModel = ctx.headerModel, nodeFacts = ctx.nodeFacts;

// entry builder: role + bin (none | empty | {payload}) + orders [{status, payload_code}]
function entry(role, bin, orders) {
    let bin_state = null;
    if (bin === 'empty') bin_state = { occupied: true, payload_code: '' };
    else if (bin && bin.payload) bin_state = { occupied: true, payload_code: bin.payload };
    return { active_claim: { role: role }, bin_state: bin_state, orders: orders || [] };
}
function card(e, code) { return cardModel(e, code); }

// ─────────────────────────── LOADER (produce) ───────────────────────────

// 1. Delivered empty, no bin reflected yet — the fix: clickable "tap to load",
//    NOT a dead card. Header agrees ("BIN ARRIVED").
(function () {
    const e = entry('produce', null, [{ status: 'delivered', payload_code: 'BRKT' }]);
    const c = card(e, 'BRKT');
    eq(c.statusText, 'DELIVERED', 'produce delivered: status');
    eq(c.detail, 'Tap to load', 'produce delivered: detail');
    eq(c.action, 'load', 'produce delivered: action is load (clickable)');
    eq(c.queueCount, 1, 'produce delivered: badge counts the real order');
    eq(c.delivered, true, 'produce delivered: badge delivered flag');
    eq(headerModel(e).text, 'BIN ARRIVED', 'produce delivered: header');
})();

// 2. Empty bin present + demand → LOAD now, clickable.
(function () {
    const e = entry('produce', 'empty', [{ status: 'queued', payload_code: 'BRKT' }]);
    const c = card(e, 'BRKT');
    eq(c.statusText, 'LOAD', 'produce loadNow: status');
    eq(c.detail, 'Empty bin at node — tap to load', 'produce loadNow: detail');
    eq(c.action, 'load', 'produce loadNow: action');
})();

// 3. Empty en route, no bin → IN TRANSIT, not clickable. Header "BIN ARRIVING".
(function () {
    const e = entry('produce', null, [{ status: 'in_transit', payload_code: 'BRKT' }]);
    const c = card(e, 'BRKT');
    eq(c.statusText, 'IN TRANSIT', 'produce in transit: status');
    eq(c.action, '', 'produce in transit: not clickable');
    eq(headerModel(e).text, 'BIN ARRIVING', 'produce in transit: header');
})();

// 4. Demand queued, no bin → QUEUED, not clickable.
(function () {
    const c = card(entry('produce', null, [{ status: 'queued', payload_code: 'BRKT' }]), 'BRKT');
    eq(c.statusText, 'QUEUED', 'produce queued: status');
    eq(c.action, '', 'produce queued: not clickable');
})();

// 5. Empty bin parked, no demand → still loadable.
(function () {
    const c = card(entry('produce', 'empty', []), 'BRKT');
    eq(c.statusText, 'LOAD', 'produce empty-no-demand: status');
    eq(c.detail, 'Empty bin parked — tap to load', 'produce empty-no-demand: detail');
    eq(c.action, 'load', 'produce empty-no-demand: action');
})();

// 6/7. Header: loaded bin → LOADED; idle → AWAITING BIN.
eq(headerModel(entry('produce', { payload: 'BRKT' }, [])).text, 'LOADED', 'produce loaded: header');
eq(headerModel(entry('produce', null, [])).text, 'AWAITING BIN', 'produce idle: header');

// 8. Agnostic blank-payload empty: an empty + untagged demand lights every payload
//    (plant 2026-06-01). Card for an arbitrary code reads LOAD-now, not QUEUED.
(function () {
    const e = entry('produce', 'empty', [{ status: 'in_transit', payload_code: '' }]);
    const c = card(e, 'BRKT');
    eq(c.statusText, 'LOAD', 'produce agnostic empty: status is LOAD (not QUEUED)');
    eq(c.action, 'load', 'produce agnostic empty: action');
    eq(c.queueCount, 0, 'produce agnostic empty: no queue badge (untagged order)');
})();

// ─────────────────────────── UNLOADER (consume) ──────────────────────────

// 9. Delivered full, no bin reflected — clickable "tap to unload". Header "FULL ARRIVED".
(function () {
    const e = entry('consume', null, [{ status: 'delivered', payload_code: 'ASSY' }]);
    const c = card(e, 'ASSY');
    eq(c.statusText, 'DELIVERED', 'consume delivered: status');
    eq(c.detail, 'Tap to unload', 'consume delivered: detail');
    eq(c.action, 'unload', 'consume delivered: action is unload (clickable)');
    eq(headerModel(e).text, 'FULL ARRIVED', 'consume delivered: header');
})();

// 10. Loaded full parked (matching code) → SWAP, clickable.
(function () {
    const c = card(entry('consume', { payload: 'ASSY' }, []), 'ASSY');
    eq(c.statusText, 'SWAP', 'consume loaded: status');
    eq(c.detail, 'Loaded bin parked — tap to swap', 'consume loaded: detail');
    eq(c.action, 'unload', 'consume loaded: action');
})();

// 11. Empty bin parked at a consumer → SWAP (NOT a produce-style LOAD) — plant 2026-06-02.
(function () {
    const c = card(entry('consume', 'empty', []), 'ASSY');
    eq(c.statusText, 'SWAP', 'consume empty: status is SWAP not LOAD');
    eq(c.detail, 'Empty bin parked — tap to swap', 'consume empty: detail');
    eq(c.action, 'unload', 'consume empty: action');
})();

// 12/13/14. Header role wording: arriving / present full / idle.
eq(headerModel(entry('consume', null, [{ status: 'in_transit', payload_code: 'ASSY' }])).text, 'FULL ARRIVING', 'consume in transit: header');
eq(headerModel(entry('consume', { payload: 'ASSY' }, [])).text, 'FULL', 'consume full present: header');
eq(headerModel(entry('consume', null, [])).text, 'AWAITING FULL', 'consume idle: header');

// ─────────────────────────── acknowledged de-aliased from in_transit ───────────────────────────
// acknowledged used to be bucketed with in_transit ("IN TRANSIT") while Core
// ACKs at intake, pre-sourcing — the operator saw a moving robot for an order
// still hunting bins. acknowledged must render as its OWN step, never as IN
// TRANSIT, in both the card and the header.

// 15. An acknowledged order is NOT in transit: card must not say IN TRANSIT.
(function () {
    const e = entry('produce', null, [{ status: 'acknowledged', payload_code: 'BRKT' }]);
    const c = card(e, 'BRKT');
    if (c.statusText === 'IN TRANSIT') { failed++; console.error('FAIL: acknowledged must not render IN TRANSIT (statusText=' + c.statusText + ')'); }
    else { passed++; }
    if (c.inTransit === true) { failed++; console.error('FAIL: acknowledged must not set inTransit flag'); }
    else { passed++; }
    // Header must not say ARRIVING for an acknowledged-only order.
    const h = headerModel(e);
    if (/ARRIVING/.test(h.text)) { failed++; console.error('FAIL: acknowledged header must not say ARRIVING (got ' + h.text + ')'); }
    else { passed++; }
})();

// 16. in_transit still renders IN TRANSIT (the de-alias must not swallow the
//     real transit state).
(function () {
    const c = card(entry('produce', null, [{ status: 'in_transit', payload_code: 'BRKT' }]), 'BRKT');
    eq(c.statusText, 'IN TRANSIT', 'in_transit still renders IN TRANSIT');
    eq(c.inTransit, true, 'in_transit sets inTransit flag');
})();

// 17. A sourcing order is ACTIVE: window-state's inlined active list used to
//     exclude sourcing, so it vanished to NO DEMAND. After mirroring !terminal
//     it must count as demand.
(function () {
    const e = entry('produce', null, [{ status: 'sourcing', payload_code: 'BRKT' }]);
    const c = card(e, 'BRKT');
    if (c.statusText === 'NO DEMAND') { failed++; console.error('FAIL: sourcing must not vanish to NO DEMAND'); }
    else { passed++; }
    const f = nodeFacts(e);
    eq(f.activeOrders.length, 1, 'sourcing counts as an active order');
})();

// 18. sourceNode carries the supermarket/buffer slot the carrier is coming FROM.
//     The home-location board renders it in place of the queue ordinal, which on a
//     board of independent one-bin homes says nothing an operator can act on.
(function () {
    const c = card(entry('produce', null, [
        { status: 'in_transit', payload_code: 'BRKT', source_node: 'SMN_005' },
    ]), 'BRKT');
    eq(c.sourceNode, 'SMN_005', 'sourceNode comes from the per-payload order');
})();

// 19. sourceNode is EMPTY when no real per-payload order backs the card — the
//     agnostic blank-payload empty lights every allowed payload (case 3), and must
//     not stamp a source pill on all of them any more than it stamps a number.
(function () {
    const c = card(entry('produce', 'empty', [{ status: 'queued', payload_code: '' }]), 'BRKT');
    eq(c.queueCount, 0, 'agnostic demand contributes no per-payload order');
    eq(c.sourceNode, '', 'agnostic demand stamps no source pill');
})();

// 20. With several orders in flight for one payload, sourceNode must describe the
//     SAME order the badge colour does (delivered → in transit → first queued),
//     or the pill and the colour tell the operator about different robots.
(function () {
    const c = card(entry('produce', null, [
        { status: 'queued', payload_code: 'BRKT', source_node: 'SMN_009' },
        { status: 'delivered', payload_code: 'BRKT', source_node: 'SMN_006' },
    ]), 'BRKT');
    eq(c.delivered, true, 'delivered wins the badge colour');
    eq(c.sourceNode, 'SMN_006', 'sourceNode follows the delivered order, not the first');
})();

// ── result ──
if (failed > 0) { console.error('\n' + failed + ' failure(s), ' + passed + ' passed'); process.exit(1); }
console.log('operator-window-state: ' + passed + ' assertions passed');
