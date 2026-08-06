// laneversion.go — the diff that has to happen before the overwrite.
//
// SyncScenePoints mirrors the fleet's scene by DELETING each area's edges and
// re-inserting them. That is fine for a mirror and fatal for a history: by
// the time the new rows land, the old ones are gone, so nothing can measure
// how far anything moved and nothing records that it moved at all. Core has
// therefore never been able to answer "I re-routed that lane Tuesday, did it
// help?" — not because the query was missing, but because there was no before.
//
// The order this file imposes is: read what is stored, fingerprint both
// states, write the version rows and one diff row, and only then let the
// replace proceed.

package scenesync

import (
	"time"

	"shingocore/fleet"
	"shingocore/scenemap"
	"shingocore/store/scene"
	"shingocore/store/sceneversion"
)

// laneEdgesFromStore groups the rows currently in scene_edges by lane.
//
// Edges with an unnamed endpoint are skipped rather than grouped: they have
// no lane key, so there is nothing to version them under. They are counted
// so the omission is visible.
func laneEdgesFromStore(edges []*scene.Edge, area string) (map[string][]scenemap.LaneEdge, int) {
	out := map[string][]scenemap.LaneEdge{}
	unnamed := 0
	for _, e := range edges {
		if e.AreaName != area {
			continue
		}
		key := scenemap.LaneKey(e.FromName, e.ToName)
		if key == "" {
			unnamed++
			continue
		}
		le := scenemap.LaneEdge{
			Instance: e.InstanceName, Class: e.ClassName,
			FromName: e.FromName, ToName: e.ToName,
			From: scenemap.Point{X: e.FromX, Y: e.FromY},
			To:   scenemap.Point{X: e.ToX, Y: e.ToY},
		}
		if e.Ctrl1X != nil && e.Ctrl1Y != nil && e.Ctrl2X != nil && e.Ctrl2Y != nil {
			le.Ctrl1 = &scenemap.Point{X: *e.Ctrl1X, Y: *e.Ctrl1Y}
			le.Ctrl2 = &scenemap.Point{X: *e.Ctrl2X, Y: *e.Ctrl2Y}
		}
		out[key] = append(out[key], le)
	}
	return out, unnamed
}

// laneEdgesFromFleet groups an incoming fleet payload by lane.
//
// Same skip, and this is where it matters most: an edge the fleet publishes
// with no endpoint names is about to be written into scene_edges, where it
// would sit unkeyable forever. See RejectUnnameableEdge.
func laneEdgesFromFleet(area fleet.SceneArea) (map[string][]scenemap.LaneEdge, int) {
	out := map[string][]scenemap.LaneEdge{}
	unnamed := 0
	for _, ed := range area.Edges {
		key := scenemap.LaneKey(ed.FromName, ed.ToName)
		if key == "" {
			unnamed++
			continue
		}
		le := scenemap.LaneEdge{
			Instance: ed.InstanceName, Class: ed.ClassName,
			FromName: ed.FromName, ToName: ed.ToName,
			From: scenemap.Point{X: ed.FromX, Y: ed.FromY},
			To:   scenemap.Point{X: ed.ToX, Y: ed.ToY},
		}
		if ed.Ctrl1 != nil && ed.Ctrl2 != nil {
			le.Ctrl1 = &scenemap.Point{X: ed.Ctrl1.X, Y: ed.Ctrl1.Y}
			le.Ctrl2 = &scenemap.Point{X: ed.Ctrl2.X, Y: ed.Ctrl2.Y}
		}
		out[key] = append(out[key], le)
	}
	return out, unnamed
}

// RejectUnnameableEdge reports whether an incoming edge must not be written.
//
// scene_edges declares from_name/to_name NOT NULL with an empty default, so
// an edge with no endpoint names is storable and useless: it has no lane key,
// so it can carry no version row, and every sample that lands on it is
// quarantined by the roll-up. Refusing it at the source is the fix the
// quarantine exists to make unnecessary.
//
// The vendor's own payload already contains such rows — the adapter skips
// curves with two empty endpoint names before this ever runs — so this guards
// the half-named case the adapter lets through.
func RejectUnnameableEdge(fromName, toName string) bool {
	return scenemap.LaneKey(fromName, toName) == ""
}

// buildLaneVersions fingerprints one area's incoming lanes.
//
// A lane that cannot be fingerprinted is dropped with its reason rather than
// versioned wrong — three directed rows for one lane, or an endpoint that
// lost its name between grouping and hashing, are data faults and inventing a
// version for them would put a number on something nobody measured.
func buildLaneVersions(area string, byLane map[string][]scenemap.LaneEdge,
	log LogFn) []sceneversion.Lane {

	out := make([]sceneversion.Lane, 0, len(byLane))
	for key, edges := range byLane {
		v, err := scenemap.FingerprintLane(edges)
		if err != nil {
			log("scenesync: lane %s/%s not versioned: %v", area, key, err)
			continue
		}
		out = append(out, sceneversion.Lane{
			Area: area, Lane: key, Version: v, Shape: scenemap.LaneShape(edges),
		})
	}
	return out
}

// DiffLanesBeforeReplace fingerprints the incoming scene against what is
// stored and records the edit. Call it BEFORE any delete.
//
// gateHash is the scene_md5 that moved — the value that says this is a new
// scene rather than a restart. previousSync bounds the window the change
// happened in, because a diff row records an OBSERVED edit and two edits
// between syncs are one row.
func DiffLanesBeforeReplace(db Store, log LogFn, areas []fleet.SceneArea,
	gateHash string, observedAt time.Time, previousSync *time.Time) {

	if len(areas) == 0 {
		return
	}
	stored, err := db.ListSceneEdges()
	if err != nil {
		// Without the previous state there is no diff to take. Log and let
		// the sync proceed: a missing version row is recoverable, a refused
		// scene sync is a live map going stale.
		log("scenesync: lane diff skipped, cannot read stored edges: %v", err)
		return
	}

	names := make([]string, 0, len(areas))
	var lanes []sceneversion.Lane
	var unnamedIn, unnamedStored int

	for _, area := range areas {
		names = append(names, area.Name)
		incoming, u := laneEdgesFromFleet(area)
		unnamedIn += u
		lanes = append(lanes, buildLaneVersions(area.Name, incoming, log)...)

		_, us := laneEdgesFromStore(stored, area.Name)
		unnamedStored += us
	}

	if unnamedIn > 0 {
		log("scenesync: %d incoming edge(s) have an unnamed endpoint and were not "+
			"versioned — they have no lane key, so any sample landing on them is "+
			"quarantined by the roll-up", unnamedIn)
	}
	if unnamedStored > 0 {
		log("scenesync: %d stored edge(s) carry no endpoint names", unnamedStored)
	}

	res, err := db.ApplyLaneVersions(sceneversion.SourceRDSScene, gateHash,
		observedAt, previousSync, names, lanes)
	if err != nil {
		log("scenesync: lane diff: %v", err)
		return
	}
	if !res.Changed_() {
		return // an unchanged scene leaves no diff row
	}
	log("scenesync: scene changed — %s", res.String())
	if res.Disagreements > 0 {
		log("scenesync: %d lane(s) whose two directions no longer mirror each other. "+
			"Lane-grain versioning describes only part of the truth for those lanes; "+
			"every Springfield pair mirrored exactly as of 2026-08-06", res.Disagreements)
	}
}
