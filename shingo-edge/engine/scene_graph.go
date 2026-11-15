package engine

import "shingo/protocol"

// scene_graph.go — the vendor map's own universe, cached from the node list.
//
// SHINGO WORKS IN APs. Every node Shingo knows is a point it gave a job to, so
// the synced node list is a SUBSET of the map — and a key route is expressed in
// the vendor's universe, where a plain waypoint (class LM) is the primary use.
// Validating a route against the node list therefore refuses correct routes,
// confidently. Core has mirrored the whole scene graph since the SEER adapter
// was written; its point slice is it arriving. (The drivable segments ride the
// same response; only the geometry cache, scene_geometry.go, keeps them.)
//
// IN-MEMORY ONLY, and re-delivered on every node-list sync — the same contract
// as the payload→dunnage catalog beside it. Freshness is node-sync freshness,
// and an Edge that has not heard from Core answers "I don't know", which the
// key-route validator degrades to a warning rather than a refusal.

// SetSceneGraph caches the scene points delivered by Core on each
// NodeListResponse. Called from ApplyNodeList (node_feed.go) on every reply.
func (e *Engine) SetSceneGraph(points []protocol.ScenePointInfo) {
	e.sceneGraphMu.Lock()
	e.scenePoints = points
	e.sceneGraphMu.Unlock()
}

// ScenePoints returns the cached map points.
func (e *Engine) ScenePoints() []protocol.ScenePointInfo {
	e.sceneGraphMu.RLock()
	defer e.sceneGraphMu.RUnlock()
	return e.scenePoints
}

// ScenePointNames is the cached map points as a name set, which is the shape
// key-route validation wants.
//
// EMPTY MEANS "COULD NOT LOOK", never "the map has no points" — the same rule
// as CoreNodes, and the reason the validator warns instead of refusing when it
// gets nothing back. A fresh Edge, a restart or a Kafka gap all produce an
// empty set, and refusing a configuration write on that basis would brick setup
// exactly when someone is most likely to be doing it.
func (e *Engine) ScenePointNames() map[string]bool {
	pts := e.ScenePoints()
	if len(pts) == 0 {
		return nil
	}
	out := make(map[string]bool, len(pts))
	for _, p := range pts {
		if p.InstanceName != "" {
			out[p.InstanceName] = true
		}
	}
	return out
}
