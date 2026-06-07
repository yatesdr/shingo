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
	if !received {
		t.Error("expected EventUOPAdjusted (screen refresh) on release")
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
