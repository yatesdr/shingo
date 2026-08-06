// Unit tests for the robot-map dashboard's STALL threshold — the rule behind
// the frozen route lane. Run under plain Node via the Go wrapper
// dashboard_map_stall_test.go. Exit 0 on pass, 1 on any assertion failure.
//
// The bug these exist to prevent: the lane predicate was
//   showLane = blocked || reduceMotion || (!isMoving(r) && calmOK)
// and isMoving means "displaced within MOVE_LINGER_MS" — five seconds. Five
// seconds is the CHEVRON-FLICKER threshold: it exists so a robot pivoting at a
// corner doesn't blink from chevron to disc and back. Reusing it as a STALL
// threshold makes the map assert that a robot is stuck every time it holds
// still for five seconds, which on a real floor means every traffic-point
// wait, every lift call, every jack cycle, and every order that has been
// assigned but not yet started. Each one painted a full route line across the
// plant, so the one drawing that should have meant "go look at this robot"
// meant nothing at all.
//
// Two properties carry the fix:
//   1. The lane keys off STALL_MS, which is its own value in the 30–60s band
//      and strictly longer than MOVE_LINGER_MS. Asserted as a RANGE, not a
//      point, so retuning it inside the band cannot hollow these tests out.
//   2. A robot with no observation history is NOT stalled. lastMoveAt starts at
//      0, and Date.now() - 0 is decades, so a stall test written against
//      lastMoveAt alone reports the entire fleet as stalled on the first frame
//      after a kiosk reload. Absence of data must not render as a finding.

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
// dashboard-map.js is an IIFE, so its internals are not reachable the way
// loaders.js's top-level functions are. Rather than strip the wrapper (which
// would change scoping rules for every `var` in the file), inject one call to
// a harness-supplied __export just before the IIFE closes. If the module's
// shape changes so the injection no longer applies, load() throws with a
// message naming what to fix rather than failing an assertion obscurely.
//
// Nothing below tests geometry, but dashboard-map.js configures its projector
// at module scope from components/scene-geom.js, so the module does not
// EVALUATE without it. vm runs a script, not a module: scene-geom's exports are
// stripped to plain declarations and evaluated into the same context, where the
// stripped imports resolve to them. Same technique, spelled out, in
// dashboard-map.curve.test.js.
function loadSceneGeom(ctx) {
    const file = path.join(__dirname, '..', 'components', 'scene-geom.js');
    const raw = fs.readFileSync(file, 'utf8');
    const src = raw.replace(/^export /mg, '');
    if (src === raw) {
        throw new Error('components/scene-geom.js no longer declares its exports as ' +
            '"export function"/"export var" at line start, which this harness strips to ' +
            'run it as a script; update loadSceneGeom in dashboard-map.stall.test.js');
    }
    vm.runInContext(src, ctx);
}

function load() {
    const ctx = {
        console: console,
        Date: Date,
        Math: Math,
        Object: Object,
        Array: Array,
        JSON: JSON,
        isFinite: isFinite,
        document: {
            // Deliberately 'loading': the module tail calls init() otherwise,
            // which fetches over the network.
            readyState: 'loading',
            body: { getAttribute() { return '1'; } },
            documentElement: {},
            getElementById() { return null; },
            querySelector() { return null; },
            querySelectorAll() { return []; },
            createElementNS() { return { setAttribute() {}, appendChild() {}, style: {} }; },
            addEventListener() {},
        },
        window: { addEventListener() {}, matchMedia() { return { matches: false }; } },
        setInterval() { return 0; },
        clearInterval() {},
        setTimeout() { return 0; },
        clearTimeout() {},
        requestAnimationFrame() { return 0; },
        cancelAnimationFrame() {},
        fetch() { return Promise.resolve({ ok: true, json() { return Promise.resolve([]); } }); },
        // Injected in place of the stripped ES import.
        onSSE() {},
        setSSEReloadOnBuild() {},
    };
    let exported = null;
    ctx.__export = function (o) { exported = o; };
    vm.createContext(ctx);
    loadSceneGeom(ctx);

    const file = path.join(__dirname, 'dashboard-map.js');
    const raw = fs.readFileSync(file, 'utf8');
    // /g, not one shot: dashboard-map.js imports from two modules now, and a
    // non-global strip leaves the second `import` in a script vm cannot parse.
    const stripped = raw.replace(/^import[^;]+;\s*/mg, '');
    const INJECT = '  __export({ mergeRobot: mergeRobot, isMoving: isMoving, isStalled: isStalled,' +
        ' laneVisible: laneVisible, robots: robots,' +
        ' MOVE_LINGER_MS: MOVE_LINGER_MS, STALL_MS: STALL_MS });\n})();\n';
    const src = stripped.replace(/\}\)\(\);\s*$/, INJECT);
    if (src === stripped) {
        throw new Error('dashboard-map.js no longer ends in the "})();" IIFE close this ' +
            'harness injects before; update the injection in dashboard-map.stall.test.js');
    }
    vm.runInContext(src, ctx);
    if (!exported) throw new Error('__export was not reached inside dashboard-map.js');
    return exported;
}

// A robot as the module holds it, with its motion history stated outright.
// msSinceMove null means "never observed moving".
function robotAged(msSinceMove, msSinceFirstSeen) {
    const now = Date.now();
    return {
        id: 'AMR-01', x: 10, y: 10, angle: 0, state: 'busy',
        lastMoveAt: msSinceMove == null ? 0 : now - msSinceMove,
        firstSeenAt: now - msSinceFirstSeen,
    };
}

// --- the threshold itself ------------------------------------------------

console.log('STALL_MS');

(function stallThresholdIsItsOwnValue() {
    const m = load();
    check('STALL_MS is not MOVE_LINGER_MS',
        m.STALL_MS !== m.MOVE_LINGER_MS,
        'both are ' + m.STALL_MS);
    check('STALL_MS is strictly longer than the chevron-flicker linger',
        m.STALL_MS > m.MOVE_LINGER_MS,
        'STALL_MS=' + m.STALL_MS + ' MOVE_LINGER_MS=' + m.MOVE_LINGER_MS);
    // A range, not a point: retuning inside the band must not silently make
    // any assertion below vacuous.
    check('STALL_MS sits in the 30–60s band',
        m.STALL_MS >= 30000 && m.STALL_MS <= 60000,
        'STALL_MS=' + m.STALL_MS);
})();

// --- isStalled -----------------------------------------------------------

console.log('isStalled');

(function pauseBelowStallIsNotAStall() {
    const m = load();
    // The headline case. Sample every pause length between the two thresholds
    // rather than one point, so the property holds across the whole gap: a
    // robot that has stopped but not stalled is neither moving nor stalled.
    const gap = m.STALL_MS - m.MOVE_LINGER_MS;
    [0.05, 0.25, 0.5, 0.75, 0.95].forEach(function (f) {
        const paused = m.MOVE_LINGER_MS + Math.round(gap * f);
        const r = robotAged(paused, paused + 60000);
        check('paused ' + paused + 'ms: not moving', m.isMoving(r) === false);
        check('paused ' + paused + 'ms: not stalled', m.isStalled(r) === false);
    });
})();

(function movingIsNeverStalled() {
    const m = load();
    const r = robotAged(0, 600000);
    check('just displaced: moving', m.isMoving(r) === true);
    check('just displaced: not stalled', m.isStalled(r) === false);
})();

(function stoppedPastThresholdIsStalled() {
    const m = load();
    const r = robotAged(m.STALL_MS + 1000, m.STALL_MS + 61000);
    check('stopped past STALL_MS: stalled', m.isStalled(r) === true);
    check('stopped past STALL_MS: not moving', m.isMoving(r) === false);
})();

(function neverObservedIsNotStalled() {
    const m = load();
    // Kiosk reload / SSE reconnect: the robot has just appeared and we have
    // watched it for 200ms. lastMoveAt is 0. Unknown is not a finding.
    const r = robotAged(null, 200);
    check('never observed moving, just seen: not stalled', m.isStalled(r) === false);
    // Once it HAS been watched that long without moving, it is a stall.
    const r2 = robotAged(null, m.STALL_MS + 1000);
    check('never observed moving, watched past STALL_MS: stalled', m.isStalled(r2) === true);
    // No history at all (neither field) is unknown, not stalled.
    check('no timestamps at all: not stalled',
        m.isStalled({ id: 'X', lastMoveAt: 0, firstSeenAt: 0 }) === false);
})();

// --- mergeRobot stamps the observation clock -----------------------------

console.log('mergeRobot');

(function mergeStampsFirstSeen() {
    const m = load();
    const before = Date.now();
    m.mergeRobot({ id: 'AMR-07', x: 1, y: 1 });
    const held = m.robots['AMR-07'];
    check('first merge stamps firstSeenAt', held.firstSeenAt >= before && held.firstSeenAt <= Date.now(),
        'firstSeenAt=' + held.firstSeenAt);
    check('first merge leaves lastMoveAt unset', held.lastMoveAt === 0,
        'lastMoveAt=' + held.lastMoveAt);
    check('a robot seen for the first time is not stalled', m.isStalled(held) === false);
})();

(function mergePreservesFirstSeenAcrossTicks() {
    const m = load();
    m.mergeRobot({ id: 'AMR-08', x: 1, y: 1 });
    const stamped = m.robots['AMR-08'].firstSeenAt;
    // Rewind the observation clock to simulate a long watch, then tick again
    // with the robot in the same place.
    m.robots['AMR-08'].firstSeenAt = stamped - (m.STALL_MS + 5000);
    m.mergeRobot({ id: 'AMR-08', x: 1, y: 1 });
    check('firstSeenAt survives a merge',
        m.robots['AMR-08'].firstSeenAt === stamped - (m.STALL_MS + 5000),
        'got ' + m.robots['AMR-08'].firstSeenAt);
    check('watched that long without moving: stalled',
        m.isStalled(m.robots['AMR-08']) === true);
    // A displacement clears it.
    m.mergeRobot({ id: 'AMR-08', x: 9, y: 9 });
    check('a displacement clears the stall', m.isStalled(m.robots['AMR-08']) === false);
    check('a displacement reads as moving', m.isMoving(m.robots['AMR-08']) === true);
})();

// --- laneVisible: the call site, not just the predicate ------------------

console.log('laneVisible');

(function pausedRobotDrawsNoLane() {
    const m = load();
    // Ten seconds still, calm floor, ordinary working order. This is the whole
    // bug: under the old rule this painted a lane the length of the plant.
    const r = robotAged(10000, 300000);
    check('paused 10s on a calm floor: no lane',
        m.laneVisible(r, false, false, true) === false);
})();

(function stalledRobotDrawsALane() {
    const m = load();
    const r = robotAged(m.STALL_MS + 1000, m.STALL_MS + 61000);
    check('stalled on a calm floor: lane', m.laneVisible(r, false, false, true) === true);
    check('stalled on a BUSY floor: no lane (calm gate still applies)',
        m.laneVisible(r, false, false, false) === false);
})();

(function blockedAndReducedMotionAreIndependentOfMotion() {
    const m = load();
    const moving = robotAged(0, 300000);
    check('blocked while moving: lane', m.laneVisible(moving, true, false, true) === true);
    check('blocked on a busy floor: lane', m.laneVisible(moving, true, false, false) === true);
    check('reduced motion while moving: lane', m.laneVisible(moving, false, true, true) === true);
    check('reduced motion on a busy floor: lane', m.laneVisible(moving, false, true, false) === true);
})();

(function freshlyLoadedFleetDrawsNoLanes() {
    const m = load();
    // The regression that firstSeenAt exists for: after a kiosk reload every
    // robot has lastMoveAt 0. Without an observation clock every one of them
    // would paint a lane on the first frame.
    m.mergeRobot({ id: 'AMR-11', x: 1, y: 1 });
    m.mergeRobot({ id: 'AMR-12', x: 5, y: 5 });
    const lanes = Object.keys(m.robots).filter(function (k) {
        return m.laneVisible(m.robots[k], false, false, true);
    });
    check('no lanes on the first frame after a reload', lanes.length === 0,
        'lanes for ' + JSON.stringify(lanes));
})();

console.log(failures === 0 ? 'PASS' : failures + ' FAILURE(S)');
process.exit(failures === 0 ? 0 : 1);
