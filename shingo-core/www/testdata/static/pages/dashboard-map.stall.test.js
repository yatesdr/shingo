// Unit tests for the robot-map dashboard's static route lane and the word on
// motion it reads from Core. Run under plain Node via the Go wrapper
// dashboard_map_stall_test.go. Exit 0 on pass, 1 on any assertion failure.
//
// The lane's history, each step a regression these pin:
//   1. It was drawn for any robot with an order that had not moved for five
//      seconds (the chevron-flicker threshold), so every
//      traffic-point wait, lift call and jack cycle painted a route across the
//      plant.
//   2. It then keyed off a 45 s stall threshold, and a robot stuck in traffic
//      drew a highlighted route that read as something to act on (HOP,
//      2026-11-12).
//   3. Now a robot standing still draws no lane at all, however long. The lane
//      is drawn only where a comet cannot carry the route: a blocked/faulted
//      order, or prefers-reduced-motion. And a freshly loaded fleet standing
//      still draws none.
//
// Whether a robot is moving is Core's robot poll's word (`moving`, engine
// robot_motion.go), pinned there; the page takes it as it comes.

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
// Nothing below tests geometry, but dashboard-map.js builds its projector and
// its comet layer at module scope from the shared scene modules, so the module
// does not EVALUATE without them. vm runs a script, not a module:
// shared-modules.js loads each shared file into the same context and binds the
// page's imports to them. Same technique, spelled out, in
// dashboard-map.curve.test.js.
const SM = require(path.join(SRC_DIR, '..', '..', '..', '..', 'shared', 'testdata', 'js', 'shared-modules.js'));

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

    const file = path.join(SRC_DIR, 'dashboard-map.js');
    const raw = fs.readFileSync(file, 'utf8');
    const INJECT = '  __export({ mergeRobot: mergeRobot, isMoving: isMoving,' +
        ' laneVisible: laneVisible, robots: robots,' +
        ' alarmWords: alarmWords, STATUS_COLOR: STATUS_COLOR, restDisc: restDisc });\n})();\n';
    const src = raw.replace(/\}\)\(\);\s*$/, INJECT);
    if (src === raw) {
        throw new Error('dashboard-map.js no longer ends in the "})();" IIFE close this ' +
            'harness injects before; update the injection in dashboard-map.stall.test.js');
    }
    const shared = SM.realm(ctx);
    // The page's own rows-to-scene adapter (components/scene-rows.js, a Core
    // module the page imports), its scene-geom import bound like the page's.
    ctx.sceneOfRows = SM.moduleOf(ctx, shared, path.join(SRC_DIR, '..', 'components', 'scene-rows.js')).sceneOfRows;
    SM.runPage(ctx, src, shared, null, 'dashboard-map.js');
    if (!exported) throw new Error('__export was not reached inside dashboard-map.js');
    return exported;
}

// --- isMoving and mergeRobot: Core's word -------------------------------

console.log('isMoving');

(function movingIsCoresWord() {
    const m = load();
    const robot = moving => ({ id: 'AMR-01', x: 10, y: 10, angle: 0, state: 'busy', moving: moving });
    check('Core says moving: moving, on the first frame', m.isMoving(robot(true)) === true);
    check('Core says still: not moving', m.isMoving(robot(false)) === false);
    check('no word (a Core predating it): not moving', m.isMoving(robot(undefined)) === false);
})();

console.log('mergeRobot');

(function mergeTakesTheLatestWord() {
    const m = load();
    m.mergeRobot({ id: 'AMR-08', x: 1, y: 1, moving: true });
    check('a robot Core says is driving: moving from its first merge', m.isMoving(m.robots['AMR-08']) === true);
    m.mergeRobot({ id: 'AMR-08', x: 9, y: 9, moving: false });
    check('the next word replaces it: not moving', m.isMoving(m.robots['AMR-08']) === false);
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
    // After a kiosk reload a fleet standing still paints no lane on the first
    // frame.
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

// --- a robot at rest: one disc, one size ---------------------------------

console.log('restDisc');

(function everyRestingRobotIsOneSize() {
    const m = load();
    // HOP, 2026-11-12: AMR-12, paused on a park bay, drew larger than the
    // ready robots parked in the same row.
    const nodeR = 0.5;
    const sizes = ['ready', 'paused', 'offline', 'error'].map(function (state) {
        return m.restDisc({ id: 'AMR-12', x: 3, y: 4, state: state }, nodeR).r;
    });
    sizes.push(m.restDisc({ id: 'AMR-05', x: 3, y: 4, state: 'ready', charging: true }, nodeR).r);
    check('ready, paused, offline, error and charging rest at one size',
        sizes.every(function (r) { return r === sizes[0]; }), JSON.stringify(sizes));
    const off = m.restDisc({ id: 'AMR-12', x: 3, y: 4, state: 'paused' }, nodeR);
    check('off any bay, the disc sits where the robot is', off.x === 3 && off.y === 4, JSON.stringify(off));
})();

console.log(failures === 0 ? 'PASS' : failures + ' FAILURE(S)');
process.exit(failures === 0 ? 0 : 1);
