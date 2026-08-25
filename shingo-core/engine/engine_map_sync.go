package engine

import (
	"time"

	"shingocore/fleet"
	"shingocore/scenemap"
	"shingocore/store/sceneversion"
)

// ── Map sync (robot .smap → DB), and the scene gate that belongs beside it ──
//
// THIS IS THE LOOP COMMIT 3 WAS MISSING. The client, the parser, the versioner
// and the store all landed and nothing called them, so every lane sat on its
// first open-lower-bound version forever, no diff was ever detected, and
// /api/map/areas and /api/map/reflectors answered [] on a live plant. Every
// piece below already existed; what was absent was the thing that decides WHEN.
//
// TWO TRANSPORTS, TWO GATES, ONE LOOP.
//
//	areas + reflectors   the robot's own .smap, gated by current_map_md5
//	lanes                RDS /scene, gated by scene_md5
//
// Both hashes already arrive on the /robotsStatus poll Core makes every two
// seconds — the map hash per robot, the scene hash on the envelope — so
// neither gate costs a request. That is the whole reason the design refused a
// second connection to every robot: RDS already aggregates, and the cheap half
// of the decision is already in hand.
//
// A DAILY FLOOR UNDER BOTH. A gate that only fires on change cannot tell "the
// map is stable" from "the hash stopped arriving", and the second is a silent
// stop. Re-reading once a day is one 7 MB transfer against an idempotent
// writer: the same map observed twice writes one version row and no diff, so
// the floor costs a transfer and never pollutes the diff log.

const (
	// mapSyncInterval is how often the GATE is evaluated, not how often a map
	// is fetched. Evaluating it is a cache read and one indexed row; the fetch
	// behind it runs only when a hash moved or the floor elapsed.
	mapSyncInterval = 5 * time.Minute

	// mapSyncFloor is the backstop re-read. See the header: without it, a
	// stopped hash and a stable plant are the same observation.
	mapSyncFloor = 24 * time.Hour

	// sceneSyncFloor is the same backstop for the lane network. SceneSync
	// already runs on fleet reconnect, which is frequent at these plants —
	// this covers a Core that stays up for days.
	sceneSyncFloor = 24 * time.Hour
)

// mapSyncLoop evaluates both gates on a slow ticker.
func (e *Engine) mapSyncLoop() {
	ticker := time.NewTicker(mapSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopChan:
			return
		case <-ticker.C:
			if !e.fleetConnected.Load() {
				continue
			}
			e.mapSyncPass(time.Now())
			e.sceneSyncPass(time.Now())
		}
	}
}

// fleetMap is the map the fleet is agreed on, and who can serve it.
type fleetMap struct {
	name string
	md5  string
	// vehicle is a robot currently running this map — the one #4011 is asked.
	vehicle string
	// minority counts robots on some OTHER map. Non-zero is the Hopkinsville
	// state: eleven robots on Hop_20 and AMR-11 on Hop_21, connected, held
	// undispatchable by RDS.
	minority int
}

// majorityMap picks the map most of the fleet is running.
//
// THE MAJORITY, NOT EVERY MAP, AND THAT IS A SCHEMA CONSTRAINT RATHER THAN A
// PREFERENCE. scene_areas carries no map name — its identity is area_name, with
// one open version per area enforced by a partial unique index. Two maps synced
// into it would fight over the same rows: area "01" from one map would close
// area "01" from the other, and the diff log would fill with edits nobody made
// as the two took turns. Until an area row knows which map it came from, one
// map is the only honest thing to store.
//
// A robot on a minority map is not ignored, it is COUNTED and logged — and the
// roll-up already quarantines its samples through the same majority rule
// (FleetMapMode), so the two agree by construction rather than by coincidence.
func majorityMap(robots []fleet.RobotStatus) (fleetMap, bool) {
	type tally struct {
		n       int
		md5     string
		vehicle string
	}
	counts := map[string]*tally{}
	for _, r := range robots {
		// A robot with no map hash cannot vote: an empty hash is "we did not
		// collect it", not agreement with whatever the others report.
		if r.VehicleID == "" || r.CurrentMap == "" || r.MapMD5 == "" {
			continue
		}
		// A DISCONNECTED ROBOT STILL REPORTS ITS LAST MAP, AND CANNOT SERVE IT.
		//
		// RDS keeps publishing a robot's cached basic_info after the link drops,
		// so an offline robot looks like a perfectly good source: name, map,
		// hash, all present. Ask it for its map through the proxy and RDS tries,
		// times out after ~3 s, and answers {"code":0,"msg":"ok"} -- a success
		// envelope for a robot that is not there.
		//
		// Measured at Springfield 2026-08-07: AMR-01 was connection_status 0 and
		// returned exactly that for every Robokit call, while AMR-03 and AMR-08
		// returned the whole 7.3 MB map in under three seconds. Picking the
		// wrong one is the difference between a working sync and a permanent
		// silent failure.
		//
		// It is excluded from the VOTE as well as from serving, because its map
		// hash is a memory of whenever it dropped -- during a fleet-wide map
		// push, a stale vote is a vote for the old map.
		if !r.Connected {
			continue
		}
		t := counts[r.CurrentMap]
		if t == nil {
			t = &tally{}
			counts[r.CurrentMap] = t
		}
		t.n++
		if t.vehicle == "" {
			t.vehicle = r.VehicleID
			t.md5 = r.MapMD5
		}
	}
	var best fleetMap
	var bestN int
	total := 0
	for name, t := range counts {
		total += t.n
		// Ties broken by name so a fleet split evenly between two maps picks
		// the same one on every pass. A gate that alternated would rewrite the
		// whole area set twice a day and call each rewrite an edit.
		if t.n > bestN || (t.n == bestN && name < best.name) {
			bestN = t.n
			best = fleetMap{name: name, md5: t.md5, vehicle: t.vehicle}
		}
	}
	if bestN == 0 {
		return fleetMap{}, false
	}
	best.minority = total - bestN
	return best, true
}

// mapFetchReason decides whether to pull the map, and says which of the three
// reasons fired.
//
// THE THREE ARE KEPT APART BECAUSE THEY ARE DIFFERENT FACTS. "Never fetched"
// and "unchanged" produce the same empty diff log, and a plant sitting in the
// first one — no areas, no reflectors, every localization question
// unanswerable — looks exactly like a stable plant unless the reason is
// carried. The daily floor is the third: it exists so that a hash which stopped
// arriving is distinguishable from a map that stopped changing.
//
// Pure so the decision can be tested without a database or a robot.
func mapFetchReason(prev sceneversion.MapVersionState, found bool, wireMD5 string, now time.Time) (string, bool) {
	switch {
	case !found:
		return "no version archived yet", true
	case prev.MapMD5 != wireMD5:
		return "map hash moved", true
	case now.Sub(prev.SyncedAt) >= mapSyncFloor:
		return "daily floor", true
	default:
		return "", false
	}
}

// mapSyncPass fetches the fleet's map when its hash has moved, or when the
// daily floor has elapsed.
func (e *Engine) mapSyncPass(now time.Time) {
	dl, ok := e.fleet.(fleet.RobotMapDownloader)
	if !ok {
		return // a backend that cannot serve a map has no areas to sync
	}

	fm, ok := majorityMap(e.GetAllCachedRobots())
	if !ok {
		return // nothing has reported a map hash yet
	}
	if fm.minority > 0 {
		e.logFn("engine: map sync: %d robot(s) are not on %s — their samples are "+
			"quarantined by the roll-up and their map is not archived",
			fm.minority, fm.name)
	}

	prev, found, err := e.db.LatestMapVersion(fm.name)
	if err != nil {
		e.logFn("engine: map sync: read latest version of %s: %v", fm.name, err)
		return
	}

	reason, fetch := mapFetchReason(prev, found, fm.md5, now)
	if !fetch {
		return
	}

	raw, err := dl.DownloadRobotMap(fm.vehicle, fm.name)
	if err != nil {
		e.noteMapSyncFailure(fm.name, reason, err)
		return
	}
	parsed, err := scenemap.Parse(raw)
	if err != nil {
		// STOP HERE. This used to log and fall through to ApplyMapSnapshot on
		// the reasoning that "the raw bytes are still archived -- a body we
		// cannot split is still evidence". THAT WAS FALSE: ApplyMapSnapshot
		// refuses a nil parse outright, so the call could only ever return a
		// second error. A comment promising a guarantee the code does not make
		// is worse than no comment, and this one cost two log lines per pass
		// instead of one.
		e.noteMapSyncFailure(fm.name, reason, err)
		return
	}

	var previousSync *time.Time
	if found {
		at := prev.SyncedAt
		previousSync = &at
	}

	res, err := e.db.ApplyMapSnapshot(sceneversion.MapSnapshot{
		MapName:     fm.name,
		MapMD5:      fm.md5,
		SourceRobot: fm.vehicle,
		Raw:         raw,
		Parsed:      parsed,
		ObservedAt:  now,
	}, previousSync)
	if err != nil {
		e.noteMapSyncFailure(fm.name, reason, err)
		return
	}
	e.clearMapSyncFailure()
	if res.Unchanged {
		// The floor fired and the content was identical. Worth one line,
		// because it is the difference between "stable" and "stopped".
		e.logFn("engine: map sync: %s unchanged (%s)", fm.name, reason)
		return
	}
	e.logFn("engine: map sync: %s (%s) — %s", fm.name, reason, res.String())
	if res.EmptyReflectorAreas > 0 {
		e.logFn("engine: map sync: %d declared reflector zone(s) in %s contain no "+
			"reflectors at all — localization cannot work inside them",
			res.EmptyReflectorAreas, fm.name)
	}
}

// sceneSyncPass runs SceneSync when the RDS scene hash has moved, or when the
// daily floor has elapsed.
//
// SCENE SYNC USED TO HAVE NO SCHEDULE AT ALL. It fired on fleet reconnect,
// which is restart-shaped rather than edit-shaped: it runs often when Core is
// unstable and never when Core is healthy, which is exactly backwards for
// something whose job is to notice map edits. scene_md5 is what makes it
// edit-shaped, and it has been on the /robotsStatus envelope all along.
//
// An unchanged scene is cheap by construction: ApplyLaneDiff rolls its whole
// transaction back when nothing moved, so a floor-driven pass on a stable plant
// writes nothing and leaves no diff row.
func (e *Engine) sceneSyncPass(now time.Time) {
	p, ok := e.fleet.(fleet.SceneStateProvider)
	if !ok {
		return
	}
	state, seen := p.GetSceneState()
	if !seen {
		return // never polled; an empty hash here is not "no scene"
	}

	e.sceneGateMu.Lock()
	lastHash := e.lastSceneHash
	lastAt := e.lastSceneSync
	e.sceneGateMu.Unlock()

	var reason string
	switch {
	case lastAt == nil:
		reason = "no sync since boot"
	case state.SceneMD5 != "" && state.SceneMD5 != lastHash:
		reason = "scene hash moved"
	case now.Sub(*lastAt) >= sceneSyncFloor:
		reason = "daily floor"
	default:
		return
	}

	// SceneSync computes the gate itself and records lastSceneSync; this only
	// decides whether to call it. Recording the hash we acted on is what stops
	// the next tick firing on the same edit.
	if _, _, _, err := e.SceneSync(); err != nil {
		e.noteSceneSyncFailure(reason, err)
		return
	}
	e.clearSceneSyncFailure()
	e.sceneGateMu.Lock()
	e.lastSceneHash = state.SceneMD5
	e.sceneGateMu.Unlock()
	e.logFn("engine: scene sync (%s) complete", reason)
}

// ── Failure reporting that does not train people to ignore the log ─────────
//
// TWO ERRORS EVERY FIVE MINUTES, FOREVER, IS HOW A LOG STOPS BEING READ. The
// map-sync gate re-evaluates on a timer, so a PERSISTENT failure -- a proxy
// that is not relaying, which is the live state at Springfield -- reports
// identically to a transient one and does so 288 times a day. That is the
// "gate clean" pattern rebuilt in operations: a signal that is always present
// carries no information, and the real failure arrives to an audience that
// stopped looking months ago.
//
// So a repeat of the SAME failure is counted, not reprinted. The first
// occurrence logs in full; after that only an escalating summary, at
// exponentially spaced repeats. A change of failure resets it, because a
// different error is news.
const mapSyncFailureFirstQuiet = 4

func (e *Engine) noteMapSyncFailure(mapName, reason string, err error) {
	key := mapName + " | " + err.Error()
	e.sceneGateMu.Lock()
	if key != e.mapSyncFailKey {
		e.mapSyncFailKey, e.mapSyncFailN = key, 0
	}
	e.mapSyncFailN++
	n := e.mapSyncFailN
	e.sceneGateMu.Unlock()

	// 1, then 4, 16, 64, 256... Roughly: immediately, then ~20 min, ~1 h,
	// ~5 h, ~21 h. A stuck plant says so once a day rather than 288 times,
	// and the count makes the duration legible in the one line that prints.
	if n > 1 && n%mapSyncFailureFirstQuiet != 0 {
		return
	}
	if n == 1 {
		e.logFn("engine: map sync: %s (%s): %v", mapName, reason, err)
		return
	}
	e.logFn("engine: map sync: %s STILL failing after %d attempts (%s): %v",
		mapName, n, reason, err)
}

// clearMapSyncFailure is called on any successful sync, so recovery is a
// visible transition rather than a silence that means either "fixed" or
// "still broken, throttled".
func (e *Engine) clearMapSyncFailure() {
	e.sceneGateMu.Lock()
	wasFailing := e.mapSyncFailN > 0
	e.mapSyncFailKey, e.mapSyncFailN = "", 0
	e.sceneGateMu.Unlock()
	if wasFailing {
		e.logFn("engine: map sync: recovered")
	}
}

// noteSceneSyncFailure / clearSceneSyncFailure are the same throttle for the
// SCENE half, which had none: sceneSyncPass logged one unthrottled line per
// five-minute tick, 288 a day, for a failure that does not change.
//
// It matters more here than it looks. scene_points is where the station alias
// lives, so this log line is the freshness signal for the table a bin placement
// now resolves through — and a signal printed 288 times a day is one nobody
// reads on the day it starts meaning something.
func (e *Engine) noteSceneSyncFailure(reason string, err error) {
	key := err.Error()
	e.sceneGateMu.Lock()
	if key != e.sceneSyncFailKey {
		e.sceneSyncFailKey, e.sceneSyncFailN = key, 0
	}
	e.sceneSyncFailN++
	n := e.sceneSyncFailN
	e.sceneGateMu.Unlock()

	if n > 1 && n%mapSyncFailureFirstQuiet != 0 {
		return
	}
	if n == 1 {
		e.logFn("engine: scene sync (%s): %v", reason, err)
		return
	}
	e.logFn("engine: scene sync STILL failing after %d attempts (%s): %v", n, reason, err)
}

func (e *Engine) clearSceneSyncFailure() {
	e.sceneGateMu.Lock()
	wasFailing := e.sceneSyncFailN > 0
	e.sceneSyncFailKey, e.sceneSyncFailN = "", 0
	e.sceneGateMu.Unlock()
	if wasFailing {
		e.logFn("engine: scene sync: recovered")
	}
}
