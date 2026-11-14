// robot-motion.test.js — shared/robot-motion.js: when a robot on a scene map
// counts as moving. Runs under plain Node via TestRobotMotionSaysWhenARobotMoves
// (scene_geom_test.go). Exit 0 = pass, 1 = any failure.
//
// The two numbers the comets read besides (COMET_LIMIT, PICKUP_RADIUS) are
// held by what they do, on the pages that read them: Core's
// twelveCometsThenNone and the panel's "twelve comets, thirteen calm", and the
// legs both pages draw.

'use strict';

const { realm } = require('./shared-modules.js');
const { movedFrom, movingAt, MOVE_LINGER_MS } = realm()('robot-motion.js');

let failures = 0;
function check(name, cond, detail) {
    if (cond) { console.log('  ok  ' + name); return; }
    failures++;
    console.log('  FAIL ' + name + (detail ? ' — ' + detail : ''));
}

// movingAt: the linger after the last move, on the page's own clock.
const now = Date.UTC(2026, 10, 14, 12, 0, 0);
check('moved just now: moving', movingAt(now, now) === true);
check('moved within the linger: moving', movingAt(now - (MOVE_LINGER_MS - 1), now) === true);
check('the linger run out: not moving', movingAt(now - MOVE_LINGER_MS, now) === false);
check('never seen moving (0): not moving', movingAt(0, now) === false);
check('never seen moving (absent): not moving', movingAt(undefined, now) === false);

// movedFrom: a jitter threshold of 2% of a median row, never under 5 cm.
const at = (x, y) => ({ x: x, y: y });
check('10 m rows: 19 cm is jitter', movedFrom(at(0, 0), at(0.19, 0), 10) === false);
check('10 m rows: 21 cm is a move', movedFrom(at(0, 0), at(0.21, 0), 10) === true);
check('the move is a distance, both axes counted', movedFrom(at(0, 0), at(0.15, 0.15), 10) === true);
check('1 m rows: the 5 cm floor holds (4 cm is jitter)', movedFrom(at(0, 0), at(0.04, 0), 1) === false);
check('1 m rows: 6 cm is a move', movedFrom(at(0, 0), at(0, 0.06), 1) === true);
check('no scene to scale by: 5 cm (4 cm jitter, 6 cm a move)',
    movedFrom(at(0, 0), at(0.04, 0), 0) === false && movedFrom(at(0, 0), at(0.06, 0), 0) === true);
check('standing still is not a move', movedFrom(at(3, 4), at(3, 4), 10) === false);

console.log(failures ? failures + ' failed' : 'robot motion: all passed');
process.exit(failures ? 1 : 0);
