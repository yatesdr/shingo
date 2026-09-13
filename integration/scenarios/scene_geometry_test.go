// End-to-end round trip of the scene geometry on the node-list sync: the real
// Core handler answers a real Edge request over the harness bus, and the
// Edge's cache is what the assertions read. The per-module tests each see one
// side; only this one can show that what Core leaves off is what the Edge
// keeps, and that the revision the Edge quotes back is the one Core minted.
//
//go:build docker

package scenarios

import (
	"testing"

	"shingo/integration/harness"
	"shingo/protocol"
	"shingo/protocol/router"

	coremessaging "shingocore/messaging"
	"shingocore/service"
	"shingocore/store/scene"
	coreharness "shingocore/testharness"

	edgeharness "shingoedge/testharness"
)

func fptr(v float64) *float64 { return &v }

func TestScenario_SceneGeometryRoundTripsAndTheRevisionShortCircuits(t *testing.T) {
	const stationID = "edge.geometry"

	// ── Core: a scene with the two shapes that matter, and the real handler ──
	coreDB := coreharness.OpenDB(t)
	// A bin location AT THE ORIGIN, the coordinate a bare float would drop.
	if err := coreDB.UpsertScenePoint(&scene.Point{
		AreaName: "AREA-G", InstanceName: "PLN_ORIGIN", ClassName: "GeneralLocation", PointName: "AP1",
		PosX: 0, PosY: 0, Dir: 0, PropertiesJSON: "{}",
	}); err != nil {
		t.Fatalf("upsert origin: %v", err)
	}
	if err := coreDB.UpsertScenePoint(&scene.Point{
		AreaName: "AREA-G", InstanceName: "LM9", ClassName: "LocationMark",
		PosX: -0.604, PosY: 22.449, Dir: 1.5708, PropertiesJSON: "{}",
	}); err != nil {
		t.Fatalf("upsert LM9: %v", err)
	}
	if err := coreDB.UpsertSceneEdge(&scene.Edge{
		AreaName: "AREA-G", InstanceName: "LM9-PP224", ClassName: "BezierPath", FromName: "LM9", ToName: "PP224",
		FromX: -0.604, FromY: 22.449, ToX: 0.986, ToY: 22.169,
		Ctrl1X: fptr(-0.287), Ctrl1Y: fptr(22.094), Ctrl2X: fptr(0.303), Ctrl2Y: fptr(22.142),
	}); err != nil {
		t.Fatalf("upsert bezier: %v", err)
	}
	if err := coreDB.UpsertSceneEdge(&scene.Edge{
		AreaName: "AREA-G", InstanceName: "CP51-LM54", ClassName: "StraightPath", FromName: "CP51", ToName: "LM54",
		FromX: -9.338, FromY: 6.23, ToX: -8.855, ToY: 6.221,
	}); err != nil {
		t.Fatalf("upsert straight: %v", err)
	}

	coreHandler := coremessaging.NewCoreHandler(coreDB, nil, "core", "shingo.dispatch", nil)
	svc := coremessaging.NewCoreDataService(coreDB, coreHandler, service.EpochAnnounce{})
	coreSubjects := router.NewSubject()
	router.RegisterSubject(coreSubjects, protocol.SubjectNodeListRequest, svc.HandleNodeListRequest)
	coreIngestor := protocol.NewIngestor(nil)
	coreRouter := router.New[string]()
	router.Register(coreRouter, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) {
		coreSubjects.Dispatch(env, p)
	})
	coreIngestor.Dispatch = func(env *protocol.Envelope) { coreRouter.Dispatch(env, env.Type) }

	// ── Edge: a real engine, with the response wired the way main.go wires it ──
	edge := edgeharness.NewEdge(t, stationID)
	var last *protocol.NodeListResponse
	edgeSubjects := router.NewSubject()
	router.RegisterSubject(edgeSubjects, protocol.SubjectNodeListResponse, func(_ *protocol.Envelope, resp *protocol.NodeListResponse) {
		last = resp
		edge.Engine.SetSceneGraph(resp.ScenePoints, resp.SceneEdges)
		edge.Engine.SetSceneGeometry(resp.SceneRevision, resp.ScenePoints, resp.SceneEdges)
	})
	edgeIngestor := protocol.NewIngestor(func(hdr *protocol.RawHeader) bool {
		return hdr.Dst.Station == stationID || hdr.Dst.Station == protocol.StationBroadcast
	})
	edgeRouter := router.New[string]()
	router.Register(edgeRouter, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) {
		edgeSubjects.Dispatch(env, p)
	})
	edgeIngestor.Dispatch = func(env *protocol.Envelope) { edgeRouter.Dispatch(env, env.Type) }

	bus := harness.NewBus(t,
		harness.EdgeSide{EdgeStore: edge.DB, EdgeIngestor: edgeIngestor},
		harness.CoreSide{CoreStore: coreDB, CoreIngestor: coreIngestor},
	)
	drainOutbox(t, edge)

	// request is the heartbeater's node-list request, built the same way
	// (protocol.NewDataEnvelope on SubjectNodeListRequest) with the revision
	// the caller chooses to quote.
	request := func(t *testing.T, rev string) *protocol.NodeListResponse {
		t.Helper()
		last = nil
		env, err := protocol.NewDataEnvelope(protocol.SubjectNodeListRequest,
			protocol.Address{Role: protocol.RoleEdge, Station: stationID},
			protocol.Address{Role: protocol.RoleCore},
			&protocol.NodeListRequest{SceneRevision: rev})
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		data, err := env.Encode()
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		if _, err := edge.DB.EnqueueOutbox(data, "data."+protocol.SubjectNodeListRequest); err != nil {
			t.Fatalf("enqueue request: %v", err)
		}
		bus.PumpAll()
		if last == nil {
			t.Fatal("no node.list_response reached the Edge")
		}
		return last
	}
	geometryOn := func(r *protocol.NodeListResponse) bool {
		for _, p := range r.ScenePoints {
			if p.PosX != nil {
				return true
			}
		}
		return false
	}

	// 1. A fresh Edge quotes nothing and gets everything.
	if got := edge.Engine.SceneRevision(); got != "" {
		t.Fatalf("fresh Edge already holds revision %q", got)
	}
	full := request(t, edge.Engine.SceneRevision())
	if full.SceneRevision == "" || !geometryOn(full) {
		t.Fatalf("first sync did not carry geometry under a revision: rev=%q geometry=%v", full.SceneRevision, geometryOn(full))
	}
	if got := edge.Engine.SceneRevision(); got != full.SceneRevision {
		t.Fatalf("Edge holds revision %q after caching, Core sent %q", got, full.SceneRevision)
	}
	g := edge.Engine.SceneGeometry()
	if g == nil {
		t.Fatal("geometry not cached on the Edge")
	}
	origin, ok := g.Points["PLN_ORIGIN"]
	if !ok || origin.X != 0 || origin.Y != 0 || origin.ClassName != "GeneralLocation" {
		t.Errorf("origin cached as %+v, ok=%v — (0,0) must survive the whole trip", origin, ok)
	}
	var bezHandles *[4]float64
	straightHandles := "unseen"
	for _, e := range g.Edges {
		switch e.From {
		case "LM9":
			bezHandles = e.Handles
		case "CP51":
			if e.Handles == nil {
				straightHandles = "nil"
			} else {
				straightHandles = "present"
			}
		}
	}
	if bezHandles == nil || bezHandles[0] != -0.287 || bezHandles[3] != 22.142 {
		t.Errorf("bezier handles on the Edge = %v, want [-0.287 22.094 0.303 22.142]", bezHandles)
	}
	if straightHandles != "nil" {
		t.Errorf("straight segment on the Edge has handles %s, want nil", straightHandles)
	}
	if names := edge.Engine.ScenePointNames(); !names["PLN_ORIGIN"] || !names["LM9"] {
		t.Errorf("validator name set = %v after a full sync", names)
	}

	// 2. The next tick quotes the cached revision: names, no geometry, same
	//    revision, cache untouched.
	same := request(t, edge.Engine.SceneRevision())
	if same.SceneRevision != full.SceneRevision {
		t.Errorf("revision moved with no scene change: %q -> %q", full.SceneRevision, same.SceneRevision)
	}
	if geometryOn(same) {
		t.Error("a matching revision still shipped geometry")
	}
	if len(same.ScenePoints) != len(full.ScenePoints) || len(same.SceneEdges) != len(full.SceneEdges) {
		t.Errorf("names dropped on the short-circuit: %d/%d points, %d/%d edges", len(same.ScenePoints), len(full.ScenePoints), len(same.SceneEdges), len(full.SceneEdges))
	}
	if edge.Engine.SceneRevision() != full.SceneRevision || edge.Engine.SceneGeometry().Points["PLN_ORIGIN"].ClassName != "GeneralLocation" {
		t.Error("the name-only response disturbed the Edge cache")
	}
	if names := edge.Engine.ScenePointNames(); !names["PLN_ORIGIN"] {
		t.Error("the validator name set was not refreshed by the name-only response")
	}

	// 3. A stale revision gets the whole scene back.
	stale := request(t, "some-revision-from-last-week")
	if !geometryOn(stale) || stale.SceneRevision != full.SceneRevision {
		t.Errorf("stale revision: geometry=%v rev=%q", geometryOn(stale), stale.SceneRevision)
	}

	// 4. An older Edge binary sends {} — no field at all — and gets the whole
	//    scene, same as a fresh one.
	older := request(t, "")
	if !geometryOn(older) || older.SceneRevision != full.SceneRevision {
		t.Errorf("no-revision request: geometry=%v rev=%q", geometryOn(older), older.SceneRevision)
	}
}
