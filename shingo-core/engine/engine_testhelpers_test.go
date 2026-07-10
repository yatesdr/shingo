//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingocore/config"
	"shingocore/fleet"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// Test scaffolding for the engine package.
//
// Extracted from engine_test.go so the happy-path behavior file is
// not dominated by setup plumbing. Callers are engine_test.go,
// engine_regression_test.go, and any future engine-level test file.
// All functions here are thin wrappers that delegate to the shared
// internal/testdb package — engine-specific fixtures belong here,
// generic ones belong in internal/testdb.

func testDB(t *testing.T) *store.DB {
	return testdb.Open(t)
}

func setupTestData(t *testing.T, db *store.DB) (storageNode *nodes.Node, lineNode *nodes.Node, bp *payloads.Payload) {
	t.Helper()
	sd := testdb.SetupStandardData(t, db)
	return sd.StorageNode, sd.LineNode, sd.Payload
}

func createTestBinAtNode(t *testing.T, db *store.DB, payloadCode string, nodeID int64, label string) *bins.Bin {
	return testdb.CreateBinAtNode(t, db, payloadCode, nodeID, label)
}

func testEnvelope() *protocol.Envelope {
	return testdb.Envelope()
}

// newTestEngine constructs a real Engine wired to the test database and a
// fleet backend (simulator, or a test wrapper that embeds it). No Kafka, no
// HTTP server. Background goroutines tick harmlessly against the fleet.
// The engine is stopped automatically via t.Cleanup.
//
// The fleet parameter takes fleet.Backend (not *simulator.SimulatorBackend)
// so test wrappers like fakeOccupancyBackend can be injected at construction
// — replacing the field after eng.Start() would race the connection-health
// goroutine that reads e.fleet.
func newTestEngine(t *testing.T, db *store.DB, flt fleet.Backend) *Engine {
	t.Helper()
	cfg := config.Defaults()
	cfg.Messaging.StationID = "test-core"
	cfg.Messaging.DispatchTopic = "shingo.dispatch"

	eng := New(Config{
		AppConfig: cfg,
		DB:        db,
		Fleet:     flt,
		MsgClient: nil, // safe: checkConnectionStatus nil-guards msgClient
		LogFunc:   t.Logf,
	})
	eng.Start()
	t.Cleanup(func() { eng.Stop() })
	return eng
}
