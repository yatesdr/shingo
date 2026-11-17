package engine

import (
	"math"
	"sort"
	"strings"
	"time"

	"shingocore/fleet"
	"shingocore/store/scene"
)

// ── Whether a robot is moving ────────────────────────────────────────
//
// A robot is moving while its position moved within the last robotMoveLinger,
// by more than the fleet's jitter between two polls: 2% of the scene's median
// row, never less than 5 cm. The linger bridges the brief stops of a drive
// (turns, traffic points, an order-cancel pause), so a robot does not flicker
// between parked and driving. The fleet's Busy flag is not read: it stays
// false for a direct or manual move, so a driving robot would read as idle.
//
// DECIDED ON THE POLL BECAUSE ONLY THE POLL SEES EVERY SAMPLE. A page that
// judged motion from the frames it received could not call a robot moving
// before its second frame: a robot panel just opened, a tab shown again or a
// kiosk reloaded drew every robot parked for one poll. Core polls every robot
// every protocol.RobotPollInterval whether anyone looks or not, so the first
// frame any reader gets already says (fleet.RobotStatus.Moving, and
// protocol.RobotPosition.Moving for an Edge's robot panel).
//
// At Hopkinsville, 2026-11-17: median row 2.26 m (so the 5 cm floor rules),
// twelve parked robots moved 0.000 m between polls, a driving one 0.36-0.76 m.

const (
	// robotMoveLinger is how long a robot stays moving after its last move.
	robotMoveLinger = 5 * time.Second
	// robotMoveFloor is the smallest move that is not jitter, metres.
	robotMoveFloor = 0.05
	// robotMoveShare is the move that is not jitter, as a share of the
	// scene's median row.
	robotMoveShare = 0.02
)

// robotMotion is each robot's last polled place and when it last moved.
//
// Single-writer: touched only by markRobotMotion, from robotRefreshLoop's
// goroutine. A restart forgets it: the first poll after one sees every robot
// for the first time, and so none moving, until the next.
type robotMotion struct {
	last map[string]motionMark
}

type motionMark struct {
	x, y    float64
	movedAt time.Time
}

// markRobotMotion writes Moving on each robot of one poll taken at now. It runs
// before anything reads the poll, beside resolveRobotPaths.
func (e *Engine) markRobotMotion(robots []fleet.RobotStatus, now time.Time) {
	e.motion.mark(robots, now, math.Float64frombits(e.rowScale.Load()))
}

// mark judges one poll. scale is the scene's median row length, 0 with no
// scene. A robot missing from the poll is forgotten, so its return is a first
// sight; a first sight is not a move.
func (m *robotMotion) mark(robots []fleet.RobotStatus, now time.Time, scale float64) {
	eps := max(scale*robotMoveShare, robotMoveFloor)
	seen := make(map[string]motionMark, len(robots))
	for i := range robots {
		r := &robots[i]
		if r.VehicleID == "" {
			continue
		}
		mk, had := m.last[r.VehicleID]
		if had {
			if dx, dy := r.X-mk.x, r.Y-mk.y; dx*dx+dy*dy > eps*eps {
				mk.movedAt = now
			}
		}
		mk.x, mk.y = r.X, r.Y
		r.Moving = !mk.movedAt.IsZero() && now.Sub(mk.movedAt) < robotMoveLinger
		seen[r.VehicleID] = mk
	}
	m.last = seen
}

// medianRowLength is the median length of the scene's rows, metres, 0 with
// none: the scale shared/scene-net.js gives Core's robot map from the same
// rows (components/scene-rows.js over /api/map/edges), counted the same way,
// so the jitter is judged against the map the robots are drawn on. Pinned
// against the JS by TestMedianRowLength_IsTheMapsScale.
//
// A row end is a point named case-blind, placed where the first row naming it
// puts it; an end with no name is a point by its place. A row from a point to
// itself and a row of no length are not counted; every other row is, both
// directions of a lane included. A curved row's length is its cubic's, along
// sceneCubicSamples chords as shared/scene-geom.js cubicLength measures it.
func medianRowLength(edges []*scene.Edge) float64 {
	type key struct {
		name string
		x, y float64
	}
	type place struct{ x, y float64 }
	places := map[key]place{}
	end := func(name string, x, y float64) (key, place) {
		k := key{name: strings.ToLower(name)}
		if name == "" {
			k = key{x: x, y: y}
		}
		p, ok := places[k]
		if !ok {
			p = place{x, y}
			places[k] = p
		}
		return k, p
	}
	var lens []float64
	for _, e := range edges {
		if e == nil {
			continue
		}
		fk, from := end(e.FromName, e.FromX, e.FromY)
		tk, to := end(e.ToName, e.ToX, e.ToY)
		if fk == tk {
			continue
		}
		var n float64
		if e.Curved() {
			n = cubicLength(from.x, from.y, *e.Ctrl1X, *e.Ctrl1Y, *e.Ctrl2X, *e.Ctrl2Y, to.x, to.y)
		} else {
			dx, dy := to.x-from.x, to.y-from.y
			n = math.Sqrt(dx*dx + dy*dy)
		}
		if n > 0 {
			lens = append(lens, n)
		}
	}
	if len(lens) == 0 {
		return 0
	}
	sort.Float64s(lens)
	return lens[len(lens)/2]
}

// sceneCubicSamples is shared/scene-geom.js CUBIC_SAMPLES.
const sceneCubicSamples = 24

// cubicLength is the length of the cubic Bezier from (x0, y0) through the
// handles to (x3, y3), along sceneCubicSamples chords.
func cubicLength(x0, y0, c1x, c1y, c2x, c2y, x3, y3 float64) float64 {
	total, px, py := 0.0, x0, y0
	for i := 1; i <= sceneCubicSamples; i++ {
		t := float64(i) / sceneCubicSamples
		mt := 1 - t
		a, b, d, f := mt*mt*mt, 3*mt*mt*t, 3*mt*t*t, t*t*t
		x := a*x0 + b*c1x + d*c2x + f*x3
		y := a*y0 + b*c1y + d*c2y + f*y3
		dx, dy := x-px, y-py
		total += math.Sqrt(dx*dx + dy*dy)
		px, py = x, y
	}
	return total
}
