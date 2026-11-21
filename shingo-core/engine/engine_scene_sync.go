package engine

import (
	"fmt"
	"time"

	"shingocore/fleet"
	"shingocore/scenesync"
)

// ── Scene sync (fleet → DB → nodes) ─────────────────────────────────
//
// Logic lives in shingocore/scenesync. Engine holds the orchestration
// state (sceneSyncing atomic.Bool) and wires in the event bus via
// onNodeChange. External callers keep the same method signatures.

func (e *Engine) emitNodeChange(nodeID int64, nodeName, action string) {
	e.Events.Emit(Event{Type: EventNodeUpdated, Payload: NodeUpdatedEvent{
		NodeID: nodeID, NodeName: nodeName, Action: action,
	}})
}

// SyncScenePoints persists fleet scene areas to the database.
// Returns the total number of points synced and a map of bin
// location instanceName → areaName.
//
// The scenesync.LogFn(...) conversion is required because
// engine.LogFunc and scenesync.LogFn are structurally identical
// but nominally distinct named types; Go requires type identity,
// not structural equivalence, for assignment. The conversion is
// free at runtime.
//
// Robot routes are named from the scene this writes, so the route index is
// rebuilt after it (rebuildPointIndex). The Edges hold the scene, so it goes
// through the plant-wide change door after; an area written again unchanged
// keeps its revision (scene.Revision), and nothing is sent for it.
func (e *Engine) SyncScenePoints(areas []fleet.SceneArea) (int, map[string]string) {
	gate, prev := e.sceneGate()
	total, locations := scenesync.SyncScenePoints(e.db, scenesync.LogFn(e.logFn), areas, gate, prev)
	e.rebuildPointIndex()
	e.feedDoors.FeedsChangedPlantWide()
	return total, locations
}

// sceneGate reports the scene hash this sync is being taken against, and when
// the previous one landed.
//
// The hash is what says a sync is an EDIT rather than a restart: SceneSync
// today fires on a fleet reconnect, which is restart-shaped, so without it
// every Core bounce would look like a map change. It arrives on the
// /robotsStatus envelope that Core already polls every two seconds.
//
// An empty hash means the fleet backend has not been polled yet or does not
// publish one. The diff still runs — a real edit must not be missed because
// the gate was unknown — but it is recorded as ungated so a reader can tell
// "the scene hash moved" from "we looked and could not tell".
//
// THE READ-MODIFY-WRITE OF lastSceneSync TAKES A LOCK, and it is a real lock
// rather than a comment because e.sceneSyncing does not cover this. SceneSync
// calls this BEFORE passing the atomic to scenesync.Sync, and SyncScenePoints
// calls it without touching the atomic at all — so two HTTP handlers and the
// fleet-reconnect goroutine can be inside this function together. Without the
// lock two syncs read the same `prev` and one diff row claims a window that
// another sync already consumed.
func (e *Engine) sceneGate() (string, *time.Time) {
	p, ok := e.fleet.(fleet.SceneStateProvider)
	if !ok {
		return "", nil
	}
	state, seen := p.GetSceneState()
	if !seen {
		return "", nil
	}
	e.sceneGateMu.Lock()
	defer e.sceneGateMu.Unlock()
	prev := e.lastSceneSync
	e.lastSceneSync = &state.ObservedAt
	return state.SceneMD5, prev
}

// SyncFleetNodes creates nodes for new scene locations and removes
// nodes no longer in the scene, then goes through the plant-wide change door
// when it did either: a station with no nodes of its own is sent every root.
func (e *Engine) SyncFleetNodes(locationSet map[string]string) (created, deleted int) {
	created, deleted = scenesync.SyncFleetNodes(e.db, scenesync.LogFn(e.logFn), e.emitNodeChange, locationSet)
	if created+deleted > 0 {
		e.feedDoors.FeedsChangedPlantWide()
	}
	return created, deleted
}

// UpdateNodeZones updates node zones from a location→area map.
// If overwrite is true, updates zone whenever it differs; if false,
// only fills empty zones.
func (e *Engine) UpdateNodeZones(locationSet map[string]string, overwrite bool) {
	scenesync.UpdateNodeZones(e.db, scenesync.LogFn(e.logFn), e.emitNodeChange, locationSet, overwrite)
}

// SceneSync loads scene data from the fleet backend and syncs nodes.
// It is guarded by an atomic bool to prevent concurrent runs. A sync that
// wrote rebuilds the robot-route index (rebuildPointIndex) and goes through
// the plant-wide change door (the scene and the nodes it made or removed);
// one that failed before writing (no scene from the fleet, a sync already
// running) does neither.
func (e *Engine) SceneSync() (int, int, int, error) {
	syncer, ok := e.fleet.(fleet.SceneSyncer)
	if !ok {
		return 0, 0, 0, fmt.Errorf("fleet backend does not support scene sync")
	}
	gate, prev := e.sceneGate()
	total, created, deleted, err := scenesync.Sync(e.db, scenesync.LogFn(e.logFn), e.emitNodeChange, syncer, &e.sceneSyncing, gate, prev)
	if err == nil {
		e.rebuildPointIndex()
		e.feedDoors.FeedsChangedPlantWide()
	}
	return total, created, deleted, err
}
