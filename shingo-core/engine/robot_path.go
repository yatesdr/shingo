package engine

import (
	"math"

	"shingocore/fleet"
	"shingocore/scenesync"
)

// ── The route a robot is on ──────────────────────────────────────────
//
// The fleet plans every route and publishes what is left of it on the
// /robotsStatus poll Core already makes every 2 s (rds.RobotStatus.UnfinishedPath,
// carried up as fleet.RobotStatus.PathIDs). Those are the fleet's bare point
// ids; the map draws scene names. So each poll names the ids from the stored
// scene (scenesync.PointIndex) and writes fleet.RobotStatus.Path before the
// robots reach the cache, the change hash or the robot-update frame. Nothing
// new is fetched, and a route that cannot be named is not drawn: there is no
// estimate in its place (owner ruling, shared-map 2026-11-14).

// rebuildPointIndex reads the scene as stored and swaps in the index the poll
// names routes from. Called at boot and after every scene sync, so the names
// it hands out are always names of rows the map draws.
//
// The lock is what makes "after every sync" true when two syncs overlap (the
// reconnect goroutine, the scene gate and two HTTP handlers can all run one):
// each rebuild reads after its own sync wrote, and the last to take the lock
// reads last, so the index left in place is never older than the database.
//
// A failed read keeps the index already in place. At boot that is none, and
// every route is reported once per robot as having no synced scene.
func (e *Engine) rebuildPointIndex() {
	e.pointIndexMu.Lock()
	defer e.pointIndexMu.Unlock()
	points, err := e.db.ListScenePoints()
	if err != nil {
		e.logFn("engine: robot routes: read scene points: %v (routes keep the previous scene)", err)
		return
	}
	edges, err := e.db.ListSceneEdges()
	if err != nil {
		e.logFn("engine: robot routes: read scene edges: %v (routes keep the previous scene)", err)
		return
	}
	e.pointIndex.Store(scenesync.NewPointIndex(points, edges))
	e.rowScale.Store(math.Float64bits(medianRowLength(edges)))
}

// routeNotDrawn is the last "no route drawn" line one robot was given: the
// index it was judged against and the id that failed. A robot is told again
// only when either changes, so a route that stays unnameable logs once and
// not on every 2 s poll.
type routeNotDrawn struct {
	ix  *scenesync.PointIndex
	bad string
}

// resolveRobotPaths writes each robot's Path from its PathIDs. notDrawn is
// the poll loop's own memory of what it has already logged (single goroutine,
// like prevHash); a robot whose route names again, or that has no route,
// is forgotten so its next failure is reported.
func (e *Engine) resolveRobotPaths(robots []fleet.RobotStatus, notDrawn map[string]routeNotDrawn) {
	ix := e.pointIndex.Load()
	for i := range robots {
		r := &robots[i]
		names, bad, ok := ix.Resolve(r.PathIDs)
		r.Path = names
		if ok {
			delete(notDrawn, r.VehicleID)
			continue
		}
		now := routeNotDrawn{ix: ix, bad: bad}
		if notDrawn[r.VehicleID] == now {
			continue
		}
		notDrawn[r.VehicleID] = now
		e.logFn("engine: robot %s: no route drawn: %s", r.VehicleID, ix.Explain(bad))
	}
}
