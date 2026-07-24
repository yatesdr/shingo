package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

func TestHandleUOPAdjustment_ValidUpdate(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false, false)
	sid, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "S"})
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:         pid,
		OperatorStationID: &sid,
		CoreNodeName:      "ALN_001",
		Enabled:           true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	binID := int64(42)
	db.UpdateProcessNodeUOP(nodeID, 500)
	db.SetProcessNodeRuntimeWithBin(nodeID, nil, &binID, 500)

	eng := testEngine(t, db)
	var received bool
	eng.Events.SubscribeTypes(func(evt Event) {
		if _, ok := evt.Payload.(UOPAdjustedEvent); ok {
			received = true
		}
	}, EventUOPAdjusted)

	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID:        binID,
		CoreNodeName: "ALN_001",
		NewRemaining: 800,
		Actor:        "admin",
	})

	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	if rt.RemainingUOPCached != 800 {
		t.Errorf("RemainingUOPCached = %d, want 800", rt.RemainingUOPCached)
	}
	if !received {
		t.Error("expected EventUOPAdjusted to be emitted")
	}
}

func TestHandleUOPAdjustment_MismatchedBin(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false, false)
	sid, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "S"})
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:         pid,
		OperatorStationID: &sid,
		CoreNodeName:      "ALN_002",
		Enabled:           true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	activeBinID := int64(42)
	wrongBinID := int64(99)
	db.UpdateProcessNodeUOP(nodeID, 500)
	db.SetProcessNodeRuntimeWithBin(nodeID, nil, &activeBinID, 500)

	eng := testEngine(t, db)
	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID:        wrongBinID,
		CoreNodeName: "ALN_002",
		NewRemaining: 800,
		Actor:        "admin",
	})

	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	if rt.RemainingUOPCached != 500 {
		t.Errorf("RemainingUOPCached = %d, want 500 (unchanged)", rt.RemainingUOPCached)
	}
}

func TestHandleUOPAdjustment_UnknownNode(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	var received bool
	eng.Events.SubscribeTypes(func(evt Event) {
		if _, ok := evt.Payload.(UOPAdjustedEvent); ok {
			received = true
		}
	}, EventUOPAdjusted)

	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID:        1,
		CoreNodeName: "NOEXIST",
		NewRemaining: 800,
		Actor:        "admin",
	})

	if received {
		t.Error("expected no EventUOPAdjusted for unknown node")
	}
}

// TestHandleUOPAdjustment_ReleasedClearsActiveBin pins the move-release path:
// a Released adjustment (Core moved the bin off this node) must clear the
// node's active_bin_id so its PLC ticks stop counting down a departed bin.
func TestHandleUOPAdjustment_ReleasedClearsActiveBin(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false, false)
	sid, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "S"})
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:         pid,
		OperatorStationID: &sid,
		CoreNodeName:      "ALN_003",
		Enabled:           true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err = db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	binID := int64(42)
	db.UpdateProcessNodeUOP(nodeID, 500)
	db.SetProcessNodeRuntimeWithBin(nodeID, nil, &binID, 500)

	eng := testEngine(t, db)
	var received bool
	eng.Events.SubscribeTypes(func(evt Event) {
		if _, ok := evt.Payload.(UOPAdjustedEvent); ok {
			received = true
		}
	}, EventUOPAdjusted)

	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID:        binID,
		CoreNodeName: "ALN_003",
		Released:     true,
		Actor:        "admin",
	})

	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	if rt.ActiveBinID != nil {
		t.Errorf("ActiveBinID = %v, want nil (bin moved away → released)", rt.ActiveBinID)
	}
	// P2-C5: release also blanks the cached count so the operator tile does not
	// show a dead number for the now-empty slot.
	if rt.RemainingUOPCached != 0 {
		t.Errorf("RemainingUOPCached = %d, want 0 (tile blanked on release, not left at the stale 500)", rt.RemainingUOPCached)
	}
	if !received {
		t.Error("expected EventUOPAdjusted (screen refresh) on release")
	}
}

// TestHandleUOPAdjustment_StagedUnboundBinBindsOnCorrection pins P2-C5: a count
// correction addressed to a node with NO bin bound binds the named bin with the
// corrected value (via the Bound=true machinery) instead of throwing the
// correction away as stale. This is the front door that repairs the SNF3
// detachment — a delivered bin that never bound, whose tile stranded.
func TestHandleUOPAdjustment_StagedUnboundBinBindsOnCorrection(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false, false)
	sid, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "S"})
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:         pid,
		OperatorStationID: &sid,
		CoreNodeName:      "ALN_006",
		Enabled:           true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err = db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	// No bin bound — the staged-but-unbound state (active_bin_id nil).

	eng := testEngine(t, db)
	var received bool
	eng.Events.SubscribeTypes(func(evt Event) {
		if _, ok := evt.Payload.(UOPAdjustedEvent); ok {
			received = true
		}
	}, EventUOPAdjusted)

	const binID int64 = 24 // CARRIER-0024 analog
	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID:        binID,
		CoreNodeName: "ALN_006",
		NewRemaining: 150,
		Epoch:        5,
		Actor:        "admin",
	})

	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	if rt.ActiveBinID == nil || *rt.ActiveBinID != binID {
		t.Errorf("ActiveBinID = %v, want %d (staged bin bound by the correction)", rt.ActiveBinID, binID)
	}
	if rt.RemainingUOPCached != 150 {
		t.Errorf("RemainingUOPCached = %d, want 150 (bound at the corrected value)", rt.RemainingUOPCached)
	}
	if rt.ActiveBinEpoch != 5 {
		t.Errorf("ActiveBinEpoch = %d, want 5 (bind seeds the epoch so resumed deltas are not dropped)", rt.ActiveBinEpoch)
	}
	if !received {
		t.Error("expected EventUOPAdjusted (screen refresh) on staged-bin bind")
	}
}

// TestHandleUOPAdjustment_CorrectionForBinBoundElsewhereRejected pins the other
// half of P2-C5: a correction naming a bin that a DIFFERENT bin already occupies
// at that node is still rejected — Edge never evicts the bin physically present.
func TestHandleUOPAdjustment_CorrectionForBinBoundElsewhereRejected(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false, false)
	sid, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "S"})
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:         pid,
		OperatorStationID: &sid,
		CoreNodeName:      "ALN_007",
		Enabled:           true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err = db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	presentBin := int64(70)
	db.UpdateProcessNodeUOP(nodeID, 300)
	db.SetProcessNodeRuntimeWithBin(nodeID, nil, &presentBin, 300)

	eng := testEngine(t, db)
	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID:        99, // a bin NOT at this node
		CoreNodeName: "ALN_007",
		NewRemaining: 150,
		Epoch:        5,
		Actor:        "admin",
	})

	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	if rt.ActiveBinID == nil || *rt.ActiveBinID != presentBin {
		t.Errorf("ActiveBinID = %v, want %d (present bin must not be evicted)", rt.ActiveBinID, presentBin)
	}
	if rt.RemainingUOPCached != 300 {
		t.Errorf("RemainingUOPCached = %d, want 300 (unchanged — correction was for a bin bound elsewhere)", rt.RemainingUOPCached)
	}
}

// TestHandleUOPAdjustment_BoundSetsActiveBin pins the move-bind path: a Bound
// adjustment (Core moved the bin ONTO this node) must bind the node's
// active_bin_id, epoch, and cached UOP so its PLC ticks resume counting the
// arrived bin — even when the destination was previously blank.
func TestHandleUOPAdjustment_BoundSetsActiveBin(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false, false)
	sid, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "S"})
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:         pid,
		OperatorStationID: &sid,
		CoreNodeName:      "ALN_004",
		Enabled:           true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err = db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	// Destination starts blank — no active bin (the fork-truck-recovered node).

	eng := testEngine(t, db)
	var received bool
	eng.Events.SubscribeTypes(func(evt Event) {
		if _, ok := evt.Payload.(UOPAdjustedEvent); ok {
			received = true
		}
	}, EventUOPAdjusted)

	binID := int64(77)
	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID:        binID,
		CoreNodeName: "ALN_004",
		NewRemaining: 640,
		Epoch:        9,
		Bound:        true,
		Actor:        "admin",
	})

	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	if rt.ActiveBinID == nil || *rt.ActiveBinID != binID {
		t.Errorf("ActiveBinID = %v, want %d (bin moved in → bound)", rt.ActiveBinID, binID)
	}
	if rt.RemainingUOPCached != 640 {
		t.Errorf("RemainingUOPCached = %d, want 640", rt.RemainingUOPCached)
	}
	if rt.ActiveBinEpoch != 9 {
		t.Errorf("ActiveBinEpoch = %d, want 9", rt.ActiveBinEpoch)
	}
	if !received {
		t.Error("expected EventUOPAdjusted (screen refresh) on bind")
	}
}

// TestHandleUOPAdjustment_BoundOverwritesStaleBin pins the unconditional-bind
// decision: Core's Move guarantees the destination held no other bin, so a
// Bound adjustment overwrites any stale active_bin_id rather than bailing the
// way the count-update / release paths do on a bin mismatch.
func TestHandleUOPAdjustment_BoundOverwritesStaleBin(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false, false)
	sid, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "S"})
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:         pid,
		OperatorStationID: &sid,
		CoreNodeName:      "ALN_005",
		Enabled:           true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err = db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	staleBinID := int64(11)
	db.UpdateProcessNodeUOP(nodeID, 100)
	db.SetProcessNodeRuntimeWithBin(nodeID, nil, &staleBinID, 100)

	eng := testEngine(t, db)
	movedBinID := int64(22)
	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID:        movedBinID,
		CoreNodeName: "ALN_005",
		NewRemaining: 480,
		Epoch:        3,
		Bound:        true,
		Actor:        "admin",
	})

	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	if rt.ActiveBinID == nil || *rt.ActiveBinID != movedBinID {
		t.Errorf("ActiveBinID = %v, want %d (bind overwrites stale pointer)", rt.ActiveBinID, movedBinID)
	}
	if rt.RemainingUOPCached != 480 {
		t.Errorf("RemainingUOPCached = %d, want 480", rt.RemainingUOPCached)
	}
}
