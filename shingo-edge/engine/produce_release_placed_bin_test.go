package engine

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// produce_release_placed_bin_test.go — sim 2026-09-07, PLN_001 press death.
//
// Under two_robot_press_index the index leg auto-dispatches at creation and
// binds its bin through the delivery handler, so by the time the operator
// taps RELEASE the slot's active bin can be the one the press is filling
// INTO — not the departing one. The unconditional clear in
// produceIngestAtRelease erased exactly that bin; nothing rebinds it (the
// anti-ghost guard and bin_epoch_refresh both decline by design) and
// SimMachineReady gates the machine off permanently. These tests pin the
// gate: the clear fires only when the bound bin is not the one the placing
// leg delivered.

// seedPressIndexProduceNode seeds a produce claim in two_robot_press_index
// mode whose swap legs RequestProduceSwap can mint.
func seedPressIndexProduceNode(t *testing.T, db *store.DB) (nodeID int64) {
	t.Helper()
	processID, err := db.CreateProcess("PRESS-PROC", "press test", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "PRESS-FRONT",
		Code:         "PF1",
		Name:         "Press Front",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create process node: %v", err)
	}
	styleID, err := db.CreateStyle("PRESS-STYLE", "press style", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &styleID), "set active style")

	_, err = upsertClaimLegacySimple(db, processes.NodeClaimInput{
		StyleID:             styleID,
		CoreNodeName:        "PRESS-FRONT",
		Role:                "produce",
		SwapMode:            protocol.SwapModeTwoRobotPressIndex,
		PayloadCode:         "PANEL-X",
		UOPCapacity:         100,
		AutoReorder:         domain.Ptr(true),
		InboundSource:       "EMPTY-STORAGE",
		OutboundDestination: "FILLED-STORAGE",
		AutoRequestPayload:  "PANEL-X",
		PairedCoreNode:      "PRESS-BACK",
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	// A counted cell: BuildProducePlan refuses a press with no parts
	// ("no parts to finalize"), and the 09-07 press was mid-production
	// when the operator clicked.
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, nil, 6), "seed count")
	return nodeID
}

// placingLegID returns the id of the leg whose steps place a bin at the
// press front — the same discriminator the release classifier uses
// (legPlacesBinAt), not a hand-rolled dropoff scan. Both legs exist by
// construction (RequestProduceSwap minted the pair).
func placingLegID(t *testing.T, eng *Engine, result *NodeOrderResult) int64 {
	t.Helper()
	for _, ord := range []*storeorders.Order{result.OrderA, result.OrderB} {
		if ord == nil {
			continue
		}
		stepsJSON, err := eng.db.GetOrderStepsJSON(ord.ID)
		testutil.MustNoErr(t, err, "read steps")
		steps, err := decodeSteps(stepsJSON)
		testutil.MustNoErr(t, err, "decode steps")
		if legPlacesBinAt(steps, "PRESS-FRONT") {
			return ord.ID
		}
	}
	t.Fatal("no leg places a bin at PRESS-FRONT — the press-index plan shape changed")
	return 0
}

// The 09-07 replay: the index leg delivered its bin and the delivery
// handler bound it; the press has ticked parts into it; the operator taps
// RELEASE. The manifest still stamps (the count is real) but the slot must
// KEEP its binding — the bin is standing on the press.
func TestProduceRelease_PlacedBinIsNotCleared(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID := seedPressIndexProduceNode(t, db)
	eng := testEngine(t, db)

	result, err := eng.RequestProduceSwap(nodeID)
	testutil.MustNoErr(t, err, "RequestProduceSwap")

	// The 09-07 shape: index leg delivered+bound, evac still staged. Both
	// staged also exercises the guard; the delivery-shaped state is what
	// matters for the gate, so mirror it by binding the placed bin while
	// the evac leg sits at staged (markStaged) — the same statuses the run
	// had at click 4.
	placed := int64(3000)
	bindPlacedBin(t, eng, db, result, nodeID, placed, 6)

	var logs []string
	eng.logFn = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{CalledBy: "test-op"}), "release")

	// Manifest stamped with the live count...
	ingests, _ := listIngests(t, db)
	if len(ingests) != 1 {
		t.Fatalf("release-time ingest stamps = %d, want 1", len(ingests))
	}
	if ingests[0].Quantity != 6 {
		t.Errorf("manifest quantity = %d, want 6 (the live count)", ingests[0].Quantity)
	}
	// ...but the slot keeps its binding and its count.
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.ActiveBinID == nil || *rt.ActiveBinID != placed {
		t.Fatalf("ActiveBinID = %v, want %d — the clear erased the bin the press is filling "+
			"INTO; SimMachineReady gates the machine off permanently (sim 2026-09-07, PLN_001)",
			rt.ActiveBinID, placed)
	}
	if rt.RemainingUOPCached != 6 {
		t.Errorf("RemainingUOP = %d, want 6 — ticks keep landing on the placed bin", rt.RemainingUOPCached)
	}
	if findLogLine(logs, "not the departing one") == "" {
		t.Errorf("no gate breadcrumb logged; logs:\n%s", strings.Join(logs, "\n"))
	}
}

// The classic two_robot shape: the supply leg parks at a staging node and
// never binds here, so the active bin is the DEPARTING one and the clear
// must still fire. Pins that the gate did not over-reach.
func TestProduceRelease_DepartingBinStillClears(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	result, err := eng.RequestProduceSwap(nodeID)
	testutil.MustNoErr(t, err, "RequestProduceSwap")
	markStaged(t, db, result.OrderA.ID)
	markStaged(t, db, result.OrderB.ID)

	departing := int64(777)
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, nil, 61), "bump count")
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, &departing), "bind departing bin")

	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{CalledBy: "test-op"}), "release")

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.ActiveBinID != nil {
		t.Errorf("ActiveBinID = %v, want cleared — the classic clear (active bin is the departing one)", rt.ActiveBinID)
	}
	if rt.RemainingUOPCached != 0 {
		t.Errorf("RemainingUOP = %d, want 0 — hold-and-replay window opens at release", rt.RemainingUOPCached)
	}
}

// The ambiguity arm: a placing order that is terminal but carries no bin_id
// cannot be ruled out as the binder, so the clear is skipped and a
// breadcrumb logged. A wrongly-skipped clear costs a few ticks on the
// departing bin; a wrongly-fired clear costs the press.
func TestProduceRelease_TerminalUnnamedPlacerSkipsClear(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID := seedPressIndexProduceNode(t, db)
	eng := testEngine(t, db)

	result, err := eng.RequestProduceSwap(nodeID)
	testutil.MustNoErr(t, err, "RequestProduceSwap")

	placed := int64(3000)
	markStaged(t, db, result.OrderA.ID)
	markStaged(t, db, result.OrderB.ID)
	b := placed
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, nil, 6), "count ticks")
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, &b), "bind placed bin")
	// Terminal but unnamed — the row the gate cannot decide on.
	testutil.MustNoErr(t, storeorders.UpdateStatus(db.DB, placingLegID(t, eng, result), string(protocol.StatusConfirmed)), "terminalize placer")

	var logs []string
	eng.logFn = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{CalledBy: "test-op"}), "release")

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.ActiveBinID == nil {
		t.Fatal("ActiveBinID cleared — the ambiguity arm must default to keeping the slot bound")
	}
	if findLogLine(logs, "skipping the clear") == "" {
		t.Errorf("no ambiguity breadcrumb logged; logs:\n%s", strings.Join(logs, "\n"))
	}
}

// bindPlacedBin reproduces the 09-07 frame at click 4: the index leg's bin
// is bound on the runtime (the delivery handler's write) and stamped on the
// placing order (Core's write), the press has ticked `count` into it, and
// the evac leg still sits at staged.
func bindPlacedBin(t *testing.T, eng *Engine, db *store.DB, result *NodeOrderResult, nodeID int64, bin int64, count int) {
	t.Helper()
	markStaged(t, db, result.OrderA.ID)
	markStaged(t, db, result.OrderB.ID)
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, nil, count), "count ticks")
	b := bin
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, &b), "bind placed bin")
	testutil.MustNoErr(t, storeorders.UpdateBinID(db.DB, placingLegID(t, eng, result), &b), "stamp the placing leg's bin")
}
