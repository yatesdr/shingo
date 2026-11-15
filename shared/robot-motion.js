// robot-motion.js — when a robot on a scene map counts as moving, and the two
// thresholds its comets read: the busy-floor calm and the pickup radius.
//
// SHARED, under docs/shared-layer-promotion.md. Core's robot map
// (pages/dashboard-map.js) and the Edge's robot panel
// (operator-station/operator-robot-map.js) draw the same robots on the same
// machinery; "moving" is what runs a comet and turns a disc into a chevron
// (motion means motion), so two answers would show one robot driving on one
// screen and parked on the other. Moved from dashboard-map.js, 2026-11-14,
// with its values (owner: one machinery in shared/; lead ruling on U10, 4).
// Pinned by robot-motion.test.js (TestRobotMotionSaysWhenARobotMoves) and, on
// Core's page, the stall pins (isMoving, mergeRobot) and the calm pins
// (twelveCometsThenNone, theCalmCountsMovingOrdersPathOrNot).
//
// The leg of a comet (fetch hollow, carry solid) stays each page's own
// orderLeg: Core reads the board's `carrying`, the panel cannot.
//
// No clock and no state here: each page passes its own `now` and keeps its own
// robots. The fleet's Busy flag is not read: it stays false for a direct or
// manual move, so a driving robot would draw as idle on flags alone.

// MOVE_LINGER_MS: a robot is moving while its position moved this recently.
// It bridges brief stops (turns, order-cancel pauses) without flickering from
// chevron to disc.
export var MOVE_LINGER_MS = 5000;

// COMET_LIMIT: above this many moving orders the ambient view calms (no comets
// and no intent lines, just the robots), so a busy floor is not a mass of
// arrows. Counted per order moving with its robot seen moving, whether or not
// its path can be drawn.
export var COMET_LIMIT = 12;

// PICKUP_RADIUS: in median scene rows (the scene's scale); a robot this near
// its order's source is at it, which latches the carry leg.
export var PICKUP_RADIUS = 0.9;

// movedFrom reports whether a robot now at `next` has moved from `prev`: by
// more than 2% of a median row (`scale`), never less than 5 cm, and 5 cm with
// no scene to scale by. Below that a new position is the fleet's jitter.
export function movedFrom(prev, next, scale) {
    var eps = scale > 0 ? Math.max(scale * 0.02, 0.05) : 0.05;
    var dx = next.x - prev.x, dy = next.y - prev.y;
    return dx * dx + dy * dy > eps * eps;
}

// movingAt reports whether a robot last seen moving at `lastMoveAt` (ms; 0 or
// none for never) is moving at `now`.
export function movingAt(lastMoveAt, now) {
    return (now - (lastMoveAt || 0)) < MOVE_LINGER_MS;
}
