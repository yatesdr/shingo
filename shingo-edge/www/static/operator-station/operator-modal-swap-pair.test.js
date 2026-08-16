// Unit tests for swapPair (operator-modal.js) — which two orders at a node are
// the coordinated pair.
//
// WHY THIS IS PINNED. The node's active-order list is "every non-terminal order
// whose process_node_id is this node", and that is NOT the same set as "the two
// legs of this swap". `delivered` is not a terminal status, so an order Core
// finished but the Edge never heard about stays in the list indefinitely.
// Springfield ALN_001, 2026-08-03: order 3980 sat there from 18:49 to 22:39.
//
// Two consumers read that list POSITIONALLY and both got the wrong answer:
//   - the blocker label took the first non-staged order over a created_at-sorted
//     list, so the OLDEST leftover won and the operator was told "Waiting for
//     material: 76683-6TA0A.06" — the style the line was changing AWAY FROM —
//     during the changeover to 76682.
//   - the >=2 count decided whether the disabled WAITING button appears at all.
//     That count is the recovery surface: when one leg of a swap dies it should
//     drop to 1 so the arm steps aside and the survivor's own branch offers a
//     working button. A ghost padded it back to 2 and held the dead button up.
//
// Runs under plain Node (no npm), extracting the SHIPPING swapPair out of
// operator-modal.js in a vm so it can't drift from a copy. Exit 0 = pass.
// Run via the Go wrapper operator_modal_swap_pair_test.go.

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

// Same brace-matching extractor as operator-modal-waiting.test.js: modal.js
// touches the DOM at module scope and can't be loaded wholesale, but swapPair is
// pure.
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
const ctx = vm.createContext({ console: console });
vm.runInContext(extractFn(modalSrc, 'swapPair') + '\nthis.swapPair = swapPair;', ctx);
const swapPair = ctx.swapPair;

const ids = list => list.map(o => o.id).join(',');

// ── The live pair, cleanly ─────────────────────────────────────────────
// Evac parked at the node, supply still moving. Both carry the sibling pointer.
const evac = { id: 3994, status: 'staged', sibling_order_id: 3993 };
const supply = { id: 3993, status: 'in_transit', sibling_order_id: 3994 };

eq(ids(swapPair([supply, evac])), '3994,3993', 'clean pair resolves, staged leg anchors');

// ── The incident ───────────────────────────────────────────────────────
// A finished order Core confirmed 2½ hours earlier, still non-terminal on the
// Edge, still carrying the reason it was queued with, and FIRST in created_at
// order — which is exactly how it won the blocker slot.
const ghost = {
    id: 3980,
    status: 'delivered',
    queue_reason: 'Waiting for material: 76683-6TA0A.06',
    sibling_order_id: 3981,   // its own long-dead partner, not in the active list
};

const withGhost = [ghost, supply, evac];
eq(ids(swapPair(withGhost)), '3994,3993', 'ghost is excluded — the pair is resolved by linkage, not by age');

// The blocker the modal derives from the pair must be the leg that is actually
// still moving, never the ghost. This is the assertion the operator felt.
const pair = swapPair(withGhost);
const blocker = pair.find(o => o.status !== 'staged') || null;
eq(blocker && blocker.id, 3993, 'blocker is the in-transit supply leg');
eq(blocker && blocker.queue_reason, undefined, 'blocker carries no stale reason to display');

// ── The recovery surface ───────────────────────────────────────────────
// One leg cancelled/failed drops out of the active list. The pair must NOT
// resolve, so the >=2 guard fails, the disabled WAITING arm steps aside, and the
// survivor's own branch renders a usable button. A ghost must not prop it up.
eq(swapPair([evac]).length, 0, 'half a pair does not resolve — recovery surface opens');
eq(swapPair([ghost, evac]).length, 0, 'a ghost cannot stand in for the dead leg');

// ── Degenerate input ───────────────────────────────────────────────────
eq(swapPair([]).length, 0, 'empty list');
eq(swapPair([{ id: 1, status: 'staged' }, { id: 2, status: 'in_transit' }]).length, 0,
    'no sibling pointers at all → no pair (single-leg flows use per-order release)');

// Neither leg parked yet — both still inbound. The staged-first anchor misses,
// so the fallback (any leg carrying a pointer) has to resolve it, or a pair in
// this state loses its label entirely.
const bothMoving = [
    { id: 4002, status: 'in_transit', sibling_order_id: 4003 },
    { id: 4003, status: 'in_transit', sibling_order_id: 4002 },
];
eq(swapPair(bothMoving).length, 2, 'pair with neither leg staged still resolves via fallback anchor');

// A pointer aimed at an order that is not here (terminal, or never projected)
// must not fabricate a pair out of the anchor alone.
eq(swapPair([{ id: 5001, status: 'staged', sibling_order_id: 9999 }]).length, 0,
    'dangling sibling pointer resolves to nothing, not to a one-legged pair');

console.log('swapPair: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed === 0 ? 0 : 1);
