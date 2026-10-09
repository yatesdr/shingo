package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// The node-list digest is held only once the reply applied; an empty loader
// set from Core is a value like any other and its digest is held; a reply with
// no digest (an older Core) applies and holds nothing.
func TestApplyNodeList_HoldsTheDigestItApplied(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		resp     protocol.NodeListResponse
		wantHeld string
	}{
		{"with loaders", protocol.NodeListResponse{Nodes: feedspinNodes(), Loaders: feedspinLoaders(), Digest: "n1"}, "n1"},
		{"empty loader set", protocol.NodeListResponse{Nodes: feedspinNodes(), Digest: "n2"}, "n2"},
		{"older Core, no digest", protocol.NodeListResponse{Nodes: feedspinNodes(), Loaders: feedspinLoaders()}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := testEngine(t, testEngineDB(t))
			e.ApplyNodeList(&c.resp)
			if got := e.heldDigest(protocol.FeedNodes); got != c.wantHeld {
				t.Errorf("held nodes digest = %q, want %q", got, c.wantHeld)
			}
			if len(e.CoreNodes()) != 2 {
				t.Errorf("core nodes = %d, want 2 (the reply applied)", len(e.CoreNodes()))
			}
		})
	}
}

// A loader cache write that fails leaves the old digest held, so the next
// heartbeat differs and Core sends again.
func TestApplyNodeList_FailedLoaderWriteKeepsTheOldDigest(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	e.ApplyNodeList(&protocol.NodeListResponse{Nodes: feedspinNodes(), Loaders: feedspinLoaders(), Digest: "n1"})
	_, err := db.DB.Exec(`ALTER TABLE core_loaders RENAME TO core_loaders_hidden`)
	testutil.MustNoErr(t, err, "hide core_loaders")

	e.ApplyNodeList(&protocol.NodeListResponse{Nodes: feedspinNodes(), Loaders: feedspinLoaders(), Digest: "n2"})
	if got := e.heldDigest(protocol.FeedNodes); got != "n1" {
		t.Errorf("held nodes digest after a failed loader write = %q, want the old n1", got)
	}
}

// The catalog digest is held after the rows land; an older Core's reply holds
// nothing.
func TestApplyCatalog_HoldsTheDigest(t *testing.T) {
	t.Parallel()
	e := testEngine(t, testEngineDB(t))
	entries := []protocol.CatalogPayloadInfo{{ID: 1, Name: "Part A", Code: "PART-A", UOPCapacity: 24}}
	e.ApplyCatalog(&protocol.CatalogPayloadsResponse{Payloads: entries})
	if got := e.heldDigest(protocol.FeedCatalog); got != "" {
		t.Errorf("older Core's reply held %q, want nothing", got)
	}
	e.ApplyCatalog(&protocol.CatalogPayloadsResponse{Payloads: entries, Digest: "c1"})
	if got := e.heldDigest(protocol.FeedCatalog); got != "c1" {
		t.Errorf("held catalog digest = %q, want c1", got)
	}
}

// The heartbeat names the node list, the scene and the catalog: the held
// digests, the scene revision the geometry store holds, "" where nothing is
// held yet.
func TestFeedDigests_NodesSceneCatalog(t *testing.T) {
	t.Parallel()
	e := testEngine(t, testEngineDB(t))
	got := e.FeedDigests()
	for _, key := range []string{protocol.FeedNodes, protocol.FeedScene, protocol.FeedCatalog} {
		if d, ok := got[key]; !ok || d != "" {
			t.Errorf("fresh Edge: %s = %q (present %v), want present and empty", key, d, ok)
		}
	}

	pts, eds := completeScene()
	e.ApplyNodeList(&protocol.NodeListResponse{Nodes: feedspinNodes(), Digest: "n1",
		ScenePoints: pts, SceneEdges: eds, SceneRevision: "rev-1"})
	got = e.FeedDigests()
	if got[protocol.FeedNodes] != "n1" || got[protocol.FeedScene] != "rev-1" {
		t.Errorf("after a reply: %v, want nodes n1 and scene rev-1", got)
	}
}
