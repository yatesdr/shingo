package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
)

// node_list_apply_feedspin_test.go — what a node-list reply and a catalog
// reply do to the Edge, pinned before the versioned feeds make an identical
// reply a no-op.
//
// applyNodeListReply is what the SubjectNodeListResponse closure in
// cmd/shingoedge/main.go does (pinned there): since F4, eng.ApplyNodeList.

func applyNodeListReply(e *Engine, resp *protocol.NodeListResponse) {
	e.ApplyNodeList(resp)
}

// feedspinDigest is the FeedNodes digest Core puts on a reply carrying the
// pin's nodes, loaders and bin types.
func feedspinDigest(t *testing.T) string {
	t.Helper()
	d, err := protocol.NodesDigest(feedspinNodes(), feedspinLoaders(), feedspinBinTypes())
	testutil.MustNoErr(t, err, "digest")
	return d
}

// totalChanges is SQLite's running count of rows inserted, updated or deleted
// on the connection. The Edge store holds exactly one connection
// (SetMaxOpenConns(1)), so a difference across a call is every row that call
// wrote — the measure of a write that rewrites identical data.
func totalChanges(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var n int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT total_changes()`).Scan(&n), "read total_changes")
	return n
}

// countCoreNodesEvents counts EventCoreNodesUpdated on the engine's bus. The
// bus is synchronous, so the count is final when the emitting call returns.
func countCoreNodesEvents(e *Engine) *int {
	n := 0
	e.Events.SubscribeTypes(func(Event) { n++ }, EventCoreNodesUpdated)
	return &n
}

func feedspinNodes() []protocol.NodeInfo {
	return []protocol.NodeInfo{{Name: "LN-1", NodeType: "cell"}, {Name: "GRP-1.LN-2", NodeType: "storage"}}
}

func feedspinLoaders() []protocol.LoaderInfo {
	return []protocol.LoaderInfo{sharedLoaderInfo("LD-1", "produce", "operator", "PART-A", 0, 0)}
}

func feedspinBinTypes() []protocol.PayloadBinTypeInfo {
	return []protocol.PayloadBinTypeInfo{{PayloadCode: "PART-A", BinTypeCode: "TOTE-1"}}
}

// namesOnly is the scene part of a reply when the Edge quoted the revision Core
// holds: names, no geometry, the revision repeated.
func namesOnly() ([]protocol.ScenePointInfo, []protocol.SceneEdgeInfo) {
	pts, eds := completeScene()
	np := make([]protocol.ScenePointInfo, len(pts))
	for i, p := range pts {
		np[i] = protocol.ScenePointInfo{InstanceName: p.InstanceName, ClassName: p.ClassName}
	}
	ne := make([]protocol.SceneEdgeInfo, len(eds))
	for i, e := range eds {
		ne[i] = protocol.SceneEdgeInfo{From: e.From, To: e.To}
	}
	return np, ne
}

// TestFeedsPin_NodeListReplyApply applies a first full reply, then a second
// reply, and measures what the second one moved. The two "identical repeat"
// cases are the same input today; after F4 the first carries a Digest equal to
// the held one and the second (an old Core) carries none.
func TestFeedsPin_NodeListReplyApply(t *testing.T) {
	t.Parallel()
	type moved struct {
		genBump  uint64 // plant generation delta
		events   int    // EventCoreNodesUpdated emitted
		rowsHit  bool   // any row written by the apply
		revision string // scene revision held afterwards
		pln01X   float64
	}
	pts, eds := completeScene()
	pts2, eds2 := completeScene()
	pts2[0].PosX = fp(-16.5)
	np, ne := namesOnly()

	digest := feedspinDigest(t)
	cases := []struct {
		name   string
		digest string // on both replies (F4); "" is an older Core
		second protocol.NodeListResponse
		want   moved
		after  moved
		label  string
	}{
		{
			name:   "identical repeat, digest matching the held one",
			digest: digest,
			second: protocol.NodeListResponse{Nodes: feedspinNodes(), Loaders: feedspinLoaders(),
				PayloadBinTypes: feedspinBinTypes(), ScenePoints: np, SceneEdges: ne, SceneRevision: "rev-1"},
			// Before F4: SetCoreNodes bumped and emitted on every call and
			// ReplaceCoreLoaders deleted and re-inserted every loader row
			// (genBump 1, events 1, rowsHit true).
			want:  moved{genBump: 0, events: 0, rowsHit: false, revision: "rev-1", pln01X: -16.929}, // F4
			after: moved{genBump: 0, events: 0, rowsHit: false, revision: "rev-1", pln01X: -16.929},
			label: "F4 (B16)",
		},
		{
			name: "identical repeat, no digest (old Core)",
			second: protocol.NodeListResponse{Nodes: feedspinNodes(), Loaders: feedspinLoaders(),
				PayloadBinTypes: feedspinBinTypes(), ScenePoints: np, SceneEdges: ne, SceneRevision: "rev-1"},
			want:  moved{genBump: 1, events: 1, rowsHit: true, revision: "rev-1", pln01X: -16.929},
			after: moved{genBump: 1, events: 1, rowsHit: true, revision: "rev-1", pln01X: -16.929},
			label: "same",
		},
		{
			// Same nodes, loaders and bin types; the scene moved to a new
			// revision. The geometry is applied (and written) either way.
			name:   "scene-only change",
			digest: digest,
			second: protocol.NodeListResponse{Nodes: feedspinNodes(), Loaders: feedspinLoaders(),
				PayloadBinTypes: feedspinBinTypes(), ScenePoints: pts2, SceneEdges: eds2, SceneRevision: "rev-2"},
			// Before F4: genBump 2, events 1.
			want:  moved{genBump: 1, events: 0, rowsHit: true, revision: "rev-2", pln01X: -16.5}, // F4
			after: moved{genBump: 1, events: 0, rowsHit: true, revision: "rev-2", pln01X: -16.5},
			label: "F4 (the scene applies; with a matching nodes digest only SetSceneGeometry bumps)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			e := testEngine(t, db)
			applyNodeListReply(e, &protocol.NodeListResponse{Nodes: feedspinNodes(), Loaders: feedspinLoaders(),
				PayloadBinTypes: feedspinBinTypes(), ScenePoints: pts, SceneEdges: eds, SceneRevision: "rev-1",
				Digest: tc.digest})
			if e.SceneRevision() != "rev-1" {
				t.Fatalf("first reply did not cache rev-1 (held %q)", e.SceneRevision())
			}

			events := countCoreNodesEvents(e)
			gen0, rows0 := e.PlantGeneration(), totalChanges(t, db)
			second := tc.second
			second.Digest = tc.digest
			applyNodeListReply(e, &second)
			got := moved{
				genBump:  e.PlantGeneration() - gen0,
				events:   *events,
				rowsHit:  totalChanges(t, db) > rows0,
				revision: e.SceneRevision(),
			}
			if g := e.SceneGeometry(); g != nil {
				got.pln01X = g.Points["PLN_01"].X
			}
			if got != tc.want {
				t.Errorf("second reply moved %+v, want %+v (after %s: %+v)", got, tc.want, tc.label, tc.after)
			}
			// The node set, the loader cache and the bin types hold the reply's
			// values in every case, before and after.
			if n := e.CoreNodes(); len(n) != 2 || n["LN-2"].Name != "LN-2" {
				t.Errorf("core nodes = %+v, want LN-1 and the bare LN-2", n)
			}
			if ls, err := db.ListCoreLoaders(); err != nil || len(ls) != 1 {
				t.Errorf("cached loaders = %d (err %v), want 1", len(ls), err)
			}
			if bt := e.BinTypeForPayload("PART-A"); bt != "TOTE-1" {
				t.Errorf("bin type for PART-A = %q, want TOTE-1", bt)
			}
		})
	}
}

// TestFeedsPin_EmptyLoaderSet: a reply with no loaders truncates the loader
// cache, and the claim reconcile declines to quarantine the stored manual_swap
// claim. after (F1): the same apply; SetCoreLoaders returns an error value
// (nil here) and, the apply having succeeded, the empty set's digest is stored.
func TestFeedsPin_EmptyLoaderSet(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	testutil.MustNoErr(t, db.ReplaceCoreLoaders(feedspinLoaders()), "seed loader cache")
	_, claimID := seedManualSwapClaim(t, db, "EMPTYSET", protocol.ClaimRoleProduce, "PART-A", "OUT")

	applyNodeListReply(e, &protocol.NodeListResponse{Nodes: feedspinNodes(), Loaders: nil})

	var stored, archived int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM style_node_claims WHERE id=?`, claimID).Scan(&stored), "count stored")
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM style_node_claims_quarantine WHERE id=?`, claimID).Scan(&archived), "count archived")
	ls, err := db.ListCoreLoaders()
	testutil.MustNoErr(t, err, "list loaders")

	cases := []struct {
		name      string
		got, want int
		after     int
		label     string
	}{
		{"cached loaders", len(ls), 0, 0, "same"},
		{"stored manual_swap claim left in place", stored, 1, 1, "same"},
		{"claims quarantined", archived, 0, 0, "same"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d (after %s: %d)", tc.name, tc.got, tc.want, tc.label, tc.after)
		}
	}
}

// TestFeedsPin_CatalogApplyUnchangedWritesNothing: the catalog sync writes
// its rows on first sight and nothing when the same catalog comes again.
// after: same (F4 adds a digest beside it; the conditional upsert is kept).
func TestFeedsPin_CatalogApplyUnchangedWritesNothing(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	entries := []protocol.CatalogPayloadInfo{
		{ID: 1, Name: "Part A", Code: "PART-A", UOPCapacity: 24},
		{ID: 2, Name: "Part B", Code: "PART-B", UOPCapacity: 48},
	}
	cases := []struct {
		name        string
		want, after int64
		label       string
	}{
		{"first sync", 2, 2, "same"},
		{"identical repeat", 0, 0, "same"},
	}
	for _, tc := range cases {
		before := totalChanges(t, db)
		e.HandlePayloadCatalog(entries)
		if got := totalChanges(t, db) - before; got != tc.want {
			t.Errorf("%s wrote %d rows, want %d (after %s: %d)", tc.name, got, tc.want, tc.label, tc.after)
		}
	}
}
