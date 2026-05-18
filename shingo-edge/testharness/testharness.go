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
	"shingoedge/messaging"
	"shingoedge/orders"
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
// the production reporter, and other subsystems integration scenarios
// don't exercise.
//
// Use the Ingestor as harness.EdgeSide.EdgeIngestor when constructing a
// bus. Call methods on Engine directly to drive the scenario
// (StartProcessChangeover, ReleaseChangeoverWait, etc). DB is the same
// store the engine reads/writes, exposed for direct seeding/assertion.
type Edge struct {
	Engine      *engine.Engine
	EdgeHandler *messaging.EdgeHandler
	Ingestor    *protocol.Ingestor
	DB          *store.DB
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
//   - Production reporter, count-group handler, backup service: not
//     touched by release-path code.
//
// AppConfig is minimal: namespace + line_id are used for station ID
// derivation and log strings. WarLink stays disabled (default zero-
// value), so plc.Manager.StartPolling polls an empty reporting-points
// list — a benign goroutine until eng.Stop closes its stop channel.
func NewEdge(t *testing.T, stationID string) *Edge {
	t.Helper()
	db := OpenDB(t)

	cfg := &config.Config{
		Namespace: "test",
		LineID:    "edge-test",
		Messaging: config.MessagingConfig{
			StationID: stationID,
		},
	}
	eng := engine.New(engine.Config{
		AppConfig: cfg,
		DB:        db,
		LogFunc:   t.Logf,
	})
	eng.Start()
	t.Cleanup(eng.Stop)

	edgeHandler := messaging.NewEdgeHandler(eng.OrderManager())

	// Mirror cmd/shingoedge/main.go's wiring for the subject handlers F'
	// Phase 2 scenarios actually exercise. Other subjects (catalog,
	// status snapshots, registered/register_request, count-group
	// commands) are deliberately left unwired — the SubjectRouter will
	// log "no handler registered" if a scenario produces one; scenarios
	// that need them can RegisterSubject on the router directly.
	subjectRouter := router.NewSubject()
	router.RegisterSubject(subjectRouter, protocol.SubjectNodeListResponse, func(_ *protocol.Envelope, resp *protocol.NodeListResponse) {
		eng.SetCoreNodes(resp.Nodes)
	})
	router.RegisterSubject(subjectRouter, protocol.SubjectBinPickedUp, func(_ *protocol.Envelope, bp *protocol.BinPickedUp) {
		eng.HandleBinPickedUp(bp.OrderUUID, bp.BinID)
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
	router.Register(protoRouter, protocol.TypeOrderStorageWaybill, func(*protocol.Envelope, *protocol.OrderStorageWaybill) {})
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
		Engine:      eng,
		EdgeHandler: edgeHandler,
		Ingestor:    ingestor,
		DB:          db,
	}
}

// Re-export commonly-needed types so integration callers don't have to
// import deeper Edge packages just for type declarations.
type (
	OrderManager = orders.Manager
)
