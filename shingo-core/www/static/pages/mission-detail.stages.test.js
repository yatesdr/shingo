// Tests for the mission-detail page's stage view (ruling R6). Run under plain
// Node via the Go wrapper mission_detail_stages_test.go. Exit 0 on pass, 1 on
// any assertion failure.
//
// The page renders the order's life from order_history: one row per status
// span, one bar on a true time scale in four classes, fault/recover pairs
// folded into the span they interrupted, and every robot action once, in a
// closed disclosure grouped by stage. The fixtures are the three the
// characterisation pins were taken on before the change (F1–F3); what each
// showed before is recorded beside the expectation it was replaced by.
//
// The page runs against the REAL formatters (shared/utils.js formatDuration and
// formatTime, components/plantclock.js formatClockSeconds) with the plant zone
// pinned to UTC, so the strings asserted here are the strings an operator reads.

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
// Enough DOM for the page's render calls, a fetch that serves one canned
// mission payload, and the server clock pinned to `now`. Returns every element
// the page wrote to.
const UTILS = fs.readFileSync(path.join(__dirname, '..', '..', '..', '..', 'shared', 'utils.js'), 'utf8')
    .replace(/^export /gm, '');
// plantclock.js imports serverNow from utils.js and keeps its own formatter
// cache, so it runs in a context of its own with serverNow handed in.
const PLANTCLOCK = fs.readFileSync(path.join(__dirname, '..', 'components', 'plantclock.js'), 'utf8')
    .replace(/^import[^;]+;\s*/gm, '')
    .replace(/^export /gm, '')
    + '\n;__out = { formatClockSeconds: formatClockSeconds, plantDate: plantDate };';
const PAGE = fs.readFileSync(path.join(__dirname, 'mission-detail.js'), 'utf8')
    .replace(/^import[^;]+;\s*/gm, '');

function render(payload, now) {
    const els = {};
    function getEl(id) {
        if (!els[id]) els[id] = { id: id, textContent: '', innerHTML: '', style: {}, querySelectorAll: () => [] };
        return els[id];
    }
    getEl('mission-order-id').textContent = String(payload.order.id);

    const ctxObj = {
        console: console,
        Intl: Intl,
        setTimeout: setTimeout,
        clearTimeout: clearTimeout,
        document: {
            getElementById: getEl,
            querySelector: function() { return null; },
            querySelectorAll: function() { return []; },
            addEventListener: function() {},
            // el() and the page create elements; escapeHtml no longer does.
            createElement: function() {
                return { appendChild: function(t) {
                    this.innerHTML = String(t).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
                } };
            },
            createTextNode: function(s) { return s; },
        },
        fetch: function() {
            return Promise.resolve({ json: function() { return Promise.resolve(payload); } });
        },
        relevantNotices: function(items) { return items || []; },
        addEventListener: function() {},
        PLANT_TZ: 'UTC',
        SHINGO_CLOCK: { now: now || '2026-10-05T16:30:00Z', speed: 1, sim: false },
    };
    ctxObj.window = ctxObj;
    vm.createContext(ctxObj);
    vm.runInContext(UTILS, ctxObj);
    const pc = { Intl: Intl, Date: Date, Map: Map, window: ctxObj, serverNow: ctxObj.serverNow };
    vm.createContext(pc);
    vm.runInContext(PLANTCLOCK, pc);
    ctxObj.formatClockSeconds = pc.__out.formatClockSeconds;
    // onSSE would open an EventSource; the page's subscriptions are not under test.
    vm.runInContext('onSSE = function() {};', ctxObj);
    vm.runInContext(PAGE, ctxObj);
    return new Promise(function(resolve) { setImmediate(resolve); }).then(function() {
        if (!els['stage-rows']) {
            throw new Error('renderStages never ran; page said: '
                + (els['mission-loading'] ? els['mission-loading'].textContent : '(nothing)'));
        }
        return els;
    });
}

function text(html) { return String(html || '').replace(/<[^>]*>/g, ' ').replace(/&lt;/g, '<').replace(/\s+/g, ' ').trim(); }
function summaryLabels(els) {
    return (els['mission-summary'].innerHTML.match(/<strong>([^<]*)<\/strong>/g) || [])
        .map((m) => m.replace(/<\/?strong>/g, '')).join('|');
}
function count(s, re) { return (String(s).match(re) || []).length; }
function rows(els) {
    return (els['stage-rows'].innerHTML.match(/<tr class="stage-row[^"]*">[\s\S]*?<\/tr>/g) || []).map(text);
}

// --- fixtures --------------------------------------------------------------
// Payloads in the shape /api/missions/{id} serves: order, telemetry (null when
// the order has no mission_telemetry row yet), events (vendor transitions with
// old_status/new_status mapped server-side, plus leg rows flagged is_leg, whose
// blocks_json is the engine's blockLeg record), and history (order_history).

const Z = { robot_x: 0, robot_y: 0, robot_battery: 80, robot_station: '' };

function ev(at, oldState, newState, oldStatus, newStatus) {
    return Object.assign({ created_at: at, robot_id: 'AMR-01', old_state: oldState, new_state: newState,
        old_status: oldStatus, new_status: newStatus, is_leg: false, blocks_json: '[]' }, Z);
}
function legRow(at, blocks) {
    return Object.assign({ created_at: at, robot_id: 'AMR-01', old_state: '', new_state: 'BLOCK_FINISHED',
        old_status: '', new_status: '', is_leg: true, blocks_json: JSON.stringify(blocks) }, Z);
}
function blk(location, binTask, start, end) {
    return { blockId: 'b', location: location, binTask: binTask, startTime: start, terminateTime: end,
        durationSeconds: end >= start && start > 0 ? end - start : 0 };
}
function leg(at, location, binTask, start, end) { return legRow(at, [blk(location, binTask, start, end)]); }
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
// there at 16:30. No mission_telemetry row exists until a terminal state, so
// the API serves telemetry null.
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

// F4 — three replans while moving, then the fleet gave up.
const F4 = {
    order: { id: 700, order_type: 'retrieve', station_id: 'line-1', robot_id: 'AMR-03', status: 'failed',
        source_node: 'ALN_001', delivery_node: 'UTN_001', created_at: '2026-10-05T08:00:00Z' },
    telemetry: null,
    history: [
        hist('2026-10-05T08:00:00Z', 'pending', 'order created'),
        hist('2026-10-05T08:00:10Z', 'dispatched'),
        hist('2026-10-05T08:00:20Z', 'in_transit'),
        hist('2026-10-05T08:01:00Z', 'faulted'),
        hist('2026-10-05T08:01:10Z', 'in_transit'),
        hist('2026-10-05T08:02:00Z', 'faulted'),
        hist('2026-10-05T08:02:30Z', 'in_transit'),
        hist('2026-10-05T08:03:00Z', 'faulted'),
        hist('2026-10-05T08:05:00Z', 'failed', 'fault grace expired'),
    ],
    events: [],
};

// --- tests -----------------------------------------------------------------

async function main() {
    console.log('F1 finished swap with a staged wait');
    {
        const els = await render(F1);
        const r = rows(els);
        // Before: a leg bar whose biggest named segment was "unaccounted 42s",
        // footer "legs 14m 30s of 14m 30s total"; the staged wait was a leg.
        check('one row per status span, plus the terminal row', r.length === 7, JSON.stringify(r));
        check('pending', r[0] === 'pending order created 07:00:00 5 s 1%', r[0]);
        check('dispatched', r[1] === 'dispatched 07:00:05 5 s 1%', r[1]);
        check('in transit, where the robot loaded', r[2] === 'in_transit ALN_002 07:00:10 1m 30s 10%', r[2]);
        check('the staged wait is a stage, at the wait node',
            r[3] === 'staged Lane_03-WAIT 07:01:40 11m 74%', r[3]);
        check('the fault/recover pair folds into the span it interrupted',
            r[4] === 'in_transit 1 fault, 20 s lost UTN_013 07:12:42 1m 48s 12%', r[4]);
        check('delivered, awaiting confirmation', r[5] === 'delivered 07:14:30 26 s 3%', r[5]);
        check('the terminal row ends the life and takes none of it', r[6] === 'confirmed 07:14:56', r[6]);
        // Round-2 ruling 7. Before: the span after delivery was drawn held.
        // No robot is on a delivered order, so it is its own neutral class.
        check('the delivered span is waiting for confirm, not held',
            /<span class="stage-swatch stage-confirm"><\/span><span class="badge badge-delivered"/.test(els['stage-rows'].innerHTML),
            els['stage-rows'].innerHTML);
        check('rows sharing a minute show seconds', r[0].indexOf('07:00:00') !== -1 && r[1].indexOf('07:00:05') !== -1);

        const legend = text(els['stage-legend'].innerHTML);
        // Before (round 1): 'waiting to dispatch 5 s 1% moving 3m 23s 23% held 11m 77%'.
        check('four classes that sum to the life',
            legend === 'waiting to dispatch 5 s 1% moving 3m 23s 23% held 11m 74% waiting for confirm 26 s 3%', legend);
        check('nothing is unaccounted', (els['stage-bar'].innerHTML + legend).indexOf('unaccounted') === -1);
        check('the bar is on a true time scale (flex = milliseconds, no minimum width)',
            els['stage-bar'].innerHTML.indexOf('class="stage-seg stage-held" style="flex:662000 0 0"') !== -1
            && count(els['stage-bar'].innerHTML, /class="stage-seg /g) === 6, els['stage-bar'].innerHTML);

        const summary = text(els['mission-summary'].innerHTML);
        // Before: "Total Duration 14m 30s" (telemetry: created to the vendor's
        // terminal report). Now the order's life, created to confirmed.
        check('order life is the sum of the stages', summary.indexOf('Order life 14m') !== -1, summary);
        // Round-2 ruling 7. Before: no Duration field. Now the Missions list's
        // Duration, same number and name, beside the order life it differs from.
        // 870,000 ms prints "14m" on the shared ladder, as the list does.
        check('summary fields', summaryLabels(els) === 'Order ID|Type|Station|Robot|Route|Status|Created|Ended|Duration|Order life'
            + '|Fleet duration|Fleet created|Fleet completed', summaryLabels(els));
        check('Duration is telemetry.duration_ms, as the Missions list prints it',
            summary.indexOf('Duration 14m Order life 14m') !== -1, summary);
        check('ended at the terminal row, plant-local', summary.indexOf('Ended Oct 5, 2026 07:14 UTC') !== -1, summary);
        check('an absent fleet figure is a titled em dash, not "-"',
            els['mission-summary'].innerHTML.indexOf('title="not reported by the fleet">—') !== -1
            && summary.indexOf(' - ') === -1, summary);

        // Before: 8 timeline entries and the same 8 rows again in the Event Log,
        // an empty "UTN_013: -" chip on every leg, "(0.0, 0.0)" on every row.
        check('the timeline and the event log are gone', !els['mission-timeline'] && !els['event-log']);
        check('every robot action is listed once', els['mission-actions-count'].textContent === '8'
            && count(els['mission-actions'].innerHTML, /class="action-row"/g) === 8);
        check('no block chips', els['mission-actions'].innerHTML.indexOf('UTN_013: -') === -1
            && els['mission-actions'].innerHTML.indexOf('badge-sm') === -1);
        check('no position is blank, not (0.0, 0.0)', els['mission-actions'].innerHTML.indexOf('(0.0, 0.0)') === -1);
        check('actions are grouped by stage (6 spans, all with actions except pending)',
            count(els['mission-actions'].innerHTML, /class="actions-group"/g) === 5);
        check('the wait leg is filed under the staged span it happened in',
            /badge-staged">staged<\/span>[\s\S]*?wait @ Lane_03-WAIT · <span class="tnum">11m<\/span>/.test(els['mission-actions'].innerHTML));
    }

    console.log('F2 queued then dispatched');
    {
        const els = await render(F2);
        const r = rows(els);
        // Before: the queue was 24m 22s of "unaccounted"; "queued" appeared nowhere.
        check('the queue is a named stage with its reason',
            r[1] === 'queued waiting for a slot at UTN_010 06:59:01 23m 95%', r[1]);
        check('in transit names both places the robot worked',
            r[3] === 'in_transit ALN_002 → UTN_010 07:23:05 55 s 4%', r[3]);
        const legend = text(els['stage-legend'].innerHTML);
        check('waiting to dispatch carries the queue', legend.indexOf('waiting to dispatch 24m 95%') === 0, legend);
        check('no held time: the 19 s after delivery is waiting for confirm',
            legend.indexOf('held 0 s 0%') !== -1 && legend.indexOf('waiting for confirm 19 s 1%') !== -1, legend);
        check('order life', text(els['mission-summary'].innerHTML).indexOf('Order life 25m') !== -1);
    }

    console.log('F3 in flight');
    {
        const els = await render(F3, '2026-10-05T16:30:00Z');
        const summary = text(els['mission-summary'].innerHTML);
        // Before: Total, Core Created, Core Completed all "-"; bar "legs 10s of - total".
        check('created comes from the order', summary.indexOf('Created Oct 5, 2026 07:20 UTC') !== -1, summary);
        check('ended says in flight', summary.indexOf('Ended in flight') !== -1, summary);
        check('order life runs to now and ticks', summary.indexOf('Order life 9h 10m') !== -1
            && els['mission-summary'].innerHTML.indexOf('data-since="2026-10-05T07:20:00Z"') !== -1, summary);
        check('no fleet block before the fleet has written a summary', summary.indexOf('Fleet') === -1, summary);
        check('summary fields in flight', summaryLabels(els) === 'Order ID|Type|Station|Robot|Route|Status|Created|Ended|Duration|Order life',
            summaryLabels(els));
        // Round-2 follow-up to ruling 7. Before: a titled em dash ("reported
        // when the fleet finishes the order"). Now the Missions list's in-flight
        // Duration, same value and form: ticking from the order's created_at,
        // "so far". Nothing in the summary dashes out.
        check('Duration in flight ticks as the Missions list does', summary.indexOf('Duration 9h 10m so far') !== -1
            && count(els['mission-summary'].innerHTML, /data-since="2026-10-05T07:20:00Z"/g) === 2, summary);
        check('no dashes in the summary', !/ - /.test(summary) && count(summary, /—/g) === 0, summary);
        const r = rows(els);
        check('four spans, no terminal row', r.length === 4, JSON.stringify(r));
        check('the open span runs to now, ticking', r[3] === 'staged 07:20:55 9h 09m so far 100%'
            && els['stage-rows'].innerHTML.indexOf('data-since="2026-10-05T07:20:55Z"') !== -1, r[3]);
        check('a few seconds of a nine-hour life reads <1%, not 0%', r[0] === 'pending order created 07:20:00 2 s <1%', r[0]);
        check('in transit where the robot loaded', r[2] === 'in_transit ALN_008 07:20:05 50 s <1%', r[2]);
    }

    console.log('F4 three replans, then failed');
    {
        const els = await render(F4);
        const r = rows(els);
        // Before: none of this was visible (the page did not read history).
        check('nine history rows become four', r.length === 4, JSON.stringify(r));
        check('three fault/recover pairs are one row saying so',
            r[2] === 'in_transit 3 faults, 2m 40s lost 08:00:20 4m 40s 93%', r[2]);
        check('the terminal row carries its reason', r[3] === 'failed fault grace expired 08:05:00', r[3]);
        check('no actions reported', els['mission-actions-count'].textContent === '0');
    }

    // The three leg forms, carried over from the retired leg-bar tests: `no
    // data`, `zero` and `not applicable` are three answers and the actions list
    // must keep them apart.
    const HIST = [hist('1970-01-01T00:00:00Z', 'pending'), hist('1970-01-01T00:00:01Z', 'in_transit')];
    function legsOnly(events) {
        return { order: { id: 1, status: 'in_transit', created_at: '1970-01-01T00:00:00Z' }, telemetry: null,
            history: HIST, events: events };
    }
    function legTexts(els) {
        return (els['mission-actions'].innerHTML.match(/<tr class="action-row">[\s\S]*?<\/tr>/g) || []).map(text);
    }

    console.log('legs: stopped mission, trailing zero-duration blocks');
    {
        const t = legTexts(await render(legsOnly([
            legRow('1970-01-01T00:20:00Z', [blk('SMN_014', 'Load', 1000, 1030), blk('ALN_001', 'Unload', 1090, 1090),
                blk('SMN_003', 'Wait', 1090, 1090)]),
            ev('1970-01-01T00:20:01Z', 'RUNNING', 'STOPPED', 'in_transit', 'cancelled'),
        ])));
        check('trailing teardown blocks read "not run"',
            t.filter((s) => s.indexOf('· not run') !== -1).length === 2, JSON.stringify(t));
        check('they do not report a duration', t.join('|').indexOf('ALN_001 · 0 s') === -1, JSON.stringify(t));
        check('the block that did run reports its duration', t.join('|').indexOf('load @ SMN_014 · 30 s') !== -1, JSON.stringify(t));
    }

    console.log('legs: finished mission, zero-duration block');
    {
        const t = legTexts(await render(legsOnly([
            legRow('1970-01-01T00:20:00Z', [blk('SMN_014', 'Load', 1000, 1030), blk('ALN_001', 'Unload', 1060, 1060)]),
            ev('1970-01-01T00:20:01Z', 'RUNNING', 'FINISHED', 'in_transit', 'delivered'),
        ])));
        check('a zero on a mission that was not stopped is a measurement',
            t.join('|').indexOf('unload @ ALN_001 · 0 s') !== -1 && t.join('|').indexOf('not run') === -1, JSON.stringify(t));
    }

    console.log('legs: stopped mission, zero that is not trailing');
    {
        const t = legTexts(await render(legsOnly([
            legRow('1970-01-01T00:20:00Z', [blk('SMN_014', 'Load', 1000, 1000), blk('ALN_001', 'Unload', 1060, 1090)]),
            ev('1970-01-01T00:20:01Z', 'RUNNING', 'STOPPED', 'in_transit', 'cancelled'),
        ])));
        check('a non-trailing zero on a stopped mission stays a measurement',
            t.join('|').indexOf('load @ SMN_014 · 0 s') !== -1 && t.join('|').indexOf('not run') === -1, JSON.stringify(t));
    }

    console.log('legs: unknown keeps its own form');
    {
        const t = legTexts(await render(legsOnly([
            legRow('1970-01-01T00:20:00Z', [blk('SMN_014', 'Load', 1000, 1030), blk('ALN_001', 'Unload', 0, 0)]),
            ev('1970-01-01T00:20:01Z', 'RUNNING', 'STOPPED', 'in_transit', 'cancelled'),
        ])));
        check('an untimed block reads "unknown", not "not run"',
            t.join('|').indexOf('unload @ ALN_001 · unknown') !== -1 && t.join('|').indexOf('not run') === -1, JSON.stringify(t));
    }

    console.log('');
    if (failures > 0) {
        console.log(failures + ' assertion(s) failed');
        process.exit(1);
    }
    console.log('all mission-detail stage assertions passed');
}

main().catch(function(err) { console.error(err); process.exit(1); });
