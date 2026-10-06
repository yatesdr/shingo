// Characterisation pins for the mission-detail page, taken at the pre-change
// tree (2026-10-05) before the page returns to order stages (ruling R6). Run
// under plain Node via the Go wrapper mission_detail_stages_test.go. Exit 0 on
// pass, 1 on any assertion failure.
//
// These assert what the page SHOWS TODAY for three fixture missions, defects
// included, so the R6 change can be read as a diff of expectations rather than
// as a rewrite nobody can check.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let failures = 0;
function check(name, cond, detail) {
    if (cond) {
        console.log('  ok  ' + name);
    } else {
        failures++;
        console.log('  FAIL ' + name + (detail ? ' — ' + detail : ''));
    }
}

// --- harness -------------------------------------------------------------
// Enough DOM for the page's render calls and a fetch that serves one canned
// mission payload. Returns every element the page wrote to.
function render(payload) {
    const els = {};
    function getEl(id) {
        if (!els[id]) els[id] = { id: id, textContent: '', innerHTML: '', style: {} };
        return els[id];
    }
    getEl('mission-order-id').textContent = String(payload.order.id);

    const ctxObj = {
        console: console,
        document: {
            getElementById: getEl,
            querySelector: function() { return null; },
            querySelectorAll: function() { return []; },
        },
        fetch: function() {
            return Promise.resolve({ json: function() { return Promise.resolve(payload); } });
        },
        onSSE: function() {},
        debounce: function(fn) { return fn; },
        api: {},
        el: function() {},
        h: function() {},
        formatTime: function(ts) { return ts ? 'T(' + ts + ')' : '-'; },
        relevantNotices: function(items) { return items || []; },
    };
    ctxObj.window = ctxObj;
    vm.createContext(ctxObj);
    const src = fs.readFileSync(path.join(__dirname, 'mission-detail.js'), 'utf8')
        .replace(/^import[^;]+;\s*/gm, '');
    vm.runInContext(src, ctxObj);
    return new Promise(function(resolve) { setImmediate(resolve); }).then(function() { return els; });
}

function text(html) { return String(html || '').replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ').trim(); }
function count(s, re) { return (String(s).match(re) || []).length; }

// --- fixtures --------------------------------------------------------------
// Payloads in the shape /api/missions/{id} serves: order, telemetry (null when
// the order has no mission_telemetry row yet), events (vendor transitions with
// old_status/new_status mapped server-side, plus leg rows flagged is_leg), and
// history (order_history). Leg rows carry the block-view chip exactly as the
// pre-change server builds it: the leg JSON's keys (blockId, binTask) do not
// match fleet.BlockSnapshot's (block_id, state), so the chip arrives with a
// location and blank id/state/status.

const Z = { robot_x: 0, robot_y: 0, robot_battery: 80, robot_station: '' };

function ev(at, oldState, newState, oldStatus, newStatus) {
    return Object.assign({ created_at: at, robot_id: 'AMR-01', old_state: oldState, new_state: newState,
        old_status: oldStatus, new_status: newStatus, is_leg: false, blocks: null, blocks_json: '[]' }, Z);
}
function leg(at, location, binTask, start, end) {
    return Object.assign({ created_at: at, robot_id: 'AMR-01', old_state: '', new_state: 'BLOCK_FINISHED',
        old_status: '', new_status: '', is_leg: true,
        blocks: [{ block_id: '', location: location, state: '', status: '' }],
        blocks_json: JSON.stringify([{ blockId: 'b', location: location, binTask: binTask,
            startTime: start, terminateTime: end, durationSeconds: end - start }]) }, Z);
}
function hist(at, status, detail) { return { status: status, detail: detail || '', created_at: at }; }
function sec(iso) { return Date.parse(iso) / 1000; }

// F1 — finished swap with a staged wait (the audit's #632 shape). 11m 02s
// staged at Lane_03-WAIT, one fault/recover pair while moving, confirmed 26 s
// after delivery.
const F1 = {
    order: { id: 632, order_type: 'swap', station_id: 'line-1', robot_id: 'AMR-01', status: 'confirmed',
        source_node: 'ALN_002', delivery_node: 'UTN_013', created_at: '2026-10-05T07:00:00Z',
        steps_json: JSON.stringify([{ action: 'pickup', node: 'ALN_002' }, { action: 'wait', node: 'Lane_03-WAIT' },
            { action: 'dropoff', node: 'UTN_013' }]) },
    telemetry: { robot_id: 'AMR-01', duration_ms: 870000, vendor_duration_ms: 0,
        core_created: '2026-10-05T07:00:00Z', core_completed: '2026-10-05T07:14:30Z',
        errors_json: '[]', warnings_json: '[]', notices_json: '[]' },
    history: [
        hist('2026-10-05T07:00:00Z', 'pending', 'order created'),
        hist('2026-10-05T07:00:05Z', 'dispatched'),
        hist('2026-10-05T07:00:10Z', 'in_transit'),
        hist('2026-10-05T07:01:40Z', 'staged'),
        hist('2026-10-05T07:12:42Z', 'in_transit'),
        hist('2026-10-05T07:13:00Z', 'faulted', 'replanning'),
        hist('2026-10-05T07:13:20Z', 'in_transit'),
        hist('2026-10-05T07:14:30Z', 'delivered'),
        hist('2026-10-05T07:14:56Z', 'confirmed'),
    ],
    events: [
        ev('2026-10-05T07:00:05Z', '', 'CREATED', '', 'dispatched'),
        ev('2026-10-05T07:00:10Z', 'CREATED', 'RUNNING', 'dispatched', 'in_transit'),
        leg('2026-10-05T07:01:00Z', 'ALN_002', 'JackLoad', sec('2026-10-05T07:00:40Z'), sec('2026-10-05T07:00:58Z')),
        ev('2026-10-05T07:01:40Z', 'RUNNING', 'WAITING', 'in_transit', 'staged'),
        ev('2026-10-05T07:12:42Z', 'WAITING', 'RUNNING', 'staged', 'in_transit'),
        leg('2026-10-05T07:12:43Z', 'Lane_03-WAIT', 'Wait', sec('2026-10-05T07:01:40Z'), sec('2026-10-05T07:12:42Z')),
        leg('2026-10-05T07:14:29Z', 'UTN_013', 'JackUnload', sec('2026-10-05T07:14:10Z'), sec('2026-10-05T07:14:28Z')),
        ev('2026-10-05T07:14:30Z', 'RUNNING', 'FINISHED', 'in_transit', 'delivered'),
    ],
};

// F2 — queued, then dispatched (the audit's #610 shape). 24 minutes in the
// queue before a robot was asked for.
const F2 = {
    order: { id: 610, order_type: 'retrieve', station_id: 'line-2', robot_id: 'AMR-02', status: 'confirmed',
        source_node: 'ALN_002', delivery_node: 'UTN_010', created_at: '2026-10-05T06:59:00Z' },
    telemetry: { robot_id: 'AMR-02', duration_ms: 1500000, vendor_duration_ms: 0,
        core_created: '2026-10-05T06:59:00Z', core_completed: '2026-10-05T07:24:00Z',
        errors_json: '[]', warnings_json: '[]', notices_json: '[]' },
    history: [
        hist('2026-10-05T06:59:00Z', 'pending', 'order created'),
        hist('2026-10-05T06:59:01Z', 'queued', 'waiting for a slot at UTN_010'),
        hist('2026-10-05T07:23:00Z', 'dispatched'),
        hist('2026-10-05T07:23:05Z', 'in_transit'),
        hist('2026-10-05T07:24:00Z', 'delivered'),
        hist('2026-10-05T07:24:19Z', 'confirmed'),
    ],
    events: [
        ev('2026-10-05T07:23:00Z', '', 'CREATED', '', 'dispatched'),
        ev('2026-10-05T07:23:05Z', 'CREATED', 'RUNNING', 'dispatched', 'in_transit'),
        leg('2026-10-05T07:23:31Z', 'ALN_002', 'JackLoad', sec('2026-10-05T07:23:20Z'), sec('2026-10-05T07:23:30Z')),
        leg('2026-10-05T07:23:59Z', 'UTN_010', 'JackUnload', sec('2026-10-05T07:23:50Z'), sec('2026-10-05T07:23:58Z')),
        ev('2026-10-05T07:24:00Z', 'RUNNING', 'FINISHED', 'in_transit', 'delivered'),
    ],
};

// F3 — in flight (the audit's #664 shape): staged since 07:20:55 and still
// there. No mission_telemetry row exists until a terminal state, so the API
// serves telemetry null.
const F3 = {
    order: { id: 664, order_type: 'retrieve', station_id: 'line-3', robot_id: 'AMR-13', status: 'staged',
        source_node: 'ALN_008', delivery_node: 'PLK_H1', created_at: '2026-10-05T07:20:00Z' },
    telemetry: null,
    history: [
        hist('2026-10-05T07:20:00Z', 'pending', 'order created'),
        hist('2026-10-05T07:20:02Z', 'dispatched'),
        hist('2026-10-05T07:20:05Z', 'in_transit'),
        hist('2026-10-05T07:20:55Z', 'staged'),
    ],
    events: [
        ev('2026-10-05T07:20:02Z', '', 'CREATED', '', 'dispatched'),
        ev('2026-10-05T07:20:05Z', 'CREATED', 'RUNNING', 'dispatched', 'in_transit'),
        leg('2026-10-05T07:20:41Z', 'ALN_008', 'JackLoad', sec('2026-10-05T07:20:30Z'), sec('2026-10-05T07:20:40Z')),
        ev('2026-10-05T07:20:55Z', 'RUNNING', 'WAITING', 'in_transit', 'staged'),
    ],
};

// --- pins: the page as it renders at the pre-change tree --------------------

async function main() {
    console.log('F1 finished swap with a staged wait (pre-change)');
    {
        const els = await render(F1);
        const legend = text(els['duration-legend'].innerHTML);
        check('legend leads with the leg bar, unaccounted is a segment',
            legend.indexOf('unaccounted: 42s') !== -1, legend);
        check('legend footer counts unaccounted as legs',
            legend.indexOf('legs 14m 30s of 14m 30s total') !== -1, legend);
        check('the staged wait is a leg, not a stage',
            legend.indexOf('wait @ Lane_03-WAIT: 11m 2s') !== -1, legend);
        check('the timeline prints every event (5 transitions + 3 legs)',
            count(els['mission-timeline'].innerHTML, /class="timeline-entry"/g) === 8);
        check('every leg carries an empty block chip',
            count(els['mission-timeline'].innerHTML, /UTN_013: -|ALN_002: -|Lane_03-WAIT: -/g) === 3,
            els['mission-timeline'].innerHTML);
        check('the event log prints the same 8 rows again',
            count(els['event-log'].innerHTML, /<tr>/g) === 8);
        check('a zero position prints as (0.0, 0.0)',
            els['mission-timeline'].innerHTML.indexOf('(0.0, 0.0)') !== -1);
        check('the fault/recover pair in history is not shown at all',
            text(els['mission-timeline'].innerHTML).indexOf('faulted') === -1);
        const summary = text(els['mission-summary'].innerHTML);
        check('total duration is telemetry duration_ms (created to vendor terminal)',
            summary.indexOf('Total Duration 14m 30s') !== -1, summary);
        check('fleet duration reads "-"', summary.indexOf('Fleet Duration -') !== -1, summary);
    }

    console.log('F2 queued then dispatched (pre-change)');
    {
        const els = await render(F2);
        const legend = text(els['duration-legend'].innerHTML);
        check('the 24-minute queue is "unaccounted"',
            legend.indexOf('unaccounted: 24m 22s') !== -1, legend);
        check('no stage names the queue',
            legend.indexOf('queued') === -1 && text(els['mission-timeline'].innerHTML).indexOf('queued') === -1);
    }

    console.log('F3 in flight (pre-change)');
    {
        const els = await render(F3);
        const summary = text(els['mission-summary'].innerHTML);
        check('total duration is a dash', summary.indexOf('Total Duration -') !== -1, summary);
        check('core created / completed are dashes',
            summary.indexOf('Core Created -') !== -1 && summary.indexOf('Core Completed -') !== -1, summary);
        const legend = text(els['duration-legend'].innerHTML);
        check('the bar is the one leg, against a dash total',
            legend.indexOf('legs 10s of - total') !== -1, legend);
    }

    console.log('');
    if (failures > 0) {
        console.log(failures + ' assertion(s) failed');
        process.exit(1);
    }
    console.log('all mission-detail stage assertions passed');
}

main().catch(function(err) { console.error(err); process.exit(1); });
