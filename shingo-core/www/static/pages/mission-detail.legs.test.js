// Unit tests for the mission-detail leg bar's three duration forms. Run under
// plain Node via the Go wrapper mission_detail_legs_test.go. Exit 0 on pass,
// 1 on any assertion failure.
//
// What these exist to prevent: collapsing `no data`, `zero` and `not
// applicable` into two forms. The bar started with two — UNKNOWN (no usable
// endpoints) and a drawn duration — and a block whose startTime equals its
// terminateTime fell into the second, rendering `0s`. That is right for a block
// that ran and finished inside the vendor's one-second resolution, and wrong
// for a trailing block on a mission the fleet STOPPED, where the equal stamps
// are the teardown writing down blocks it never executed. `0s` there asserts
// that a leg which never happened happened instantly.
//
// The headline assertions are the two directions of that: a trailing zero on a
// STOPPED mission reads `not run` and claims no timeline, and a zero anywhere
// else still reads `0s` and is still drawn.

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
// mission, and nothing else. Returns the bar/legend HTML the leg renderer
// produced.
function render(events, telemetry) {
    const els = {};
    function getEl(id) {
        if (!els[id]) {
            els[id] = { id: id, textContent: '', innerHTML: '', style: {} };
        }
        return els[id];
    }
    getEl('mission-order-id').textContent = '4242';

    const payload = { order: { id: 4242 }, telemetry: telemetry, events: events };

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
        // Injected in place of the stripped ES imports.
        onSSE: function() {},
        debounce: function(fn) { return fn; },
        api: {},
        el: function() {},
        h: function() {},
    };
    ctxObj.window = ctxObj;
    vm.createContext(ctxObj);

    const src = fs.readFileSync(path.join(__dirname, 'mission-detail.js'), 'utf8')
        .replace(/^import[^;]+;\s*/gm, '');   // drop the ES imports; deps injected above
    vm.runInContext(src, ctxObj);

    // loadMission's fetch chain settles asynchronously; drain the queue before
    // reading. A miss here is reported as the page's own failure text rather
    // than a TypeError, so a broken harness cannot read as a broken renderer.
    return new Promise(function(resolve) { setImmediate(resolve); }).then(function() {
        if (!els['duration-bar']) {
            throw new Error('renderDurationBar never ran; page said: '
                + (els['mission-loading'] ? els['mission-loading'].textContent : '(nothing)'));
        }
        return { bar: els['duration-bar'].innerHTML, legend: els['duration-legend'].innerHTML };
    });
}

// blockEvent wraps block records the way the mission API SERVES them: one leg
// row, flagged is_leg, carrying the fleet's block array in blocks_json.
//
// is_leg is what the page keys on now. new_state is kept here because the API
// still sends it verbatim — the raw fleet word stays available for diagnosis —
// but the page no longer compares against it, so a fixture carrying only
// new_state would be modelling a payload the server does not produce.
function blockEvent(blocks) {
    return {
        new_state: 'BLOCK_FINISHED',
        is_leg: true,
        blocks_json: JSON.stringify(blocks),
    };
}

function blk(location, binTask, startTime, terminateTime) {
    return { location: location, binTask: binTask, startTime: startTime, terminateTime: terminateTime };
}

// --- cases ---------------------------------------------------------------

async function main() {
    // A mission the fleet STOPPED, whose last two blocks were stamped at
    // teardown rather than executed. This is the shape Springfield's RDS showed:
    // trailing blocks on a STOPPED order carry startTime == terminateTime.
    console.log('stopped mission, trailing zero-duration blocks');
    {
        const out = await render([
            blockEvent([
                blk('SMN_014', 'Load', 1000, 1030),   // ran, 30s
                blk('ALN_001', 'Unload', 1090, 1090), // teardown stamp
                blk('SMN_003', 'Wait', 1090, 1090),   // teardown stamp
            ]),
            { new_state: 'STOPPED' },
        ], { duration_ms: 200000 });

        // Matched on the rendered LEG text, not on a bare substring: 'not run'
        // also appears in the footer tally and '0s' is a substring of '20s'.
        check('trailing teardown blocks read "not run"',
            (out.legend.match(/>not run</g) || []).length === 2,
            'legend: ' + out.legend);
        check('they do NOT report a duration',
            out.legend.indexOf('ALN_001: 0s') === -1 && out.legend.indexOf('SMN_003: 0s') === -1,
            'a leg that never happened must not report a measurement; legend: ' + out.legend);
        check('they carry the not-run form, not the unknown form',
            (out.bar.match(/leg-notrun/g) || []).length === 2 && out.bar.indexOf('leg-unknown') === -1,
            'bar: ' + out.bar);
        check('they claim no share of the timeline',
            (out.bar.match(/flex:0 0 52px/g) || []).length === 2,
            'bar: ' + out.bar);
        check('the footer counts them as not run, separately from unknown',
            out.legend.indexOf('2 leg(s) not run') !== -1 && out.legend.indexOf('leg(s) unknown') === -1,
            'legend: ' + out.legend);
        check('no travel leg is drawn INTO a block that never ran',
            (out.legend.match(/travel/g) || []).length === 0,
            'the robot did not drive to a leg it did not perform; legend: ' + out.legend);
        check('the block that DID run still reports its duration',
            out.legend.indexOf('load @ SMN_014: 30s') !== -1,
            'legend: ' + out.legend);
    }

    // The other direction. A genuine sub-second block is a MEASUREMENT and must
    // survive as one — demoting it to "not run" is the same error pointing the
    // other way.
    console.log('finished mission, zero-duration block');
    {
        const out = await render([
            blockEvent([
                blk('SMN_014', 'Load', 1000, 1030),
                blk('ALN_001', 'Unload', 1060, 1060), // real, sub-second
            ]),
            { new_state: 'FINISHED' },
        ], { duration_ms: 200000 });

        check('a zero on a non-stopped mission still reads "0s"',
            out.legend.indexOf('unload @ ALN_001: 0s') !== -1,
            'legend: ' + out.legend);
        check('and is still drawn rather than treated as absent',
            out.bar.indexOf('leg-notrun') === -1 && out.bar.indexOf('leg-unknown') === -1,
            'bar: ' + out.bar);
        check('nothing is counted as not run',
            out.legend.indexOf('not run') === -1,
            'legend: ' + out.legend);
    }

    // Scoping: the teardown can only affect what had not run yet, so a zero
    // BEFORE a block with real duration is a real reading and keeps its form
    // even on a stopped mission.
    console.log('stopped mission, zero-duration block that is not trailing');
    {
        const out = await render([
            blockEvent([
                blk('SMN_014', 'Load', 1000, 1000),   // real sub-second, mid-run
                blk('ALN_001', 'Unload', 1060, 1090), // ran after it, so the stop came later
            ]),
            { new_state: 'STOPPED' },
        ], { duration_ms: 200000 });

        check('a non-trailing zero on a stopped mission stays a measurement',
            out.legend.indexOf('load @ SMN_014: 0s') !== -1 && out.legend.indexOf('not run') === -1,
            'legend: ' + out.legend);
    }

    // UNKNOWN keeps its own form. A block with no endpoints reported is not
    // evidence about what came after it, so it breaks the trailing run rather
    // than extending it — inferring "not run" from missing data is exactly the
    // conflation these three forms exist to prevent.
    console.log('unknown blocks keep the unknown form');
    {
        const out = await render([
            blockEvent([
                blk('SMN_014', 'Load', 1000, 1030),
                blk('ALN_001', 'Unload', 0, 0),       // never timed
            ]),
            { new_state: 'STOPPED' },
        ], { duration_ms: 200000 });

        check('an untimed block reads "unknown", not "not run"',
            out.legend.indexOf('unknown') !== -1 && out.legend.indexOf('not run') === -1,
            'legend: ' + out.legend);
        check('and carries the hatched unknown form',
            out.bar.indexOf('leg-unknown') !== -1 && out.bar.indexOf('leg-notrun') === -1,
            'bar: ' + out.bar);
        check('the footer counts it as unknown',
            out.legend.indexOf('1 leg(s) unknown') !== -1,
            'legend: ' + out.legend);
    }

    console.log('');
    if (failures > 0) {
        console.log(failures + ' assertion(s) failed');
        process.exit(1);
    }
    console.log('all leg-form assertions passed');
}

main().catch(function(err) { console.error(err); process.exit(1); });
