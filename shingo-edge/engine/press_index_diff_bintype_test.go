package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// ---------------------------------------------------------------------------
// The different-bin-type press-index changeover, driven to completion.
//
// The same-bin-type case rides the press's index motion: one claim, one node,
// one pair of orders. When the bin geometry changes the index cannot shift the
// bins, so FanOutPressIndexDifferentBinType rewrites the parent diff into one
// diff PER SEAT — and the back seats have no style_node_claims row. They never
// have: UpsertClaim refuses the press_position marker, the planner synthesizes
// their claims in memory, and the seat is a physical position of a press rather
// than a cell anyone configures.
//
// Every one of those seats gets its own changeover_node_task in swap_required,
// and the cutover gate blocks until every task is terminal. So the seat tasks
// have to be driveable, which is what this file pins.
// ---------------------------------------------------------------------------

// binTypeCoreServer answers Core's payload-manifest endpoint with a bin type
// per payload code, which is the only signal binTypesDiffer reads. Available()
// is true for any non-empty base URL, so this also satisfies the
// Core-availability gate that guards the fan-out.
func binTypeCoreServer(t *testing.T, binTypeByPayload map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /api/telemetry/payload/{code}/manifest
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		code := ""
		for i, p := range parts {
			if p == "payload" && i+1 < len(parts) {
				code = parts[i+1]
			}
		}
		bt, ok := binTypeByPayload[code]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(PayloadManifestResponse{UOPCapacity: 100, BinTypeCode: bt})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedDiffBinTypePressIndex builds a two-seat press whose styles use different
// bin types.
//
// Both seats get a process_nodes row. That is the configuration the start
// advisory asks for, and it is deliberately present here: without it the seats
// have no row to gate, render or release, and this test would be measuring the
// missing row rather than the missing claim.
func seedDiffBinTypePressIndex(t *testing.T, db *store.DB) (processID, frontID, backID, fromStyleID, toStyleID int64) {
	t.Helper()

	processID, err := db.CreateProcess("PI-DIFF-PROC", "different bin type press index", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")

	frontID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "PI-FRONT", Code: "PIF", Name: "Press Front", Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create front node")
	backID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "PI-BACK", Code: "PIB", Name: "Press Back", Sequence: 2, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create back node")

	fromStyleID, err = db.CreateStyle("PI-FROM", "old style", processID)
	testutil.MustNoErr(t, err, "create from style")
	toStyleID, err = db.CreateStyle("PI-TO", "new style", processID)
	testutil.MustNoErr(t, err, "create to style")
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &fromStyleID), "set active style")

	fromClaimID, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: "PI-FRONT", Role: "produce",
		SwapMode:       protocol.SwapModeTwoRobotPressIndex,
		PairedCoreNode: "PI-BACK",
		PayloadCode:    "PART-OLD", UOPCapacity: 100,
		InboundSource: "MARKET", OutboundDestination: "MARKET",
	})
	testutil.MustNoErr(t, err, "upsert from claim")

	_, err = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: toStyleID, CoreNodeName: "PI-FRONT", Role: "produce",
		SwapMode:       protocol.SwapModeTwoRobotPressIndex,
		PairedCoreNode: "PI-BACK",
		PayloadCode:    "PART-NEW", UOPCapacity: 100,
		InboundSource: "MARKET", OutboundDestination: "MARKET",
	})
	testutil.MustNoErr(t, err, "upsert to claim")

	// Both seats are physically occupied by the outgoing style.
	_, err = db.EnsureProcessNodeRuntime(frontID)
	testutil.MustNoErr(t, err, "ensure front runtime")
	_, err = db.EnsureProcessNodeRuntime(backID)
	testutil.MustNoErr(t, err, "ensure back runtime")
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(frontID, &fromClaimID, 50), "seed front runtime")
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(backID, &fromClaimID, 50), "seed back runtime")
	return
}

// startDiffBinTypeChangeover seeds the scenario, starts the changeover, and
// returns the engine plus the two seat node ids.
func startDiffBinTypeChangeover(t *testing.T, db *store.DB) (eng *Engine, processID, frontID, backID int64) {
	t.Helper()
	processID, frontID, backID, _, toStyleID := seedDiffBinTypePressIndex(t, db)
	eng = testEngine(t, db)
	eng.wireEventHandlers()
	eng.coreClient = NewCoreClient(binTypeCoreServer(t, map[string]string{
		"PART-OLD": "TOTE",
		"PART-NEW": "BIN",
	}).URL)

	_, err := eng.StartProcessChangeover(processID, toStyleID, "test", "different bin type press index")
	testutil.MustNoErr(t, err, "start changeover")
	return eng, processID, frontID, backID
}

// TestDiffBinTypePressIndex_FansOutOneTaskPerSeat is the precondition the
// deadlock test depends on: the fan-out really does give the back seat its own
// task, and that task really does gate the cutover.
func TestDiffBinTypePressIndex_FansOutOneTaskPerSeat(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, processID, _, _ := startDiffBinTypeChangeover(t, db)

	changeover, err := db.GetActiveProcessChangeover(processID)
	testutil.MustNoErr(t, err, "get active changeover")

	tasks, err := db.ListChangeoverNodeTasks(changeover.ID)
	testutil.MustNoErr(t, err, "list node tasks")

	byNode := map[string]processes.NodeTask{}
	for _, task := range tasks {
		byNode[task.NodeName] = task
	}
	if len(byNode) != 2 {
		t.Fatalf("different bin types must fan out to one task per seat, got %d task(s): %v", len(byNode), byNode)
	}
	for _, name := range []string{"Press Front", "Press Back"} {
		task, ok := byNode[name]
		if !ok {
			t.Fatalf("no changeover task for seat %q — got %v", name, byNode)
		}
		// Non-terminal is the property that matters: a task in any live state
		// gates the cutover, and only the per-node actions can advance it.
		// (The applier creates the supply legs at start, so the state here is
		// staging_requested rather than the swap_required it was born in.)
		if domain.IsNodeTaskStateTerminal(task.State, task.Situation) {
			t.Errorf("seat %q task is already terminal (%q) — this scenario is meant to leave real work to do",
				name, task.State)
		}
	}

	ok, blockers, err := eng.canCompleteChangeover(changeover.ID)
	testutil.MustNoErr(t, err, "canCompleteChangeover")
	if ok {
		t.Fatal("cutover must be blocked while both seat tasks are in swap_required")
	}
	if len(blockers) < 2 {
		t.Errorf("want a blocker per unfinished seat, got %d: %v", len(blockers), blockers)
	}
}

// TestDiffBinTypePressIndex_BackSeatDrivesToCompletion is the deadlock.
//
// The back seat owns a task keyed to its own node name, and the per-node
// actions resolve their claim with GetStyleNodeClaimByNode(styleID, nodeName).
// A back seat has no persisted row under its own name — by design — so every
// action refuses and the task can never leave swap_required. The cutover gate
// then blocks forever and cancel is the only exit.
//
// RED before the seat resolver: SwitchNodeToTarget on the back seat returns
// "target style claim not found for node".
func TestDiffBinTypePressIndex_BackSeatDrivesToCompletion(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, processID, frontID, backID := startDiffBinTypeChangeover(t, db)

	changeover, err := db.GetActiveProcessChangeover(processID)
	testutil.MustNoErr(t, err, "get active changeover")

	// The whole point: the seat with no row of its own must be actionable.
	if err := eng.SwitchNodeToTarget(processID, backID); err != nil {
		t.Fatalf("back seat PI-BACK could not be switched to the target style: %v\n"+
			"The fan-out gave this seat its own changeover task, so the cutover gate waits on it, "+
			"but the seat has no style_node_claims row under its own name and the action resolves "+
			"claims by node name only. Nothing can advance the task and cancel is the only exit.", err)
	}
	if err := eng.SwitchNodeToTarget(processID, frontID); err != nil {
		t.Fatalf("front seat PI-FRONT could not be switched to the target style: %v", err)
	}

	// The supply legs the applier created at start still gate the cutover —
	// correctly, they place bins at participant nodes. Land them, as the robots
	// would, so what remains under test is the seat tasks.
	tasks, err := db.ListChangeoverNodeTasks(changeover.ID)
	testutil.MustNoErr(t, err, "list node tasks")
	for _, task := range tasks {
		for _, orderID := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
			if orderID == nil {
				continue
			}
			testutil.MustNoErr(t, db.UpdateOrderStatus(*orderID, string(orders.StatusConfirmed)), "land order")
		}
	}

	ok, blockers, err := eng.canCompleteChangeover(changeover.ID)
	testutil.MustNoErr(t, err, "canCompleteChangeover")
	if !ok {
		t.Fatalf("every seat task is done, so cutover must be allowed; still blocked by: %v", blockers)
	}
}

// TestDiffBinTypePressIndex_BackSeatEvacuates pins the evacuation leg of the
// same seam. EvacuateNode resolves the FROM claim rather than the TO claim, so
// it fails in its own way — the seat is not claimed under its own name on
// either side.
func TestDiffBinTypePressIndex_BackSeatEvacuates(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, processID, _, backID := startDiffBinTypeChangeover(t, db)

	if _, err := eng.EvacuateNode(processID, backID, 0); err != nil {
		t.Fatalf("back seat PI-BACK could not be evacuated: %v\n"+
			"An evac on a fanned-out seat is the release leg the changeover planned for it.", err)
	}
}
