// Package testharness exposes Core test fixtures across module
// boundaries. The integration module (shingo/integration) cannot import
// shingo-core's `internal/testdb` package — Go's internal-package rules
// are enforced at the module level. This wrapper re-exports the bits
// the integration harness needs, in a publicly-importable location.
//
// Production code MUST NOT import this package. It pulls in
// testcontainers and the Postgres driver as transitive dependencies,
// neither of which has any business in a production binary.
//
// Cross-module callers: import as
//
//	import coreharness "shingocore/testharness"
//
// then `coreharness.OpenDB(t)` etc. exactly as if the integration
// caller were a Core internal test.
package testharness

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// OpenDB starts (or reuses, via sync.Once) the shared Postgres
// testcontainer and returns a *store.DB connected to a fresh database.
// Skips the test cleanly when Docker isn't available. Mirrors
// internal/testdb.Open exactly — see that doc for details.
func OpenDB(t *testing.T) *store.DB {
	t.Helper()
	return testdb.Open(t)
}

// StandardData mirrors internal/testdb.StandardData. Re-exported so
// cross-module callers don't need to know the wrapped type.
type StandardData = testdb.StandardData

// SetupStandardData creates the standard fixture: one storage node
// (STORAGE-A1, zone A), one line node (LINE1-IN), one payload (PART-A),
// one bin type (DEFAULT). Used by integration tests that don't need a
// custom fixture.
func SetupStandardData(t *testing.T, db *store.DB) *StandardData {
	t.Helper()
	return testdb.SetupStandardData(t, db)
}

// CreateBinAtNode seeds a confirmed-manifest bin at a given node, ready
// to be claimed and dispatched. Wraps the internal testdb helper.
func CreateBinAtNode(t *testing.T, db *store.DB, payloadCode string, nodeID int64, label string) *bins.Bin {
	t.Helper()
	return testdb.CreateBinAtNode(t, db, payloadCode, nodeID, label)
}

// ClaimBinForTest reserves then claims binID for orderID via the production
// reserve→claim→confirm path (a bare db.ClaimBin fails the demoted-CAS seatbelt,
// which requires a pending reservation). orderID must reference a real order.
// Wraps the internal testdb helper so cross-module integration scenarios can
// establish a claimed-bin precondition without importing internal/testdb.
func ClaimBinForTest(t *testing.T, db *store.DB, binID, orderID int64) {
	t.Helper()
	testdb.ClaimBinForTest(t, db, binID, orderID)
}

// MockBackend is the fleet.Backend stub used by Core dispatch tests.
// Re-exported so the integration harness can construct dispatchers
// without touching internal/testdb directly.
type MockBackend = testdb.MockBackend

// NewTrackingBackend returns a MockBackend that records every fleet
// call (create, release, cancel) for assertion. All calls succeed.
func NewTrackingBackend() *testdb.MockTrackingBackend {
	return testdb.NewTrackingBackend()
}

// (Envelope helper not re-exported — tests build their own via
// protocol.NewEnvelope so the src/dst addresses are explicit at the
// scenario site.)

// Re-export the underlying types so callers don't import store/{bins,nodes,payloads}
// transitively just to declare variables. These aliases keep the harness API surface
// self-contained.
type (
	Bin     = bins.Bin
	Node    = nodes.Node
	Payload = payloads.Payload
)
