package engine

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
)

// Five reproduction tests pinning the v2-plan changeover-completion gate
// (canCompleteChangeover) and its surrounding plumbing. Each test
// reproduces a specific scenario from the 5/8 trial diagnostic data or
// the round-4 review.

// TestChangeover_PrematureComplete_TasksNonTerminal_IsBlocked replicates
// changeover 33's scenario: operator-driven cutover called while node
// tasks are still at staging_requested with linked orders in flight.
// Without the gate the row would advance to completed; with the gate it
// returns an error and the row state stays in_progress.
func TestChangeover_PrematureComplete_TasksNonTerminal_IsBlocked(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)

	// Tasks land at staging_requested via auto-staging; orders are in
	// flight (status=created/submitted, not terminal). Premature cutover.
	err := eng.CompleteProcessProductionCutover(processID)
	if err == nil {
		t.Fatal("expected gate to block cutover with non-terminal tasks, got nil error")
	}
	if !strings.Contains(err.Error(), "cannot cutover") {
		t.Errorf("error message %q does not name the cutover-block reason", err.Error())
	}

	co, err := db.GetActiveProcessChangeover(processID)
	if err != nil {
		t.Fatalf("get active changeover after blocked cutover: %v", err)
	}
	if co == nil || co.State == domain.ChangeoverCompleted {
		t.Fatalf("changeover should still be active after blocked cutover, got %+v", co)
	}
	if co.ID != changeover.ID {
		t.Fatalf("changeover ID changed: got %d want %d", co.ID, changeover.ID)
	}
}

// TestChangeover_PrematureComplete_OrdersInFlight_IsBlocked replicates
// changeover 36's scenario: tasks driven to released but the linked
// retrieve order is still in_transit when operator clicks cutover.
// Pre-fix the row would close while the order was still moving; the
// late-arriving completion would then find no active changeover and
// silently bail (task 105 stranded). The gate's order-terminality
// check blocks this.
func TestChangeover_PrematureComplete_OrdersInFlight_IsBlocked(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)
	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)

	// Drive the task state to released directly. Don't move the linked
	// orders past in_transit — the gate's second check (orders terminal)
	// is the one we're pinning here.
	testutil.MustNoErr(t, db.UpdateChangeoverNodeTaskState(task.ID, domain.NodeTaskReleased), "update task state")
	for _, orderIDPtr := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
		if orderIDPtr == nil {
			continue
		}
		db.UpdateOrderStatus(*orderIDPtr, string(orders.StatusSubmitted))
		db.UpdateOrderStatus(*orderIDPtr, string(orders.StatusInTransit))
	}

	err := eng.CompleteProcessProductionCutover(processID)
	if err == nil {
		t.Fatal("expected gate to block cutover with in-flight orders, got nil")
	}
	// Error should name at least one in-flight order to make the HMI
	// surface useful — operator needs to know which order is blocking.
	if !strings.Contains(err.Error(), "order") {
		t.Errorf("error %q should name the blocking order(s)", err.Error())
	}
	co, _ := db.GetActiveProcessChangeover(processID)
	if co == nil || co.State == domain.ChangeoverCompleted {
		t.Fatalf("changeover should still be active, got %+v", co)
	}
}

// TestChangeover_AutoCompleteFiresOnTerminalTransition pins the B.3
// auto-completion trigger plumbing. After the operator-driven cutover
// has flipped active_style_id but failed the gate (tasks non-terminal),
// the subsequent order-completion handler that drives the last task to
// released must fire tryCompleteProcessChangeover so the row advances
// without operator intervention.
func TestChangeover_AutoCompleteFiresOnTerminalTransition(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)
	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)

	// Simulate the operator having set active_style_id to to-style. In
	// the real flow this happens at the top of CompleteProcessProductionCutover
	// before the gate runs (the function flips active style first); the
	// gate then blocks completion. Simulate by setting active style
	// directly so tryCompleteProcessChangeover's first precondition holds.
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &toStyleID), "set active style")

	// Drive both linked orders to terminal so the gate's order check
	// passes. Then fire EventOrderCompleted for the release order — that
	// handler stamps the task to released and (per B.3) fires tryComplete.
	for _, orderIDPtr := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
		if orderIDPtr == nil {
			continue
		}
		markOrderTerminal(db, *orderIDPtr)
	}
	releaseOrderID := *task.OldMaterialReleaseOrderID
	releaseOrder, _ := db.GetOrder(releaseOrderID)
	emitOrderCompleted(eng, releaseOrderID, releaseOrder.UUID, releaseOrder.OrderType, &nodeID)

	co, _ := db.GetActiveProcessChangeover(processID)
	if co != nil {
		t.Fatalf("expected changeover to auto-complete after terminal transition, got state=%s", co.State)
	}
}

// TestChangeover_OrphanCancelStampsTask pins the orphan-cancellation
// handler. When a linked order transitions to StatusCancelled outside
// cancelProcessChangeoverInternal — e.g. via the operator's per-order
// cancel button — the handler must stamp the task to "cancelled".
func TestChangeover_OrphanCancelStampsTask(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)
	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.NextMaterialOrderID == nil {
		t.Fatal("expected task to have a NextMaterialOrderID for this scenario")
	}
	orderID := *task.NextMaterialOrderID

	// Drive the order to StatusCancelled (the lifecycle for an
	// operator-cancelled order goes through StatusSubmitted → StatusCancelled).
	db.UpdateOrderStatus(orderID, string(orders.StatusSubmitted))
	db.UpdateOrderStatus(orderID, string(orders.StatusCancelled))

	order, _ := db.GetOrder(orderID)
	emitOrderCompleted(eng, orderID, order.UUID, order.OrderType, &nodeID)

	updated, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get task after orphan cancel: %v", err)
	}
	if updated.State != domain.NodeTaskCancelled {
		t.Errorf("expected task state cancelled, got %s", updated.State)
	}
}

// TestChangeover_OrphanFailStampsTask pins the orphan handler's Failed
// branch, the counterpart to TestChangeover_OrphanCancelStampsTask. When
// a linked order transitions to StatusFailed and reaches the completion
// dispatcher through EventOrderCompleted (i.e. via handleNodeOrderCompleted's
// non-Confirmed dispatch to handleOrphanedTaskOrderCompleted, not via
// EventOrderFailed → handleNodeOrderFailed), the handler must stamp the
// task to "error" rather than "cancelled".
//
// Drives the path with emitOrderCompleted only (not emitOrderFailed) so
// the assertion isolates the orphan handler's StatusFailed → "error"
// mapping at wiring_completion.go:540-543. In production both events
// generally fire together for failed orders; this test pins the orphan
// handler's behavior independently so a future refactor that splits or
// reorders the two paths can't silently regress this branch.
func TestChangeover_OrphanFailStampsTask(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)
	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.NextMaterialOrderID == nil {
		t.Fatal("expected task to have a NextMaterialOrderID for this scenario")
	}
	orderID := *task.NextMaterialOrderID

	// Drive the order to StatusFailed (Submitted → Failed is the path
	// taken by a fleet rejection or unrecoverable dispatch error).
	db.UpdateOrderStatus(orderID, string(orders.StatusSubmitted))
	db.UpdateOrderStatus(orderID, string(orders.StatusFailed))

	order, _ := db.GetOrder(orderID)
	emitOrderCompleted(eng, orderID, order.UUID, order.OrderType, &nodeID)

	updated, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get task after orphan fail: %v", err)
	}
	if updated.State != domain.NodeTaskError {
		t.Errorf("expected task state error, got %s", updated.State)
	}
}

// TestChangeover_FailedTaskInCompletedChangeover_NotStamped pins the
// guard against late-failure stamping on already-finalized changeovers.
// After the gate ships, "completed" means "really done"; an error stamp
// on a child task is incoherent. handleNodeOrderFailed uses
// GetActiveProcessChangeover which filters out completed/cancelled rows
// at the SQL layer — this test makes the invariant explicit.
func TestChangeover_FailedTaskInCompletedChangeover_NotStamped(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)
	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.NextMaterialOrderID == nil {
		t.Fatal("expected NextMaterialOrderID for this scenario")
	}
	orderID := *task.NextMaterialOrderID

	// Force the changeover into completed state with the task at released.
	testutil.MustNoErr(t, db.UpdateChangeoverNodeTaskState(task.ID, domain.NodeTaskReleased), "update task state")
	testutil.MustNoErr(t, db.UpdateProcessChangeoverState(changeover.ID, domain.ChangeoverCompleted), "update changeover state")

	// Late-arriving failure event for the same order. Pre-gate this would
	// have stamped the task to error; post-gate the SQL filter on
	// GetActiveProcessChangeover (changeover.state NOT IN completed,
	// cancelled) prevents the stamp.
	order, _ := db.GetOrder(orderID)
	emitOrderFailed(eng, orderID, order.UUID, order.OrderType, "late failure")

	final, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get task after late failure: %v", err)
	}
	if final.State != domain.NodeTaskReleased {
		t.Errorf("expected task to remain released, got %s — late failure stamped a finalized changeover", final.State)
	}
}
