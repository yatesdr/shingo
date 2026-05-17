//go:build docker

package engine

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/config"
	"shingocore/fleet/simulator"
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

// setupThreeBinLine creates a line with 3 bins delivered and confirmed (claims released).
// This represents a line mid-operation: bins are physically there, orders are done.
// Returns the 3 bins, the storage node, the line node, and the payload.
func setupThreeBinLine(t *testing.T, db *store.DB) (bins [3]*bins.Bin, storageNode, lineNode *nodes.Node, bp *payloads.Payload) {
	t.Helper()
	storageNode, lineNode, bp = setupTestData(t, db)

	// Create a quality-hold node (another destination the operator might use)
	qhNode := &nodes.Node{Name: "QUALITY-HOLD-1", Zone: "Q", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(qhNode), "create QH node")

	// Create 3 bins at the line node (as if retrieve orders completed)
	for i := 0; i < 3; i++ {
		label := fmt.Sprintf("BIN-LINE-%d", i+1)
		bins[i] = createTestBinAtNode(t, db, bp.Code, lineNode.ID, label)
	}

	// Refresh bins so we have current state
	for i := 0; i < 3; i++ {
		var err error
		bins[i], err = db.GetBin(bins[i].ID)
		if err != nil {
			t.Fatalf("refresh bin %d: %v", i, err)
		}
	}

	return
}
