package engine

import (
	"shingo/protocol"
)

// node_feed.go — applying Core's node list and catalog, whether they came as
// a reply to an ask or as a feed Core sent because a heartbeat's digest
// differed. The two arrive on the same subjects and apply the same way.

// ApplyNodeList applies one node-list message from Core.
//
// A DIGEST EQUAL TO THE ONE HELD APPLIES NONE OF THE THREE SLICES IT COVERS.
// Each of them costs something on every call — SetCoreNodes bumps the plant
// generation and rebuilds every station view, SetCoreLoaders rewrites the
// loader cache — and an identical list changes nothing they produce. A reply
// with no digest (an older Core) applies as it always did.
//
// The scene is applied on every message: its names ride every reply, and the
// geometry store already ignores a revision it holds.
//
// The digest is recorded only once the loader cache write succeeded, so a
// failed apply leaves the old digest, the next heartbeat differs, and Core
// sends again. An empty loader set from a successful read is a value like any
// other: it applies and its digest is held.
func (e *Engine) ApplyNodeList(resp *protocol.NodeListResponse) {
	if resp.Digest == "" || resp.Digest != e.heldDigest(protocol.FeedNodes) {
		e.SetCoreNodes(resp.Nodes)
		loadersErr := e.SetCoreLoaders(resp.Loaders)
		e.SetPayloadBinTypes(resp.PayloadBinTypes)
		if loadersErr == nil && resp.Digest != "" {
			e.holdFeed(protocol.FeedNodes, resp.Digest, "")
		}
	}
	e.SetSceneGraph(resp.ScenePoints, resp.SceneEdges)
	// The same two slices, second consumer: replaced only when the response
	// carries the whole scene with its revision.
	e.SetSceneGeometry(resp.SceneRevision, resp.ScenePoints, resp.SceneEdges)
	if resp.SceneRevision != "" {
		e.holdFeed(protocol.FeedScene, e.SceneRevision(), "")
	}
}

// ApplyCatalog applies one catalog message from Core and, when it carries a
// digest and the rows landed, holds that digest.
func (e *Engine) ApplyCatalog(resp *protocol.CatalogPayloadsResponse) {
	if err := e.HandlePayloadCatalog(resp.Payloads); err == nil && resp.Digest != "" {
		e.holdFeed(protocol.FeedCatalog, resp.Digest, "")
	}
}
