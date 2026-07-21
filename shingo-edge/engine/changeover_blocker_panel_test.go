package engine

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
)

// changeover_blocker_panel_test.go — Stage 0(c): structured cutover blockers,
// the read-only gate-status projection, and the byte-identity of the
// click-time toast across that change.
//
// The panel is the instrument that validates Fix B and C(ii): C(ii) turns
// today's loud terminal skip into a silent non-terminal wait, so the operator
// must be able to SEE what the changeover is waiting on before that lands.

// TestCanCompleteChangeover_EmitsStructuredBlockers pins the shape: one
// blocker per non-terminal task and per non-terminal linked order, each
// carrying its structured identity alongside the sentence.
func TestCanCompleteChangeover_EmitsStructuredBlockers(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)

	ok, blockers, err := eng.canCompleteChangeover(changeover.ID)
	if err != nil {
		t.Fatalf("canCompleteChangeover: %v", err)
	}
	if ok {
		t.Fatal("expected the freshly-started changeover to be blocked")
	}
	if len(blockers) == 0 {
		t.Fatal("blocked gate returned no blockers")
	}

	var sawTask, sawOrder bool
	for _, b := range blockers {
		if b.Reason == "" {
			t.Error("blocker has an empty Reason — the sentence is the only field the operator reads")
		}
		if !b.Hard {
			t.Errorf("blocker %q is not Hard; both conjuncts are hard today", b.Reason)
		}
		switch {
		case strings.HasPrefix(b.Reason, "task at node "):
			sawTask = true
			if b.NodeName == "" {
				t.Errorf("task blocker %q carries no NodeName", b.Reason)
			}
			if b.OrderID != 0 {
				t.Errorf("task blocker %q also carries OrderID %d; exactly one identity should be set", b.Reason, b.OrderID)
			}
			if !strings.Contains(b.Reason, b.NodeName) {
				t.Errorf("task blocker sentence %q does not name its NodeName %q", b.Reason, b.NodeName)
			}
		case strings.HasPrefix(b.Reason, "order "):
			sawOrder = true
			if b.OrderID == 0 {
				t.Errorf("order blocker %q carries no OrderID", b.Reason)
			}
			if b.NodeName != "" {
				t.Errorf("order blocker %q also carries NodeName %q; exactly one identity should be set", b.Reason, b.NodeName)
			}
			if !strings.Contains(b.Reason, fmt.Sprintf("order %d", b.OrderID)) {
				t.Errorf("order blocker sentence %q does not name its OrderID %d", b.Reason, b.OrderID)
			}
		default:
			t.Errorf("unrecognised blocker sentence %q — the two conjuncts produce two known prefixes", b.Reason)
		}
	}
	if !sawTask {
		t.Error("expected at least one task-conjunct blocker on a freshly-started changeover")
	}
	_ = sawOrder // orders may already be terminal depending on scenario timing; task conjunct is the invariant
}

// TestCompleteCutover_ToastMatchesBlockerReasonsExactly is the byte-identity
// guarantee at the call site, not just in the projection helper: the 400
// message must be "cannot cutover: " + the blockers' own sentences joined with
// "; ", unchanged from when the gate returned []string.
func TestCompleteCutover_ToastMatchesBlockerReasonsExactly(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)

	_, blockers, err := eng.canCompleteChangeover(changeover.ID)
	if err != nil {
		t.Fatalf("canCompleteChangeover: %v", err)
	}
	want := "cannot cutover: " + strings.Join(domain.BlockersToReasons(blockers), "; ")

	cutErr := eng.CompleteProcessProductionCutover(processID)
	if cutErr == nil {
		t.Fatal("expected cutover to be blocked")
	}
	if cutErr.Error() != want {
		t.Errorf("toast drifted.\n got: %q\nwant: %q", cutErr.Error(), want)
	}
}

// TestChangeoverGateStatus_NoActiveChangeover_IsNotAnError pins the panel's
// quiet case: a process with nothing in flight reports "can complete, nothing
// pending" instead of an error the page would have to special-case.
func TestChangeoverGateStatus_NoActiveChangeover_IsNotAnError(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, _ := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)

	ok, blockers, err := eng.ChangeoverGateStatus(processID)
	if err != nil {
		t.Fatalf("ChangeoverGateStatus with no active changeover returned an error: %v", err)
	}
	if !ok {
		t.Error("can_complete = false with no active changeover; nothing is gating")
	}
	if len(blockers) != 0 {
		t.Errorf("blockers = %v, want none", blockers)
	}
}

// TestChangeoverGateStatus_IsPureRead is the safe-to-poll guarantee. The HMI
// re-renders this on every SSE tick; if evaluating the gate mutated anything,
// watching a changeover would change it.
func TestChangeoverGateStatus_IsPureRead(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)

	before, err := db.GetActiveProcessChangeover(processID)
	if err != nil {
		t.Fatalf("read changeover before: %v", err)
	}
	tasksBefore, _ := db.ListChangeoverNodeTasks(changeover.ID)

	var firstReasons []string
	for i := range 3 {
		_, blockers, err := eng.ChangeoverGateStatus(processID)
		if err != nil {
			t.Fatalf("ChangeoverGateStatus call %d: %v", i, err)
		}
		reasons := domain.BlockersToReasons(blockers)
		if i == 0 {
			firstReasons = reasons
			continue
		}
		if strings.Join(reasons, "|") != strings.Join(firstReasons, "|") {
			t.Errorf("call %d returned different blockers than call 0: %v vs %v", i, reasons, firstReasons)
		}
	}

	after, err := db.GetActiveProcessChangeover(processID)
	if err != nil {
		t.Fatalf("read changeover after: %v", err)
	}
	if after.State != before.State {
		t.Errorf("changeover state moved %q → %q across a pure read", before.State, after.State)
	}
	tasksAfter, _ := db.ListChangeoverNodeTasks(changeover.ID)
	if len(tasksAfter) != len(tasksBefore) {
		t.Errorf("task count changed %d → %d across a pure read", len(tasksBefore), len(tasksAfter))
	}
	for i := range tasksBefore {
		if tasksBefore[i].State != tasksAfter[i].State {
			t.Errorf("task %d state moved %q → %q across a pure read",
				tasksBefore[i].ID, tasksBefore[i].State, tasksAfter[i].State)
		}
	}
}

// TestCanCompleteChangeover_MissingOrderRowIsNotABlocker preserves the
// sql.ErrNoRows skip. A GC'd or never-persisted order must not wedge the
// changeover: nobody can act on a row that does not exist, so refusing cutover
// over it would be unrecoverable without DB surgery.
func TestCanCompleteChangeover_MissingOrderRowIsNotABlocker(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)
	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}

	// Drive every task terminal so the only possible blocker is the order
	// conjunct, then point the task at an order id that does not exist.
	tasks, _ := db.ListChangeoverNodeTasks(changeover.ID)
	for _, tk := range tasks {
		testutil.MustNoErr(t, db.UpdateChangeoverNodeTaskState(tk.ID, domain.NodeTaskSwitched), "force task terminal")
	}
	if task.OldMaterialReleaseOrderID != nil {
		testutil.MustNoErr(t, db.UpdateOrderStatus(*task.OldMaterialReleaseOrderID, string(orders.StatusConfirmed)), "terminal evac")
	}
	if task.NextMaterialOrderID != nil {
		testutil.MustNoErr(t, db.UpdateOrderStatus(*task.NextMaterialOrderID, string(orders.StatusConfirmed)), "terminal supply")
	}

	ghost := int64(999999)
	testutil.MustNoErr(t, db.LinkChangeoverNodeOrders(task.ID, &ghost, nil), "link a nonexistent order")

	ok, blockers, err := eng.canCompleteChangeover(changeover.ID)
	if err != nil {
		t.Fatalf("canCompleteChangeover with a missing order row returned an error: %v", err)
	}
	for _, b := range blockers {
		if b.OrderID == ghost {
			t.Errorf("missing order %d became a blocker (%q) — it must be skipped", ghost, b.Reason)
		}
	}
	if !ok {
		t.Errorf("gate blocked on %v; a missing order row must not gate", domain.BlockersToReasons(blockers))
	}
	_ = protocol.StatusConfirmed
}
