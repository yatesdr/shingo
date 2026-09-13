//go:build docker

package messaging

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingo/shared/scenefixtures"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store/scene"
)

// core_data_service_scene_test.go — the geometry half of the scene slices on
// the node list, and the revision that lets Core leave it off.
//
// Names go every time: the key-route validator and the "absence is never a
// finding" guards read the full name set on every sync. Geometry goes when the
// Edge's revision does not match — which covers an Edge that has never held
// one, an Edge whose copy is stale, and an older binary that sends no
// revision at all, because all three look identical on the wire.

func f(v float64) *float64 { return &v }

func sceneRequest(t *testing.T, db interface {
	UpsertScenePoint(*scene.Point) error
	UpsertSceneEdge(*scene.Edge) error
}) {
	t.Helper()
	// A bin location AT THE ORIGIN — the coordinate a bare float would drop.
	testutil.MustNoErr(t, db.UpsertScenePoint(&scene.Point{
		AreaName: "AREA-G", InstanceName: "PLN_ORIGIN", ClassName: "GeneralLocation", PointName: "AP1",
		PosX: 0, PosY: 0, Dir: 0, PropertiesJSON: "{}",
	}), "upsert origin point")
	testutil.MustNoErr(t, db.UpsertScenePoint(&scene.Point{
		AreaName: "AREA-G", InstanceName: "LM9", ClassName: "LocationMark",
		PosX: -0.604, PosY: 22.449, Dir: 1.5708, PropertiesJSON: "{}",
	}), "upsert waypoint")
	// One bowed segment with all four handles (Springfield's LM9-PP224, the
	// only BezierPath the plant has) and one StraightPath with NULL handles.
	testutil.MustNoErr(t, db.UpsertSceneEdge(&scene.Edge{
		AreaName: "AREA-G", InstanceName: "LM9-PP224", ClassName: "BezierPath", FromName: "LM9", ToName: "PP224",
		FromX: -0.604, FromY: 22.449, ToX: 0.986, ToY: 22.169,
		Ctrl1X: f(-0.287), Ctrl1Y: f(22.094), Ctrl2X: f(0.303), Ctrl2Y: f(22.142),
	}), "upsert bezier edge")
	testutil.MustNoErr(t, db.UpsertSceneEdge(&scene.Edge{
		AreaName: "AREA-G", InstanceName: "CP51-LM54", ClassName: "StraightPath", FromName: "CP51", ToName: "LM54",
		FromX: -9.338, FromY: 6.23, ToX: -8.855, ToY: 6.221,
	}), "upsert straight edge")
}

func nodeListReply(t *testing.T, svc *CoreDataService, resp *captureResponder, rev string) *protocol.NodeListResponse {
	t.Helper()
	before := len(resp.replies)
	env := &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleEdge, Station: "edge.geometry"},
		Dst: protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
	svc.HandleNodeListRequest(env, &protocol.NodeListRequest{SceneRevision: rev})
	if len(resp.replies) != before+1 {
		t.Fatalf("expected one reply, got %d new", len(resp.replies)-before)
	}
	out, ok := resp.replies[before].payload.(*protocol.NodeListResponse)
	if !ok {
		t.Fatalf("reply payload is %T, want *protocol.NodeListResponse", resp.replies[before].payload)
	}
	return out
}

func pointByName(pts []protocol.ScenePointInfo, name string) *protocol.ScenePointInfo {
	for i := range pts {
		if pts[i].InstanceName == name {
			return &pts[i]
		}
	}
	return nil
}

func edgeByEnds(edges []protocol.SceneEdgeInfo, from, to string) *protocol.SceneEdgeInfo {
	for i := range edges {
		if edges[i].From == from && edges[i].To == to {
			return &edges[i]
		}
	}
	return nil
}

func hasGeometry(r *protocol.NodeListResponse) bool {
	for _, p := range r.ScenePoints {
		if p.PosX != nil || p.PosY != nil {
			return true
		}
	}
	for _, e := range r.SceneEdges {
		if e.FromX != nil || e.Ctrl1X != nil {
			return true
		}
	}
	return false
}

func TestNodeListResponse_SceneGeometryFollowsTheRevision(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sceneRequest(t, db)
	resp := &captureResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})

	// An Edge with no revision — a fresh cache, or an older binary that never
	// sends the field — gets everything.
	full := nodeListReply(t, svc, resp, "")
	if full.SceneRevision == "" {
		t.Fatal("a response cut from a readable scene must carry its revision")
	}
	origin := pointByName(full.ScenePoints, "PLN_ORIGIN")
	if origin == nil {
		t.Fatal("PLN_ORIGIN missing from the point set")
	}
	if origin.PosX == nil || origin.PosY == nil || *origin.PosX != 0 || *origin.PosY != 0 {
		t.Errorf("the origin arrived as %+v — (0,0) is a real position and must survive the wire", origin)
	}
	if origin.Dir == nil {
		t.Error("dir dropped on the origin point")
	}
	bez := edgeByEnds(full.SceneEdges, "LM9", "PP224")
	if bez == nil {
		t.Fatal("LM9->PP224 missing from the edge set")
	}
	if bez.Ctrl1X == nil || bez.Ctrl1Y == nil || bez.Ctrl2X == nil || bez.Ctrl2Y == nil {
		t.Errorf("the Bezier segment lost its handles: %+v", bez)
	} else if *bez.Ctrl1X != -0.287 || *bez.Ctrl2Y != 22.142 {
		t.Errorf("handles = (%v,%v)/(%v,%v), want (-0.287,22.094)/(0.303,22.142)", *bez.Ctrl1X, *bez.Ctrl1Y, *bez.Ctrl2X, *bez.Ctrl2Y)
	}
	if bez.FromX == nil || *bez.FromX != -0.604 || bez.ToY == nil || *bez.ToY != 22.169 {
		t.Errorf("endpoints did not arrive: %+v", bez)
	}
	straight := edgeByEnds(full.SceneEdges, "CP51", "LM54")
	if straight == nil {
		t.Fatal("CP51->LM54 missing from the edge set")
	}
	if straight.Ctrl1X != nil || straight.Ctrl1Y != nil || straight.Ctrl2X != nil || straight.Ctrl2Y != nil {
		t.Errorf("a StraightPath arrived with handles: %+v — a NULL column is nil, never a number", straight)
	}
	if straight.FromX == nil || straight.ToX == nil {
		t.Errorf("straight segment lost its endpoints: %+v", straight)
	}

	// The same revision quoted back: names still there, geometry gone, and
	// the revision restated so the Edge knows it is still current.
	same := nodeListReply(t, svc, resp, full.SceneRevision)
	if same.SceneRevision != full.SceneRevision {
		t.Errorf("revision changed between identical reads: %q then %q", full.SceneRevision, same.SceneRevision)
	}
	if len(same.ScenePoints) != len(full.ScenePoints) || len(same.SceneEdges) != len(full.SceneEdges) {
		t.Errorf("a matching revision dropped names: %d/%d points, %d/%d edges — names go every time",
			len(same.ScenePoints), len(full.ScenePoints), len(same.SceneEdges), len(full.SceneEdges))
	}
	if hasGeometry(same) {
		t.Error("a matching revision still carried geometry — the whole point of the revision is to leave it off")
	}
	if pointByName(same.ScenePoints, "PLN_ORIGIN") == nil || edgeByEnds(same.SceneEdges, "LM9", "PP224") == nil {
		t.Error("the name set on a matching revision is not the full name set")
	}

	// A stale revision is a cache Core cannot trust: full geometry again.
	stale := nodeListReply(t, svc, resp, "not-the-revision")
	if !hasGeometry(stale) || stale.SceneRevision != full.SceneRevision {
		t.Errorf("a stale revision must get the full scene back under the current revision; got geometry=%v rev=%q", hasGeometry(stale), stale.SceneRevision)
	}
}

// TestNodeListResponse_SceneRevisionTracksTheRows pins what the revision is
// made of: the rows. Re-reading gives the same revision; writing a row gives a
// different one, even when the write leaves the count and every name alone.
func TestNodeListResponse_SceneRevisionTracksTheRows(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sceneRequest(t, db)
	resp := &captureResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})

	r1 := nodeListReply(t, svc, resp, "")
	r2 := nodeListReply(t, svc, resp, "")
	if r1.SceneRevision != r2.SceneRevision {
		t.Fatalf("two reads of an unchanged scene disagree: %q vs %q — the revision must be a function of the rows, not of the read", r1.SceneRevision, r2.SceneRevision)
	}
	// Nudge one point by a centimetre. Same count, same names, new synced_at.
	testutil.MustNoErr(t, db.UpsertScenePoint(&scene.Point{
		AreaName: "AREA-G", InstanceName: "LM9", ClassName: "LocationMark",
		PosX: -0.614, PosY: 22.449, Dir: 1.5708, PropertiesJSON: "{}",
	}), "move LM9")
	r3 := nodeListReply(t, svc, resp, r1.SceneRevision)
	if r3.SceneRevision == r1.SceneRevision {
		t.Fatal("a moved point left the revision unchanged — an Edge quoting the old revision would keep drawing the old map")
	}
	if !hasGeometry(r3) {
		t.Error("the old revision was treated as current after the scene changed")
	}
	if lm := pointByName(r3.ScenePoints, "LM9"); lm == nil || lm.PosX == nil || *lm.PosX != -0.614 {
		t.Errorf("the moved coordinate did not arrive: %+v", lm)
	}
}

// TestNodeListResponse_SceneReadFailureKeepsTheNodeList keeps the existing
// degrade-on-error posture: a scene that cannot be read must not abort the
// node list, and must not mint a revision either — a revision on a partial
// scene would let the Edge believe it holds the whole one.
func TestNodeListResponse_SceneReadFailureKeepsTheNodeList(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sceneRequest(t, db)
	if _, err := db.Exec(`ALTER TABLE scene_edges RENAME TO scene_edges_hidden`); err != nil {
		t.Fatalf("hide scene_edges: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`ALTER TABLE scene_edges_hidden RENAME TO scene_edges`); err != nil {
			t.Fatalf("restore scene_edges: %v", err)
		}
	})
	resp := &captureResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})

	got := nodeListReply(t, svc, resp, "")
	if got.SceneRevision != "" {
		t.Errorf("a scene read that failed half-way minted revision %q", got.SceneRevision)
	}
	if len(got.SceneEdges) != 0 {
		t.Errorf("edges arrived from a table that does not exist: %d", len(got.SceneEdges))
	}
	if pointByName(got.ScenePoints, "PLN_ORIGIN") == nil {
		t.Error("the points that could be read were dropped with the edges that could not")
	}
}

// TestNodeListResponse_SceneBytesAtPlantScale measures what the revision
// buys: the node list with and without geometry at a whole plant's
// 350 points / 588 edges. Reported in the log for the build report; the
// assertion is only that the geometry is the bulk of the message, which is
// the premise the revision short-circuit rests on.
func TestNodeListResponse_SceneBytesAtPlantScale(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	fx := scenefixtures.B()
	for _, p := range fx.ScenePoints {
		testutil.MustNoErr(t, db.UpsertScenePoint(&scene.Point{
			AreaName: p.AreaName, InstanceName: p.InstanceName, ClassName: p.ClassName, PointName: p.PointName,
			Label: p.Label, PosX: p.PosX, PosY: p.PosY, Dir: p.Dir, PropertiesJSON: "{}",
		}), "upsert "+p.InstanceName)
	}
	for _, e := range fx.SceneEdges {
		testutil.MustNoErr(t, db.UpsertSceneEdge(&scene.Edge{
			AreaName: e.AreaName, InstanceName: e.InstanceName, ClassName: e.ClassName, FromName: e.FromName, ToName: e.ToName,
			FromX: e.FromX, FromY: e.FromY, ToX: e.ToX, ToY: e.ToY,
			Ctrl1X: e.Ctrl1X, Ctrl1Y: e.Ctrl1Y, Ctrl2X: e.Ctrl2X, Ctrl2Y: e.Ctrl2Y,
		}), "upsert "+e.InstanceName)
	}
	resp := &captureResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})

	full := nodeListReply(t, svc, resp, "")
	if len(full.ScenePoints) != len(fx.ScenePoints) || len(full.SceneEdges) != len(fx.SceneEdges) {
		t.Fatalf("fixture did not round-trip: %d/%d points, %d/%d edges", len(full.ScenePoints), len(fx.ScenePoints), len(full.SceneEdges), len(fx.SceneEdges))
	}
	names := nodeListReply(t, svc, resp, full.SceneRevision)

	fullJSON, err := json.Marshal(full)
	testutil.MustNoErr(t, err, "json.Marshal")
	namesJSON, err := json.Marshal(names)
	testutil.MustNoErr(t, err, "json.Marshal")
	src := protocol.Address{Role: protocol.RoleCore, Station: "core"}
	dst := protocol.Address{Role: protocol.RoleEdge, Station: "edge.geometry"}
	fullEnv, err := protocol.NewDataReply(protocol.SubjectNodeListResponse, src, dst, "req-1", full)
	testutil.MustNoErr(t, err, "build full reply")
	namesEnv, err := protocol.NewDataReply(protocol.SubjectNodeListResponse, src, dst, "req-2", names)
	testutil.MustNoErr(t, err, "build names reply")
	fullBytes, err := fullEnv.Encode()
	testutil.MustNoErr(t, err, "encode full")
	namesBytes, err := namesEnv.Encode()
	testutil.MustNoErr(t, err, "encode names")

	t.Logf("plant scale (%d points / %d edges): payload with geometry %d bytes, names only %d bytes; encoded envelope with geometry %d bytes, names only %d bytes",
		len(full.ScenePoints), len(full.SceneEdges), len(fullJSON), len(namesJSON), len(fullBytes), len(namesBytes))
	if len(fullJSON) <= len(namesJSON) {
		t.Errorf("geometry response (%d bytes) is not larger than the names-only one (%d bytes) — nothing was left off", len(fullJSON), len(namesJSON))
	}
	if hasGeometry(names) {
		t.Error("the names-only response at scale still carries geometry")
	}
}
