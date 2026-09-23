// Unit tests for the manual_swap demand queue in renderModal (operator-modal.js) —
// the per-payload label the modal prints while a consume node's own empty-out
// (U2) is leaving.
//
// WHY THIS IS PINNED. The modal filters a node's active orders per payload in
// its own loop, separate from cardModel. The U2 names no part
// (createUnloaderEmptyOut), so matched by payload_code it would drop from every
// row while the carrier leaves. The modal counts it by source_node
// (isOwnEmptyOut, shared with cardModel) — but only when the node has exactly
// one row: at a node with several, lighting every row would be a false IN
// TRANSIT. Beside those, the cases that must not move: a produce home receiving
// a blank L1, and a blank move arriving from elsewhere.
//
// Runs under plain Node (no npm): extracts the real renderModal out of
// operator-modal.js in a vm, with the collaborators it reaches for on the
// manual_swap path stubbed. Exit 0 = pass, 1 = any failure. Run via the Go
// wrapper operator_modal_home_transit_test.go.

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

// Same brace-matching extraction the other operator-modal tests use: exercise the
// SHIPPING source rather than a copy that can drift.
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
const windowSrc = fs.readFileSync(path.join(__dirname, 'operator-window-state.js'), 'utf8');
const statusSrc = fs.readFileSync(path.join(__dirname, 'order-status.js'), 'utf8')
    .replace(/^export\s+/gm, '');

const content = { innerHTML: '', querySelectorAll: () => [] };
const ctx = {
    console, JSON, Number, Array, Math, Date,
    nodeModalContent: content,
    esc: (s) => String(s),
    fillColor: () => '#000',
    isReplenishing: () => false,
    distinctQueueCauses: () => [],
    getView: () => ({ process: { id: 1 } }),
    handleModalAction: () => {},
    REFUSE_LABEL: 'NO PARTS AVAILABLE',
    UNDO_LABEL: 'UNDO',
};
vm.createContext(ctx);
vm.runInContext(statusSrc, ctx);
vm.runInContext('var PRESS_MAX_MS = 2000; var pressInFlight = false; var pressStartedAt = 0; var pendingEntry = null;', ctx);
vm.runInContext(extractFn(modalSrc, 'pressIsStale'), ctx);
vm.runInContext(extractFn(modalSrc, 'actionBtn'), ctx);
vm.runInContext(extractFn(windowSrc, 'isOwnEmptyOut'), ctx);
vm.runInContext(extractFn(modalSrc, 'renderModal') + '\nthis.renderModal = renderModal;', ctx);

// labelFor returns the status word the demand queue printed for one payload row.
function labelFor(html, code) {
    const re = new RegExp('>' + code + '</span></div><div style="font-size:12px;color:[^"]*">([^<]*)</div>');
    const m = html.match(re);
    return m ? m[1] : '(no row for ' + code + ')';
}

function render(role, coreNode, allowed, orders) {
    ctx.renderModal({
        node: { id: 7, name: coreNode, core_node_name: coreNode },
        active_claim: { swap_mode: 'manual_swap', role: role, allowed_payload_codes: allowed, uop_capacity: 0 },
        bin_state: null,
        orders: orders,
    });
    return content.innerHTML;
}

// A U2 still tagged with the part (filed before the Edge stopped naming it).
(function () {
    const html = render('consume', 'UNL-H1', ['ASSY'], [
        { id: 1, status: 'in_transit', order_type: 'move', payload_code: 'ASSY', source_node: 'UNL-H1', delivery_node: 'EMPTIES' },
    ]);
    eq(labelFor(html, 'ASSY'), 'IN TRANSIT', 'U2 tagged with the part: row is IN TRANSIT');
})();

// The blank U2 at a one-row node lights that row. Before D2 it read no demand.
(function () {
    const html = render('consume', 'UNL-H1', ['ASSY'], [
        { id: 1, status: 'in_transit', order_type: 'move', payload_code: '', source_node: 'UNL-H1', delivery_node: 'EMPTIES' },
    ]);
    eq(labelFor(html, 'ASSY'), 'IN TRANSIT', 'blank U2, one row: IN TRANSIT');
})();

// The blank U2 at a node with SEVERAL rows lights none of them.
(function () {
    const html = render('consume', 'UNL-W1', ['ASSY', 'BRKT'], [
        { id: 1, status: 'in_transit', order_type: 'move', payload_code: '', source_node: 'UNL-W1', delivery_node: 'EMPTIES' },
    ]);
    eq(labelFor(html, 'ASSY'), 'no demand', 'blank U2, two rows: ASSY not lit');
    eq(labelFor(html, 'BRKT'), 'no demand', 'blank U2, two rows: BRKT not lit');
})();

// MUST STAY: a blank L1 arriving at a produce home is not this node's move out.
(function () {
    const html = render('produce', 'LDR-H1', ['BRKT'], [
        { id: 1, status: 'in_transit', order_type: 'retrieve', retrieve_empty: true, payload_code: '', source_node: 'EMPTY-MKT', delivery_node: 'LDR-H1' },
    ]);
    eq(labelFor(html, 'BRKT'), 'no demand', 'blank L1 to a produce home: no demand');
})();

// MUST STAY: a blank move arriving at a consume node from elsewhere lights no row.
(function () {
    const html = render('consume', 'UNL-H1', ['ASSY'], [
        { id: 1, status: 'in_transit', order_type: 'move', payload_code: '', source_node: 'STAGE-A', delivery_node: 'UNL-H1' },
    ]);
    eq(labelFor(html, 'ASSY'), 'no demand', 'blank move arriving at a consume node: no demand');
})();

console.log('operator-modal home transit: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed === 0 ? 0 : 1);
