// What one Edge boot costs on the node list and the catalog against a Core
// that answers feeds: the asks the Edge sends, the values Core sends back, and
// the identical catalogs the Edge rewrites. The real Core handlers answer over
// the harness bus, a real Edge engine applies what comes back, and the Edge's
// own StartupReconcile runs after the burst as main.go runs it (orders only:
// TestCoreSyncService_StartupReconcileAsksOnlyForOrders); the boot burst is
// sent as messaging.Heartbeater.SendBootBurst sends it (register, node list,
// catalog, bins watch, heartbeat: TestHeartbeater_TheBootBurstEndsWithAHeartbeat), and
// the registered reply asks nothing inside an Edge's first 30 s
// (TestBootAsks_TheRegisteredReplyAsksOnlyPastThirtySeconds), so it is not
// routed here.
//
//go:build docker

package scenarios

import (
	"sync"
	"testing"

	"shingo/integration/harness"
	"shingo/protocol"
	"shingo/protocol/router"

	coremessaging "shingocore/messaging"
	"shingocore/service"
	coreharness "shingocore/testharness"

	"shingoedge/config"
	edgeengine "shingoedge/engine"
	"shingoedge/store/catalog"
	edgeharness "shingoedge/testharness"
)

// bootCount is what one boot moved, counted at each side's door.
type bootCount struct {
	nodeAsks, catalogAsks int // node.list_request, catalog.payloads_request reaching Core
	nodeLists, catalogs   int // node.list_response, catalog.payloads_response reaching the Edge
	// catalogRewrites: catalog values that carried the digest the Edge already
	// held and still wrote payload_catalog (catalog.Generation moved).
	catalogRewrites int
}

// Two boots of one Edge: the first ever (an empty database) and a restart on
// the same database, which reloads the catalog digest it persisted. Each is
// followed by one heartbeat quoting what the Edge then holds, which must move
// nothing: the boot is over.
//
// StartupReconcile asks for no node list or catalog, so the boot burst's are
// the only asks, and a catalog that carries the digest already held writes
// nothing (engine ApplyCatalog), so no identical catalog is rewritten.
//
// Not parallel: catalog.Generation is process-wide, and another scenario's
// Edge writing its catalog meanwhile would read as a rewrite here.
func TestScenario_OneEdgeBootAgainstAFeedsCore_NodeListAndCatalogCounts(t *testing.T) {
	const station = "edge.boot"

	coreDB := coreharness.OpenDB(t)
	coreharness.SetupStandardData(t, coreDB) // nodes and one payload: a node list and a catalog to send
	coreHandler := coremessaging.NewCoreHandler(coreDB, nil, "core", "shingo.dispatch", nil)
	svc := coremessaging.NewCoreDataService(coreDB, coreHandler, service.EpochAnnounce{})

	var mu sync.Mutex
	var n bootCount
	count := func(f func(*bootCount)) {
		mu.Lock()
		f(&n)
		mu.Unlock()
	}

	// Core: the subjects one boot sends, as cmd/shingocore buildSubjectRouter
	// registers them, the two asks counted on the way in.
	coreSubjects := router.NewSubject()
	router.RegisterSubject(coreSubjects, protocol.SubjectEdgeRegister, svc.HandleEdgeRegister)
	router.RegisterSubject(coreSubjects, protocol.SubjectEdgeHeartbeat, svc.HandleEdgeHeartbeat)
	router.RegisterSubject(coreSubjects, protocol.SubjectNodeListRequest, func(env *protocol.Envelope, req *protocol.NodeListRequest) {
		count(func(c *bootCount) { c.nodeAsks++ })
		svc.HandleNodeListRequest(env, req)
	})
	router.RegisterSubjectBare(coreSubjects, protocol.SubjectCatalogPayloadsRequest, func(env *protocol.Envelope) {
		count(func(c *bootCount) { c.catalogAsks++ })
		svc.HandleCatalogPayloadsRequest(env)
	})
	router.RegisterSubject(coreSubjects, protocol.SubjectOrderStatusRequest, svc.HandleOrderStatusRequest)
	router.RegisterSubject(coreSubjects, protocol.SubjectBinsWatch, svc.HandleBinWatch)
	coreRouter := router.New[string]()
	router.Register(coreRouter, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) { coreSubjects.Dispatch(env, p) })
	coreIngestor := protocol.NewIngestor(nil)
	coreIngestor.Dispatch = func(env *protocol.Envelope) { coreRouter.Dispatch(env, env.Type) }

	edgeDB := edgeharness.OpenDB(t)

	// boot starts an Edge engine on edgeDB, sends what a booting Edge sends, lets
	// every reply land, then sends one more heartbeat. It returns what the boot
	// moved and what that heartbeat moved, and the engine (stopped by stop).
	boot := func(t *testing.T) (atBoot, after bootCount, eng *edgeengine.Engine, stop func()) {
		t.Helper()
		cfg := &config.Config{StationUID: station}
		eng = edgeengine.New(edgeengine.Config{AppConfig: cfg, DB: edgeDB, LogFunc: func(string, ...any) {}})
		eng.Start()
		var once sync.Once
		stop = func() { once.Do(eng.Stop) }
		t.Cleanup(stop)

		// The Edge's inbound doors for the two values and the ack, as
		// cmd/shingoedge buildInboundRouter routes them, counted on the way in.
		edgeSubjects := router.NewSubject()
		router.RegisterSubject(edgeSubjects, protocol.SubjectNodeListResponse, func(_ *protocol.Envelope, resp *protocol.NodeListResponse) {
			count(func(c *bootCount) { c.nodeLists++ })
			eng.ApplyNodeList(resp)
		})
		router.RegisterSubject(edgeSubjects, protocol.SubjectCatalogPayloadsResponse, func(_ *protocol.Envelope, resp *protocol.CatalogPayloadsResponse) {
			held := eng.FeedDigests()[protocol.FeedCatalog]
			gen := catalog.Generation()
			eng.ApplyCatalog(resp)
			rewrote := resp.Digest != "" && resp.Digest == held && catalog.Generation() != gen
			count(func(c *bootCount) {
				c.catalogs++
				if rewrote {
					c.catalogRewrites++
				}
			})
		})
		router.RegisterSubject(edgeSubjects, protocol.SubjectEdgeHeartbeatAck, func(_ *protocol.Envelope, ack *protocol.EdgeHeartbeatAck) {
			eng.OnCoreAck(ack)
		})
		edgeRouter := router.New[string]()
		router.Register(edgeRouter, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) { edgeSubjects.Dispatch(env, p) })
		edgeIngestor := protocol.NewIngestor(func(hdr *protocol.RawHeader) bool {
			return hdr.Dst.Station == station || hdr.Dst.Station == protocol.StationBroadcast
		})
		edgeIngestor.Dispatch = func(env *protocol.Envelope) { edgeRouter.Dispatch(env, env.Type) }
		bus := harness.NewBus(t,
			harness.EdgeSide{EdgeStore: edgeDB, EdgeIngestor: edgeIngestor},
			harness.CoreSide{CoreStore: coreDB, CoreIngestor: coreIngestor},
		)

		// The Edge's sends, through its outbox to the bus: the heartbeater's
		// one-shots and the engine's sendFn alike.
		enqueue := func(env *protocol.Envelope) error {
			data, err := env.Encode()
			if err != nil {
				return err
			}
			_, err = edgeDB.EnqueueOutbox(data, "data.boot")
			return err
		}
		send := func(subject string, body any) {
			t.Helper()
			env, err := protocol.NewDataEnvelope(subject,
				protocol.Address{Role: protocol.RoleEdge, Station: station}, protocol.Address{Role: protocol.RoleCore}, body)
			mustNil(t, err, "build "+subject)
			mustNil(t, enqueue(env), "enqueue "+subject)
		}
		eng.SetSendFunc(enqueue)
		// As cmd/shingoedge main.go wires them: the engine's two resyncs are
		// the heartbeater's requests.
		eng.SetNodeSyncFunc(func() {
			send(protocol.SubjectNodeListRequest, &protocol.NodeListRequest{SceneRevision: eng.SceneRevision()})
		})
		eng.SetCatalogSyncFunc(func() { send(protocol.SubjectCatalogPayloadsRequest, &protocol.CatalogPayloadsRequest{}) })

		mu.Lock()
		n = bootCount{}
		mu.Unlock()
		// The boot burst, in SendBootBurst's order.
		send(protocol.SubjectEdgeRegister, &protocol.EdgeRegister{StationID: station, Hostname: "boot-host", Version: "v-test"})
		send(protocol.SubjectNodeListRequest, &protocol.NodeListRequest{SceneRevision: eng.SceneRevision()})
		send(protocol.SubjectCatalogPayloadsRequest, &protocol.CatalogPayloadsRequest{})
		eng.PublishBinWatchAtBoot(func(l *protocol.BinWatchList) error { send(protocol.SubjectBinsWatch, l); return nil })
		send(protocol.SubjectEdgeHeartbeat, &protocol.EdgeHeartbeat{StationID: station, Feeds: eng.FeedDigests()})
		// Then main.go's StartupReconcile, after the burst.
		mustNil(t, eng.StartupReconcile(), "StartupReconcile")
		bus.PumpAll()
		mu.Lock()
		atBoot, n = n, bootCount{}
		mu.Unlock()

		if !eng.CoreSpeaksFeeds() {
			t.Fatal("no ack from a Core that answers feeds reached the Edge: the boot was not against a feeds Core")
		}
		send(protocol.SubjectEdgeHeartbeat, &protocol.EdgeHeartbeat{StationID: station, Feeds: eng.FeedDigests()})
		bus.PumpAll()
		mu.Lock()
		after = n
		mu.Unlock()
		return atBoot, after, eng, stop
	}

	cases := []struct {
		name string
		want bootCount
	}{
		// Node list: the burst's reply and the feed the burst's heartbeat draws
		// (the Edge quotes no node-list digest at boot: it is held in memory
		// only). Catalog: the reply and the feed, which carries the digest the
		// reply left held and so writes nothing.
		{"first boot, empty database", bootCount{nodeAsks: 1, catalogAsks: 1, nodeLists: 2, catalogs: 2, catalogRewrites: 0}},
		// The persisted catalog digest is Core's, so no catalog feed; the
		// reply carries it and writes nothing.
		{"restart on the same database", bootCount{nodeAsks: 1, catalogAsks: 1, nodeLists: 2, catalogs: 1, catalogRewrites: 0}},
	}
	for _, tc := range cases {
		atBoot, after, eng, stop := boot(t)
		if atBoot != tc.want {
			t.Errorf("%s: the boot moved %+v, want %+v (if lane D2 has landed, record its numbers in PREDICTIONS.md and update want)",
				tc.name, atBoot, tc.want)
		}
		if after != (bootCount{}) {
			t.Errorf("%s: the heartbeat after the boot moved %+v, want nothing: the Edge holds what Core holds", tc.name, after)
		}
		if got := eng.FeedDigests()[protocol.FeedCatalog]; got == "" {
			t.Errorf("%s: the Edge holds no catalog digest after the boot", tc.name)
		}
		stop()
	}
}
