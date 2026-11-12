// Unit tests for the robot-map dashboard's static route lane and the motion
// clock under it. Run under plain Node via the Go wrapper
// dashboard_map_stall_test.go. Exit 0 on pass, 1 on any assertion failure.
//
// The lane's history, each step a regression these pin:
//   1. It was drawn for any robot with an order that had not moved for
//      MOVE_LINGER_MS (five seconds, the chevron-flicker threshold), so every
//      traffic-point wait, lift call and jack cycle painted a route across the
//      plant.
//   2. It then keyed off a 45 s stall threshold, and a robot stuck in traffic
//      drew a highlighted route that read as something to act on (HOP,
//      2026-11-12).
//   3. Now a robot standing still draws no lane at all, however long. The lane
//      is drawn only where a comet cannot carry the route: a blocked/faulted
//      order, or prefers-reduced-motion. And a freshly loaded fleet, every robot
//      with lastMoveAt 0, draws none.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

// The page code under test. This file lives in www/testdata, outside the
// embedded static tree, so no binary carries or serves it
// (TestStaticTree_HoldsNoTestJS); the code it reads stays in www/static.
const SRC_DIR = path.join(__dirname, '..', '..', '..', 'static', 'pages');

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
// at module scope from shared/scene-geom.js, so the module does not
// EVALUATE without it. vm runs a script, not a module: scene-geom's exports are
// stripped to plain declarations and evaluated into the same context, where the
// stripped imports resolve to them. Same technique, spelled out, in
// dashboard-map.curve.test.js.
function loadSceneGeom(ctx) {
    const file = path.join(SRC_DIR, '..', '..', '..', '..', 'shared', 'scene-geom.js');
    const raw = fs.readFileSync(file, 'utf8');
    const src = raw.replace(/^export /mg, '');
    if (src === raw) {
        throw new Error('shared/scene-geom.js no longer declares its exports as ' +
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

    const file = path.join(SRC_DIR, 'dashboard-map.js');
    const raw = fs.readFileSync(file, 'utf8');
    // /g, not one shot: dashboard-map.js imports from two modules now, and a
    // non-global strip leaves the second `import` in a script vm cannot parse.
    const stripped = raw.replace(/^import[^;]+;\s*/mg, '');
    const INJECT = '  __export({ mergeRobot: mergeRobot, isMoving: isMoving,' +
        ' laneVisible: laneVisible, robots: robots, MOVE_LINGER_MS: MOVE_LINGER_MS,' +
        ' alarmWords: alarmWords, STATUS_COLOR: STATUS_COLOR });\n})();\n';
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
function robotAged(msSinceMove) {
    const now = Date.now();
    return {
        id: 'AMR-01', x: 10, y: 10, angle: 0, state: 'busy',
        lastMoveAt: msSinceMove == null ? 0 : now - msSinceMove,
    };
}

// --- isMoving and mergeRobot: the motion clock ----------------------------

console.log('isMoving');

(function movingWithinTheLinger() {
    const m = load();
    check('just displaced: moving', m.isMoving(robotAged(0)) === true);
    check('still past the linger: not moving', m.isMoving(robotAged(m.MOVE_LINGER_MS + 1000)) === false);
    check('never observed moving: not moving', m.isMoving(robotAged(null)) === false);
})();

console.log('mergeRobot');

(function mergeStampsMotion() {
    const m = load();
    m.mergeRobot({ id: 'AMR-08', x: 1, y: 1 });
    check('first merge leaves lastMoveAt unset', m.robots['AMR-08'].lastMoveAt === 0,
        'lastMoveAt=' + m.robots['AMR-08'].lastMoveAt);
    m.mergeRobot({ id: 'AMR-08', x: 1, y: 1 });
    check('the same place again: not moving', m.isMoving(m.robots['AMR-08']) === false);
    m.mergeRobot({ id: 'AMR-08', x: 9, y: 9 });
    check('a displacement reads as moving', m.isMoving(m.robots['AMR-08']) === true);
})();

// --- laneVisible: where a static lane is drawn -----------------------------

console.log('laneVisible');

(function standingStillDrawsNoLane() {
    const m = load();
    // The HOP case: a robot with an order, still for ten minutes. It used to
    // draw a highlighted route once it passed 45 s; it draws none now.
    check('working order, not blocked, no reduced motion: no lane',
        m.laneVisible(false, false) === false);
})();

(function blockedAndReducedMotionDrawALane() {
    const m = load();
    check('blocked/faulted: lane', m.laneVisible(true, false) === true);
    check('reduced motion: lane', m.laneVisible(false, true) === true);
    check('both: lane', m.laneVisible(true, true) === true);
})();

(function freshlyLoadedFleetDrawsNoLanes() {
    const m = load();
    // After a kiosk reload every robot has lastMoveAt 0; none may paint a lane
    // on the first frame.
    m.mergeRobot({ id: 'AMR-11', x: 1, y: 1 });
    m.mergeRobot({ id: 'AMR-12', x: 5, y: 5 });
    const lanes = Object.keys(m.robots).filter(function () { return m.laneVisible(false, false); });
    check('no lanes on the first frame after a reload', lanes.length === 0,
        'lanes for ' + JSON.stringify(lanes));
})();

// --- the fault alarm in a few words, and faulted in red ------------------

console.log('alarmWords');

(function alarmWordsReadAtAGlance() {
    const m = load();
    check('a code in the table: its words',
        m.alarmWords({ code: 54231, desc: 'Caution: robot is blocked' }) === 'obstacle ahead');
    check('the table fills a format argument',
        m.alarmWords({ code: 52700, desc: '["Do not find path to {1}","CP68"]' }) === 'no path to CP68');
    check('a code not in the table: the fleet’s own text',
        m.alarmWords({ code: 59999, desc: 'diThread not running' }) === 'diThread not running');
    const long = m.alarmWords({ code: 59998, desc: 'laser frequency : 0.000 is inconsistent with default frequency : 30.000, laser id : 0' });
    check('long fleet text is shortened at a word', long.length <= 40 && long.endsWith('…'), JSON.stringify(long));
    check('a format string not in the table is filled',
        m.alarmWords({ code: 59997, desc: '["Accuracy error is too large(xy): {1}",0.3093002995165057]' }) === 'Accuracy error is too large(xy): 0.31');
    check('no alarm: no words', m.alarmWords(null) === '');
    check('faulted is drawn in blocked’s red', m.STATUS_COLOR.faulted === m.STATUS_COLOR.blocked);
})();

console.log(failures === 0 ? 'PASS' : failures + ' FAILURE(S)');
process.exit(failures === 0 ? 0 : 1);
