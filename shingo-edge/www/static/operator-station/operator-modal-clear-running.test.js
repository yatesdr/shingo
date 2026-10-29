// Unit tests for the node modal's header status line in renderModal
// (operator-modal.js) while a loader home's clear runs.
//
// WHY THIS IS PINNED. A home's clear is authored by Core: the Edge sends one
// request and writes no runtime order pointer (engine/operator_home_clear.go),
// and the board draws the clear's moves from the order rows Core pushes onto the
// tile (entry.orders). The header read only the runtime pointer
// (isReplenishing), so with the clear's move driving off the home it said "No
// active orders" over a queue that listed that move. The header now reads the
// tile's own live orders as well.
//
// Runs under plain Node (no npm): extracts the real renderModal out of
// operator-modal.js in a vm, with its collaborators stubbed as the other
// operator-modal tests stub them. Exit 0 = pass, 1 = any failure. Run via the
// Go wrapper operator_modal_clear_running_test.go.

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
const windowSrc = fs.readFileSync(path.join(__dirname, 'operator-window-state.js'), 'utf8');
const modelSrc = fs.readFileSync(path.join(__dirname, 'operator-board-model.js'), 'utf8')
    .replace(/^import\s[^;]*;/gm, '').replace(/^export\s+/gm, '');
const statusSrc = fs.readFileSync(path.join(__dirname, 'order-status.js'), 'utf8')
    .replace(/^export\s+/gm, '');

const content = { innerHTML: '', querySelectorAll: () => [] };
const ctx = {
    console, JSON, Number, Array, Math, Date,
    nodeModalContent: content,
    esc: (s) => String(s),
    fillColor: () => '#000',
    // The home's runtime carries no order pointer while Core's clear runs.
    isReplenishing: () => false,
    distinctQueueCauses: () => [],
    getView: () => ({ process: { id: 1 } }),
    handleModalAction: () => {},
    REFUSE_LABEL: 'NO PARTS AVAILABLE',
    UNDO_LABEL: 'UNDO',
};
vm.createContext(ctx);
vm.runInContext(statusSrc, ctx);
vm.runInContext(modelSrc, ctx);
vm.runInContext('var PRESS_MAX_MS = 2000; var pressInFlight = false; var pressStartedAt = 0; var pendingEntry = null; var posting = new Set();', ctx);
vm.runInContext(extractFn(modalSrc, 'pressIsStale'), ctx);
vm.runInContext(extractFn(modalSrc, 'actionBtn'), ctx);
vm.runInContext("var TO_MARKET_LABEL = 'TO MARKET'; function withQueueCause(s) { return s; }", ctx);
vm.runInContext(extractFn(modalSrc, 'orderStatusChip'), ctx);
vm.runInContext(extractFn(windowSrc, 'windowRole'), ctx);
vm.runInContext(extractFn(windowSrc, 'isOwnEmptyOut'), ctx);
vm.runInContext(extractFn(modalSrc, 'loaderOffersClear'), ctx);
vm.runInContext(extractFn(modalSrc, 'renderModal') + '\nthis.renderModal = renderModal;', ctx);

// status is the header's status line, as rendered.
function status(orders) {
    ctx.renderModal({
        node: { id: 7, name: 'DLH_1', core_node_name: 'DLH_1' },
        loader: { role: 'produce', layout: 'dedicated_positions', parts: [{ code: 'PART-A1' }] },
        bin_state: { occupied: true, payload_code: 'PART-A1', bin_label: 'KD-7', stocked: false },
        runtime: {},
        orders: orders,
    });
    const m = content.innerHTML.match(/<div class="modal-status">([^<]*)/);
    return m ? m[1] : '(no status line)';
}

// The clear's move of the emptied carrier off the home, driving.
const clearMove = { id: 41, status: 'in_transit', order_type: 'move', payload_code: '', source_node: 'DLH_1', delivery_node: 'DLB_2' };

(function () {
    const got = status([clearMove]);
    eq(got.indexOf('[REP] ') === 0, true, 'a clear running at the home: the header says an order is running (got ' + got + ')');
    eq(got.indexOf('No active orders') < 0, true, 'a clear running at the home: not "No active orders"');
})();

(function () {
    eq(status([]), 'No active orders', 'nothing running at the home: "No active orders"');
})();

(function () {
    eq(status([Object.assign({}, clearMove, { status: 'confirmed' })]), 'No active orders',
        'the clear finished: "No active orders"');
})();

console.log('operator-modal clear running: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed === 0 ? 0 : 1);
