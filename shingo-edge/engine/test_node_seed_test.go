package engine

import (
	"fmt"
	"sync"
	"testing"

	"shingoedge/store"
	"shingoedge/store/processes"
)

// captureLogger is a thread-safe sink for log lines emitted by Engine
// during a test. Used by tests that assert on log content. Originally
// in uop_reconciler_test.go; preserved for the bucket-backfill tests.
type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (c *captureLogger) Log(format string, args ...interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func (c *captureLogger) Lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.lines))
	copy(out, c.lines)
	return out
}

// reconcilerTestEngine wires a *Engine bound to a fake Core HTTP
// endpoint and returns the engine plus a log capture sink. Originally
// in uop_reconciler_test.go; preserved for the bucket-backfill tests
// which still need the same wiring even after the reconciler removal.
func reconcilerTestEngine(t *testing.T, db *store.DB, mockCoreURL string) (*Engine, *captureLogger) {
	t.Helper()
	eng := testEngine(t, db)
	logger := &captureLogger{}
	eng.logFn = logger.Log
	eng.coreClient = NewCoreClient(mockCoreURL)
	return eng, logger
}

// seedReconcilerNode builds a minimal process+node+style+claim+runtime
// graph for tests that need a fully-set-up consume node. Originally
// lived in the now-deleted uop_reconciler_test.go; preserved here so
// the bucket-backfill, regression, and counter-delta tests that
// depend on it keep compiling.
func seedReconcilerNode(t *testing.T, db *store.DB, prefix, payloadCode string) (nodeID, styleID, claimID int64) {
	t.Helper()
	processID, err := db.CreateProcess(prefix+"-PROC", prefix+" rec", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: prefix + "-NODE",
		Code:         prefix[:3],
		Name:         prefix + " Node",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, err = db.CreateStyle(prefix+"-STYLE", prefix+" style", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	db.SetActiveStyle(processID, &styleID)
	claimID, err = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:      styleID,
		CoreNodeName: prefix + "-NODE",
		Role:         "consume",
		SwapMode:     "simple",
		PayloadCode:  payloadCode,
		UOPCapacity:  100,
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	if _, err := db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	return nodeID, styleID, claimID
}
