// robot-motion.js — the two thresholds a scene map's comets read while robots
// move: the busy-floor calm and the pickup radius.
//
// SHARED, under docs/shared-layer-promotion.md. Core's robot map
// (pages/dashboard-map.js) and the Edge's robot panel
// (operator-station/operator-robot-map.js) draw the same robots on the same
// machinery, so two numbers here would show one floor two ways. Moved from
// dashboard-map.js, 2026-11-14, with its values (owner: one machinery in
// shared/; lead ruling on U10, 4). Pinned on the pages that read them: Core's
// calm pins (twelveCometsThenNone, theCalmCountsMovingOrdersPathOrNot) and the
// panel's "twelve comets, thirteen calm", and the legs both pages draw.
//
// WHETHER A ROBOT IS MOVING IS NOT DECIDED HERE. Core's robot poll decides it
// from every sample (shingo-core/engine/robot_motion.go) and both pages read
// its word: `moving` on Core's robot-update frame and /api/robots, and on each
// position of the panel's robot-pos stream. A page that judged it from the
// frames it received drew every robot parked until its second frame.
//
// The leg of a comet (fetch hollow, carry solid) stays each page's own
// orderLeg: Core reads the board's `carrying`, the panel cannot.

// COMET_LIMIT: above this many moving orders the ambient view calms (no comets
// and no intent lines, just the robots), so a busy floor is not a mass of
// arrows. Counted per order moving with its robot moving, whether or not its
// path can be drawn.
export var COMET_LIMIT = 12;

// PICKUP_RADIUS: in median scene rows (the scene's scale); a robot this near
// its order's source is at it, which latches the carry leg.
export var PICKUP_RADIUS = 0.9;
