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
const cardModel = ctx.cardModel, headerModel = ctx.headerModel;

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

// ── result ──
if (failed > 0) { console.error('\n' + failed + ' failure(s), ' + passed + ' passed'); process.exit(1); }
console.log('operator-window-state: ' + passed + ' assertions passed');
