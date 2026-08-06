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
func (e *Engine) SyncScenePoints(areas []fleet.SceneArea) (int, map[string]string) {
	gate, prev := e.sceneGate()
	return scenesync.SyncScenePoints(e.db, scenesync.LogFn(e.logFn), areas, gate, prev)
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
func (e *Engine) sceneGate() (string, *time.Time) {
	p, ok := e.fleet.(fleet.SceneStateProvider)
	if !ok {
		return "", nil
	}
	state, seen := p.GetSceneState()
	if !seen {
		return "", nil
	}
	prev := e.lastSceneSync
	e.lastSceneSync = &state.ObservedAt
	return state.SceneMD5, prev
}

// SyncFleetNodes creates nodes for new scene locations and removes
// nodes no longer in the scene.
func (e *Engine) SyncFleetNodes(locationSet map[string]string) (created, deleted int) {
	return scenesync.SyncFleetNodes(e.db, scenesync.LogFn(e.logFn), e.emitNodeChange, locationSet)
}

// UpdateNodeZones updates node zones from a location→area map.
// If overwrite is true, updates zone whenever it differs; if false,
// only fills empty zones.
func (e *Engine) UpdateNodeZones(locationSet map[string]string, overwrite bool) {
	scenesync.UpdateNodeZones(e.db, scenesync.LogFn(e.logFn), e.emitNodeChange, locationSet, overwrite)
}

// SceneSync loads scene data from the fleet backend and syncs nodes.
// It is guarded by an atomic bool to prevent concurrent runs.
func (e *Engine) SceneSync() (int, int, int, error) {
	syncer, ok := e.fleet.(fleet.SceneSyncer)
	if !ok {
		return 0, 0, 0, fmt.Errorf("fleet backend does not support scene sync")
	}
	gate, prev := e.sceneGate()
	return scenesync.Sync(e.db, scenesync.LogFn(e.logFn), e.emitNodeChange, syncer, &e.sceneSyncing, gate, prev)
}
