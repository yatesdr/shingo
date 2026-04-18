//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingocore/config"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
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

func setupTestData(t *testing.T, db *store.DB) (storageNode *store.Node, lineNode *store.Node, bp *store.Payload) {
	t.Helper()
	sd := testdb.SetupStandardData(t, db)
	return sd.StorageNode, sd.LineNode, sd.Payload
}

func createTestBinAtNode(t *testing.T, db *store.DB, payloadCode string, nodeID int64, label string) *store.Bin {
	return testdb.CreateBinAtNode(t, db, payloadCode, nodeID, label)
}

func testEnvelope() *protocol.Envelope {
	return testdb.Envelope()
}

// newTestEngine constructs a real Engine wired to the test database and simulator.
// No Kafka, no HTTP server. Background goroutines tick harmlessly against the simulator.
// The engine is stopped automatically via t.Cleanup.
func newTestEngine(t *testing.T, db *store.DB, sim *simulator.SimulatorBackend) *Engine {
	t.Helper()
	cfg := config.Defaults()
	cfg.Messaging.StationID = "test-core"
	cfg.Messaging.DispatchTopic = "shingo.dispatch"

	eng := New(Config{
		AppConfig: cfg,
		DB:        db,
		Fleet:     sim,
		MsgClient: nil, // safe: checkConnectionStatus nil-guards msgClient
		LogFunc:   t.Logf,
	})
	eng.Start()
	t.Cleanup(func() { eng.Stop() })
	return eng
}
