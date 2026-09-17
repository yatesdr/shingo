package engine

import (
	"log"

	"shingo/protocol"
	"shingoedge/domain"
)

// scene_geometry.go — the vendor map's geometry, cached beside the name set.
//
// scene_graph.go keeps the NAME set for the key-route validator: in-memory,
// re-delivered on every sync, nil meaning "could not look". This file keeps
// the GEOMETRY for the station's cell picture, and it plays by a different
// rule because it is the bulk of the message and Core only sends it when the
// Edge's copy is stale: the hot copy is replaced only by a COMPLETE response
// (domain.NewSceneGeometry), and its revision is what the heartbeater quotes
// on every node-list request so Core can leave the geometry off.
//
// Two consumers of the same two slices, deliberately not one: the validator's
// "nil means could not look" contract and the picture's "keep the last good
// map" contract are opposites, and one cache cannot honour both.

// SetSceneGeometry offers one node-list response to the geometry cache. A
// complete response (revision + every point placed + every edge with its
// endpoints) replaces the hot copy; anything less — the name-only response a
// matching revision produces, a half-read scene with no revision — leaves it
// exactly as it was. Called from the node-list-response handler beside
// SetSceneGraph.
func (e *Engine) SetSceneGeometry(revision string, points []protocol.ScenePointInfo, edges []protocol.SceneEdgeInfo) {
	g, err := domain.NewSceneGeometry(revision, points, edges)
	if err != nil {
		// The ordinary case is a matching revision (names only), which is not
		// worth a log line; a response WITH a revision that still fails the
		// completeness check is something Core did wrong.
		if revision != "" {
			log.Printf("scene geometry: response at revision %s not cached: %v", revision, err)
		}
		return
	}
	// Disk first, then the hot copy, so a write that fails leaves both halves
	// on the previous scene — the hot copy must never be ahead of what a
	// restart would load, or the revision quoted after the restart would be
	// older than the picture the operator was just looking at.
	if e.db != nil {
		if err := e.db.ReplaceSceneGeometry(g); err != nil {
			log.Printf("scene geometry: revision %s not cached: %v", g.Revision, err)
			return
		}
	}
	e.sceneGeometryMu.Lock()
	e.sceneGeometry = g
	e.sceneGeometryMu.Unlock()
	e.bumpPlantGeneration()
	log.Printf("scene geometry: cached revision %s (%d points, %d edges)", g.Revision, len(g.Points), len(g.Edges))
}

// loadSceneGeometry fills the hot copy from the store at boot, so the first
// node-list request already quotes the revision held and the picture is
// drawn before Core has answered anything. A read failure logs and leaves
// the copy empty — the next full sync repairs it — rather than refusing to
// start over a picture.
func (e *Engine) loadSceneGeometry() {
	if e.db == nil {
		return
	}
	g, err := e.db.LoadSceneGeometry()
	if err != nil {
		log.Printf("scene geometry: load at boot failed: %v", err)
		return
	}
	if g == nil {
		return
	}
	e.sceneGeometryMu.Lock()
	e.sceneGeometry = g
	e.sceneGeometryMu.Unlock()
	e.bumpPlantGeneration()
	log.Printf("scene geometry: loaded revision %s from the cache (%d points, %d edges)", g.Revision, len(g.Points), len(g.Edges))
}

// SceneGeometry returns the hot copy, or nil before the first complete
// response. Callers treat nil as "draw the schematic", never as "the plant
// has no map".
func (e *Engine) SceneGeometry() *domain.SceneGeometry {
	e.sceneGeometryMu.RLock()
	defer e.sceneGeometryMu.RUnlock()
	return e.sceneGeometry
}

// SceneRevision is what the heartbeater sends on every node-list request:
// the revision of the geometry held, or "" when none is — which Core reads
// as "send everything".
func (e *Engine) SceneRevision() string {
	if g := e.SceneGeometry(); g != nil {
		return g.Revision
	}
	return ""
}

// ── the plant generation ─────────────────────────────────────────────────────
//
// PlantGeneration is how a station poll tells that the cell picture's two
// plant-side inputs have moved — the scene geometry above and the NGRP
// membership SetCoreNodes retains — without reading either.
//
// ONE COUNTER FOR BOTH, because they are replaced by the same event: a
// node-list response from Core carries the node set and (when the Edge's
// revision is stale) the scene, and both caches are all-or-nothing swaps made
// while handling it. Two counters would be two names for one fact.
//
// SEEDED FROM THE CLOCK for the reason store/processes.NodeGeneration is: the
// counter is in memory, and a restart taking it back to zero could hand a
// browser a version it had already seen.
func (e *Engine) PlantGeneration() uint64 { return e.plantGeneration.Load() }

func (e *Engine) bumpPlantGeneration() { e.plantGeneration.Add(1) }
