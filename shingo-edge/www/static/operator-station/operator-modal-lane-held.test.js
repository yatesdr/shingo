// Unit tests for isStationReleasable (operator-modal.js) — which staged orders
// the RELEASE button is offered for.
//
// WHY THIS IS PINNED. A gate-held order is `staged` because Core wrote that
// status: the robot reached the lane's wait point, RDS reported WAITING, and
// MapState turned that into staged. So the board was INVITED to render RELEASE
// for a wait whose precondition is a lane being safe — something nobody at a
// station can observe or bring about. Core now refuses that release outright,
// which means a surviving button would be one whose only correct outcome is an
// error message.
//
// The distinction this pins is CONTROL versus STATUS. The order must still
// render, with its status and its reason; only the button goes. Suppressing the
// staged status instead would leave the tile claiming a parked robot is still
// driving, which is worse than a useless button.
//
// Runs under plain Node (no npm): extracts the real isStationReleasable out of
// operator-modal.js in a vm. Exit 0 = pass, 1 = any failure. Run via the Go
// wrapper operator_modal_lane_held_test.go.

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

// Same brace-matching extraction the waitingLabel tests use, and for the same
// reason: exercise the SHIPPING source rather than a copy that can drift.
// operator-modal.js cannot be loaded wholesale — it touches the DOM at module
// scope — but isStationReleasable is pure.
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

const modalSrc = fs.readFileSync(
    path.join(__dirname, 'operator-modal.js'), 'utf8');
const ctx = { console };
vm.createContext(ctx);
vm.runInContext(extractFn(modalSrc, 'isStationReleasable'), ctx);
const isStationReleasable = ctx.isStationReleasable;

// The case this exists for: Core parked the robot at a lane gate.
eq(isStationReleasable({ status: 'staged', lane_held: true }), false,
    'a lane-held staged order offers no RELEASE — its wait belongs to Core');

// The case that must keep working: the station's own wait. This is the guard
// against the fence degenerating into "never offer RELEASE".
eq(isStationReleasable({ status: 'staged', lane_held: false }), true,
    'a station-held staged order still offers RELEASE');

// Absent field — an older Core/Edge pairing, or a row written before the column
// was computed. Must read as releasable, i.e. exactly today's behaviour, so the
// suppression can never be caused by a MISSING signal.
eq(isStationReleasable({ status: 'staged' }), true,
    'absent lane_held degrades to releasable, not to suppressed');

// Nothing but `staged` is releasable, lane_held or not — the button belongs to
// one status and this predicate must not widen it.
eq(isStationReleasable({ status: 'in_transit', lane_held: false }), false, 'in_transit is not releasable');
eq(isStationReleasable({ status: 'in_transit', lane_held: true }), false, 'lane-held in_transit is not releasable');
eq(isStationReleasable({ status: 'delivered', lane_held: false }), false, 'delivered is not releasable');
eq(isStationReleasable({ status: 'queued', lane_held: false }), false, 'queued is not releasable');
eq(isStationReleasable({}), false, 'an order with no status is not releasable');

console.log('operator-modal isStationReleasable: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed === 0 ? 0 : 1);
