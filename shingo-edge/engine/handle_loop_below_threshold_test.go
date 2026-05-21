package engine

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// noOpOrderEmitter satisfies orders.EventEmitter without doing anything.
// Used in tests that drive the order-creation path but don't care about
// the resulting event side-effects.
type noOpOrderEmitter struct{}

func (noOpOrderEmitter) EmitOrderCreated(int64, string, protocol.OrderType, *int64, *int64) {
}
func (noOpOrderEmitter) EmitOrderStatusChanged(int64, string, protocol.OrderType, string, string, string, *int64, *int64) {
}
func (noOpOrderEmitter) EmitOrderCompleted(int64, string, protocol.OrderType, *int64, *int64) {
}
func (noOpOrderEmitter) EmitOrderDelivered(int64, string, protocol.OrderType, *int64, *int64) {
}
func (noOpOrderEmitter) EmitOrderFailed(int64, string, protocol.OrderType, string) {
}
func (noOpOrderEmitter) EmitOrderFaulted(int64, string, string) {
}

// TestHandleLoopBelowThreshold_FiresForInactiveStyleLoader pins Round-3
// Obs 9's Edge-side change: a threshold signal whose loader claim
// lives on an INACTIVE style must still trigger the L1 refill path.
// Pre-fix HandleLoopBelowThreshold used FindLoaderForPayload, which
// walks only proc.ActiveStyleID, so the signal silently dropped at
// Edge despite Core having emitted it.
//
// Fixture mirrors TestFindAnyLoaderClaimForPayload_InactiveStyle:
// loader claim for WIDGET-X lives on style NEW, but style OLD is
// active. The pre-fix code would log "no loader for payload=WIDGET-X
// — dropping signal"; the post-fix code logs "loop_threshold: signal
// received loader=CAL-LOADER" and proceeds into refillLoaderForPayload.
//
// We capture log output to assert the right path was taken without
// having to wire up Kafka/dispatch — refillLoaderForPayload's own log
// line is enough to prove HandleLoopBelowThreshold didn't short-circuit
// at the loader lookup.
func TestHandleLoopBelowThreshold_FiresForInactiveStyleLoader(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)

	processID, err := db.CreateProcess("OBS9-PROC", "obs9 test", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "CAL-LOADER",
		Code:         "CL1",
		Name:         "Cal Loader",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	_ = nodeID

	oldStyleID, err := db.CreateStyle("OLD", "old style", processID)
	if err != nil {
		t.Fatalf("create old style: %v", err)
	}
	newStyleID, err := db.CreateStyle("NEW", "new style", processID)
	if err != nil {
		t.Fatalf("create new style: %v", err)
	}
	// OLD is active. The loader claim for WIDGET-X is on NEW (inactive).
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &oldStyleID), "set active")
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:             newStyleID,
		CoreNodeName:        "CAL-LOADER",
		Role:                protocol.ClaimRoleProduce,
		SwapMode:            protocol.SwapModeManualSwap,
		PayloadCode:         "WIDGET-X",
		AllowedPayloadCodes: []string{"WIDGET-X"},
		UOPCapacity:         200,
		ReorderPoint:        2,
		InboundSource:       "EMPTY-SUPER",
		OutboundDestination: "FILLED-STORAGE",
	}); err != nil {
		t.Fatalf("upsert claim: %v", err)
	}

	var (
		mu   sync.Mutex
		logs []string
	)
	logFn := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, sprintf(format, args...))
	}
	debugFn := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, sprintf(format, args...))
	}

	eng := &Engine{
		db:       db,
		logFn:    logFn,
		debugFn:  debugFn,
		Events:   NewEventBus(),
		orderMgr: orders.NewManager(db, noOpOrderEmitter{}, "test-station"),
	}

	eng.HandleLoopBelowThreshold(&protocol.LoopBelowThresholdSignal{
		PayloadCode:  "WIDGET-X",
		CurrentUOP:   0,
		Threshold:    100,
		CoreNodeName: "CAL-LOADER",
		Reason:       "below_threshold",
	})

	mu.Lock()
	defer mu.Unlock()
	hasReceived := false
	hasDropped := false
	for _, line := range logs {
		if strings.Contains(line, "loop_threshold: signal received loader=CAL-LOADER") {
			hasReceived = true
		}
		if strings.Contains(line, "no loader for payload=WIDGET-X") {
			hasDropped = true
		}
	}
	if !hasReceived {
		t.Errorf("expected 'loop_threshold: signal received loader=CAL-LOADER' log line; got: %v", logs)
	}
	if hasDropped {
		t.Errorf("threshold signal must NOT be dropped on the no-loader check for an inactive-style loader; got drop log; full logs: %v", logs)
	}
}

// sprintf is a thin alias so the log-capture closures read cleanly.
func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }
