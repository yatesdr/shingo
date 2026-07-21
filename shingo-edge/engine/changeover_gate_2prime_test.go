package engine

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
)

// changeover_gate_2prime_test.go — Fix B (Amendment-1 form): conjunct 2′,
// auto-path parity, stragglers, and the post-finalize claim pinning test.

// start2PrimeScenario starts the Phase-3 changeover, drives every TASK
// terminal, and returns the evac order id — leaving the ORDER conjunct as the
// only thing between the gate and cutover, which is what 2′ changes.
func start2PrimeScenario(t *testing.T, eng *Engine) (processID int64, evacID int64, changeoverID int64) {
	t.Helper()
	db := eng.db
	pID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, pID, toStyleID)
	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if task.OldMaterialReleaseOrderID == nil {
		t.Fatal("expected an evac order")
	}
	// Supply leg (if any) terminal; tasks terminal. Only the evac's status and
	// step shape decide the gate from here.
	if task.NextMaterialOrderID != nil {
		testutil.MustNoErr(t, db.UpdateOrderStatus(*task.NextMaterialOrderID, string(orders.StatusConfirmed)), "confirm supply")
	}
	tasks, _ := db.ListChangeoverNodeTasks(changeover.ID)
	for _, tk := range tasks {
		testutil.MustNoErr(t, db.UpdateChangeoverNodeTaskState(tk.ID, domain.NodeTaskSwitched), "switch task")
	}
	return pID, *task.OldMaterialReleaseOrderID, changeover.ID
}

// TestGate2Prime_OutboundLegDoesNotGate is the HOP fix itself: a NON-terminal
// evac whose steps place a bin only at the MARKET (no participant) must not
// block cutover. Under the old conjunct this exact shape held the changeover
// hostage to a robot driving away from the line.
func TestGate2Prime_OutboundLegDoesNotGate(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	processID, evacID, changeoverID := start2PrimeScenario(t, eng)

	// Outbound shape: lift from the line, drop at the market. Places nothing
	// at any participant.
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(evacID,
		`[{"action":"pickup","node":"P3-NODE"},{"action":"dropoff","node":"MARKET-X"}]`), "outbound steps")
	testutil.MustNoErr(t, db.UpdateOrderStatus(evacID, string(orders.StatusInTransit)), "evac in transit")

	ok, blockers, err := eng.canCompleteChangeover(changeoverID)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if !ok {
		t.Errorf("outbound in-transit evac blocked cutover: %v — the HOP hostage case",
			domain.BlockersToReasons(blockers))
	}

	// And the cutover itself completes with the leg still in flight.
	if err := eng.CompleteProcessProductionCutover(processID); err != nil {
		t.Errorf("cutover refused with only an outbound leg in flight: %v", err)
	}
}

// TestGate2Prime_PlacingLegStillGates is the other half — the conjunct is
// scoped, not deleted. A non-terminal leg that DROPS a bin at a participant
// gates, with the byte-identical sentence.
func TestGate2Prime_PlacingLegStillGates(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, evacID, changeoverID := start2PrimeScenario(t, eng)

	// Supply shape: fetch from market, drop at the participant node.
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(evacID,
		`[{"action":"pickup","node":"MARKET-X"},{"action":"dropoff","node":"P3-NODE"}]`), "placing steps")
	testutil.MustNoErr(t, db.UpdateOrderStatus(evacID, string(orders.StatusInTransit)), "in transit")

	ok, blockers, err := eng.canCompleteChangeover(changeoverID)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if ok {
		t.Fatal("a leg placing a bin at a participant did not gate — dual-dispatch prevention lost")
	}
	reasons := domain.BlockersToReasons(blockers)
	if len(reasons) != 1 || !strings.HasPrefix(reasons[0], "order ") || !strings.Contains(reasons[0], "in_transit") {
		t.Errorf("blockers = %v, want the byte-identical \"order N in in_transit\"", reasons)
	}
}

// TestGate2Prime_UnreadableStepsFailClosed pins the fail-closed rule: no steps
// (a simple move) and undecodable steps both gate, exactly as every order did
// before 2′. The classifier only un-gates what it can prove.
func TestGate2Prime_UnreadableStepsFailClosed(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, evacID, changeoverID := start2PrimeScenario(t, eng)

	for name, stepsJSON := range map[string]string{
		"no steps":    "",
		"junk steps":  "{not json",
		"empty array": "[]", // decodes, places nothing anywhere… see below
	} {
		testutil.MustNoErr(t, db.UpdateOrderStepsJSON(evacID, stepsJSON), name)
		testutil.MustNoErr(t, db.UpdateOrderStatus(evacID, string(orders.StatusInTransit)), "in transit")

		ok, _, err := eng.canCompleteChangeover(changeoverID)
		if err != nil {
			t.Fatalf("%s: gate: %v", name, err)
		}
		switch name {
		case "empty array":
			// A DECODED empty step list proves the leg touches no participant —
			// that is a proof, not an unknown, so it does not gate.
			if !ok {
				t.Errorf("%s: gated; a decoded empty step list is a proof of no placement", name)
			}
		default:
			if ok {
				t.Errorf("%s: did NOT gate; unreadable shapes must fail closed", name)
			}
		}
	}
}

// TestTryComplete_AutoPathRunsTheConfirmPrePass is the parity fix: a DELIVERED
// but unconfirmed placing leg must not hang the automatic cutover when the
// operator path would have auto-confirmed it. One gate, one definition of
// ready.
func TestTryComplete_AutoPathRunsTheConfirmPrePass(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	processID, evacID, _ := start2PrimeScenario(t, eng)

	// A placing leg, delivered but not confirmed — the clerical state the
	// pre-pass exists for.
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(evacID,
		`[{"action":"pickup","node":"MARKET-X"},{"action":"dropoff","node":"P3-NODE"}]`), "placing steps")
	testutil.MustNoErr(t, db.UpdateOrderStatus(evacID, string(orders.StatusDelivered)), "delivered")

	// tryComplete's trigger precondition: the per-node switches have already
	// flipped the active style to the to-style; the auto path then closes the
	// row. Simulate that flip.
	co, err := db.GetActiveProcessChangeover(processID)
	if err != nil {
		t.Fatalf("get changeover: %v", err)
	}
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &co.ToStyleID), "flip active style")

	if err := eng.tryCompleteProcessChangeover(processID); err != nil {
		t.Fatalf("tryComplete: %v", err)
	}
	if still, _ := db.GetActiveProcessChangeover(processID); still != nil {
		t.Errorf("auto path left the changeover open on a delivered-unconfirmed leg (state %q) — parity with the operator pre-pass lost", still.State)
	}
	got, _ := db.GetOrder(evacID)
	if got.Status != orders.StatusConfirmed {
		t.Errorf("delivered leg = %q after auto cutover, want confirmed (the pre-pass)", got.Status)
	}
}

// TestStragglers covers both dispositions after finalize: a failure stamps the
// task with one STRAGGLER record; a confirm clears the runtime order refs
// (phantom-[REP] class). Neither re-runs completion.
func TestStragglers(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	processID, evacID, changeoverID := start2PrimeScenario(t, eng)

	// Leave the evac outbound + in flight, cut over past it (the 2′ case),
	// so its terminal arrives AFTER finalize.
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(evacID,
		`[{"action":"pickup","node":"P3-NODE"},{"action":"dropoff","node":"MARKET-X"}]`), "outbound steps")
	testutil.MustNoErr(t, db.UpdateOrderStatus(evacID, string(orders.StatusInTransit)), "in transit")
	if err := eng.CompleteProcessProductionCutover(processID); err != nil {
		t.Fatalf("cutover: %v", err)
	}

	t.Run("failure stamps the task", func(t *testing.T) {
		// Un-switch the task so there is a non-terminal state to stamp — the
		// stamp is for tasks the finalize left mid-flight.
		tasks, _ := db.ListChangeoverNodeTasks(changeoverID)
		for _, tk := range tasks {
			testutil.MustNoErr(t, db.UpdateChangeoverNodeTaskState(tk.ID, domain.NodeTaskReleaseRequested), "rewind task")
		}
		testutil.MustNoErr(t, db.UpdateOrderStatus(evacID, string(orders.StatusFailed)), "fail evac")
		eng.handleNodeOrderFailed(OrderFailedEvent{OrderID: evacID, Reason: "fleet: FAILED"})

		task, coState, err := db.FindChangeoverNodeTaskByOrderID(evacID)
		if err != nil || task == nil {
			t.Fatalf("find task by order: %v", err)
		}
		if !coState.IsTerminal() {
			t.Fatal("scenario invariant: changeover should be finalized")
		}
		if task.State != domain.NodeTaskError {
			t.Errorf("straggler task state = %q, want error (the stamped disposition)", task.State)
		}
		if task.SkipNote == "" {
			t.Error("straggler stamp carries no note; the record is the point")
		}
	})

	t.Run("confirm clears runtime refs", func(t *testing.T) {
		// Point the node's runtime at the order, as a live swap would have.
		task, _, _ := db.FindChangeoverNodeTaskByOrderID(evacID)
		testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(task.ProcessNodeID, &evacID, nil), "point runtime at order")
		testutil.MustNoErr(t, db.UpdateOrderStatus(evacID, string(orders.StatusConfirmed)), "confirm evac")

		eng.handleNodeOrderCompleted(OrderCompletedEvent{OrderID: evacID, ProcessNodeID: &task.ProcessNodeID})

		rt, err := db.GetProcessNodeRuntime(task.ProcessNodeID)
		if err != nil {
			t.Fatalf("runtime: %v", err)
		}
		if rt.ActiveOrderID != nil && *rt.ActiveOrderID == evacID {
			t.Error("confirmed straggler still referenced by runtime — the phantom-[REP] badge")
		}
	})
}

// TestFindActiveClaim_PostCutover is the pinning test the review asked for
// INSTEAD of defensive code: after finalize (target style nilled), resolution
// answers from the active style for claimed nodes and honestly nil for nodes
// the new style dropped.
func TestFindActiveClaim_PostCutover(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	processID, evacID, _ := start2PrimeScenario(t, eng)
	testutil.MustNoErr(t, db.UpdateOrderStatus(evacID, string(orders.StatusConfirmed)), "confirm evac")
	if err := eng.CompleteProcessProductionCutover(processID); err != nil {
		t.Fatalf("cutover: %v", err)
	}

	process, _ := db.GetProcess(processID)
	if process.TargetStyleID != nil {
		t.Fatal("finalize did not nil target_style_id — the pinned dependency moved")
	}
	node, err := db.GetProcessNodeByCoreNodeName("P3-NODE")
	if err != nil || node == nil {
		t.Fatalf("get node: %v", err)
	}
	claim := findActiveClaim(db, node)
	if claim == nil {
		t.Fatal("post-cutover resolution returned nil for a node the to-style claims — the active-style branch must answer")
	}
	if claim.PayloadCode != "PART-NEW" {
		t.Errorf("post-cutover claim payload = %q, want the TO-style's PART-NEW", claim.PayloadCode)
	}
}
