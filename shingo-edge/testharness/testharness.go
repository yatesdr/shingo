// Package testharness exposes Edge test fixtures across module
// boundaries. Same rationale as shingocore/testharness — used by the
// integration module to construct a real Edge engine + DB without
// duplicating setup boilerplate.
//
// Production code MUST NOT import this package.
package testharness

import (
	"path/filepath"
	"testing"

	"shingo/protocol"
	"shingo/protocol/router"

	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/engine/releasefake"
	"shingoedge/messaging"
	"shingoedge/service"
	"shingoedge/store"
)

// OpenDB opens a fresh SQLite DB in a t.TempDir() with all migrations
// applied. The database is closed and removed via t.Cleanup. Mirrors
// the per-test isolation Core gets from testcontainers.
func OpenDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Edge bundles a running Edge engine with the message-handler and
// ingestor layers wired the same way cmd/shingoedge/main.go wires them
// in production — minus Kafka, the outbox drainer, the heartbeater,
// and other subsystems integration scenarios don't exercise.
//
// Use the Ingestor as harness.EdgeSide.EdgeIngestor when constructing a
// bus. Call methods on Engine directly to drive the scenario
// (StartProcessChangeover, ReleaseChangeoverWait, etc). DB is the same
// store the engine reads/writes, exposed for direct seeding/assertion.
type Edge struct {
	Engine   *engine.Engine
	Ingestor *protocol.Ingestor
	DB       *store.DB
}

// NewEdge constructs a real Edge engine + EdgeHandler + Ingestor with
// the message handlers needed for release-path scenarios wired (notably
// the SubjectBinPickedUp subject-router entry — load-bearing for F'
// Phase 2's deferred-supply chain). Engine.Start() is called;
// engine.Stop() is registered with t.Cleanup.
//
// stationID becomes the ingestor's destination filter — envelopes with
// Dst.Station == stationID (or StationBroadcast) get delivered. Use the
// same value when constructing OrderRelease envelopes from the Core side
// to round-trip cleanly.
//
// Subsystems intentionally NOT wired (and the rationale):
//   - Kafka client, outbox drainer: harness.Bus + pump.go substitute.
//   - Heartbeater: emits register / heartbeat envelopes Core ignores in
//     scenarios; adds noise without coverage.
//   - InventoryDeltaReporter: HandleBinPickedUp's flush call already
//     guards on nil. Set explicitly via SetInventoryDeltaSink if a
//     scenario needs delta accumulation tested.
//   - Backup service: not touched by release-path code.
//
// AppConfig is minimal: namespace + line_id are used for station ID
// derivation and log strings. WarLink stays disabled (default zero-
// value), so plc.Manager.StartPolling polls an empty reporting-points
// list — a benign goroutine until eng.Stop closes its stop channel.
func NewEdge(t *testing.T, stationID string) *Edge {
	t.Helper()
	db := OpenDB(t)

	// StationID is set explicitly on both fields: the harness hands a station
	// id in, and nothing derives one from namespace/line_id any more.
	cfg := &config.Config{
		StationUID: stationID,
		Namespace:  "test",
		LineID:     "edge-test",
		Messaging: config.MessagingConfig{
			StationID: stationID,
		},
	}
	eng := engine.New(engine.Config{
		AppConfig: cfg,
		DB:        db,
		LogFunc:   t.Logf,
	})
	// No Core answers the read-asks here unless a scenario wires them, so the
	// release act's point is answered from the Edge's own rows, as Core's rule
	// reads them.
	eng.SetReleasePointSource(releasefake.DB{DB: db})
	// Nor any Core to ask the changeover preflight: every payload reads
	// stocked, as a scenario's fully stocked Core answered it over HTTP.
	eng.SetPreflightSource(stockedPreflight{})
	eng.Start()
	t.Cleanup(eng.Stop)

	edgeHandler := messaging.NewEdgeHandler(eng.OrderManager())

	// Mirror cmd/shingoedge/main.go's wiring for the subject handlers F'
	// Phase 2 scenarios actually exercise. Other subjects (catalog,
	// status snapshots, registered/register_request) are deliberately
	// left unwired — the SubjectRouter will
	// log "no handler registered" if a scenario produces one; scenarios
	// that need them can RegisterSubject on the router directly.
	subjectRouter := router.NewSubject()
	router.RegisterSubject(subjectRouter, protocol.SubjectNodeListResponse, func(_ *protocol.Envelope, resp *protocol.NodeListResponse) {
		eng.SetCoreNodes(resp.Nodes)
	})
	router.RegisterSubject(subjectRouter, protocol.SubjectBinPickedUp, func(_ *protocol.Envelope, bp *protocol.BinPickedUp) {
		eng.HandleBinPickedUpMessage(*bp)
	})
	// The landing (Lane H): the one door that binds a carrier Core placed.
	router.RegisterSubject(subjectRouter, protocol.SubjectBinLanded, func(_ *protocol.Envelope, bl *protocol.BinLanded) {
		eng.HandleBinLanded(*bl)
	})
	router.RegisterSubject(subjectRouter, protocol.SubjectBinReturn, func(_ *protocol.Envelope, br *protocol.BinReturn) {
		eng.HandleBinReturn(*br)
	})
	// Core's answers to this Edge's asks, as the production composition root
	// routes them (cmd/shingoedge/main.go): applied by kind, handed to a waiter.
	router.RegisterSubject(subjectRouter, protocol.SubjectEdgeAnswer, eng.HandleEdgeAnswer)
	// Core's sourceability feed, as the production composition root routes it
	// (cmd/shingoedge/main.go). Without it a cross-module scenario could not see
	// what the per-station feed does to an Edge's cache.
	router.RegisterSubject(subjectRouter, protocol.SubjectSourcingState, func(_ *protocol.Envelope, r *protocol.SourcingStateReport) {
		eng.HandleSourcingState(*r)
	})

	ingestor := protocol.NewIngestor(func(hdr *protocol.RawHeader) bool {
		return hdr.Dst.Station == stationID || hdr.Dst.Station == protocol.StationBroadcast
	})
	protoRouter := router.New[string]()
	router.Register(protoRouter, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) {
		subjectRouter.Dispatch(env, p)
	})
	// Outbound-only order-channel types: registered as no-ops so the
	// router is comprehensive (mirrors production composition root).
	router.Register(protoRouter, protocol.TypeOrderRequest, func(*protocol.Envelope, *protocol.OrderRequest) {})
	router.Register(protoRouter, protocol.TypeOrderCancel, func(*protocol.Envelope, *protocol.OrderCancel) {})
	router.Register(protoRouter, protocol.TypeOrderReceipt, func(*protocol.Envelope, *protocol.OrderReceipt) {})
	router.Register(protoRouter, protocol.TypeOrderRedirect, func(*protocol.Envelope, *protocol.OrderRedirect) {})
	router.Register(protoRouter, protocol.TypeComplexOrderRequest, func(*protocol.Envelope, *protocol.ComplexOrderRequest) {})
	router.Register(protoRouter, protocol.TypeOrderRelease, func(*protocol.Envelope, *protocol.OrderRelease) {})
	router.Register(protoRouter, protocol.TypeOrderIngest, func(*protocol.Envelope, *protocol.OrderIngestRequest) {})
	router.Register(protoRouter, protocol.TypeOrderAck, edgeHandler.HandleOrderAck)
	router.Register(protoRouter, protocol.TypeOrderWaybill, edgeHandler.HandleOrderWaybill)
	router.Register(protoRouter, protocol.TypeOrderUpdate, edgeHandler.HandleOrderUpdate)
	router.Register(protoRouter, protocol.TypeOrderDelivered, edgeHandler.HandleOrderDelivered)
	router.Register(protoRouter, protocol.TypeOrderError, edgeHandler.HandleOrderError)
	router.Register(protoRouter, protocol.TypeOrderCancelled, edgeHandler.HandleOrderCancelled)
	router.Register(protoRouter, protocol.TypeOrderStaged, edgeHandler.HandleOrderStaged)
	router.Register(protoRouter, protocol.TypeOrderSkipped, edgeHandler.HandleOrderSkipped)
	ingestor.Dispatch = func(env *protocol.Envelope) {
		protoRouter.Dispatch(env, env.Type)
	}

	return &Edge{
		Engine:   eng,
		Ingestor: ingestor,
		DB:       db,
	}
}

// stockedPreflight is the changeover preflight with no Core to ask: every
// payload has a free bin, and which have none anywhere is not known.
type stockedPreflight struct{}

func (stockedPreflight) Available() bool { return true }

func (stockedPreflight) PreflightInventory([]string) (*service.PreflightCoreResult, error) {
	return &service.PreflightCoreResult{Missing: []string{}}, nil
}
