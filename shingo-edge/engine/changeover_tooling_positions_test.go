package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// ---------------------------------------------------------------------------
// N1-a — THE FIRST CHANGEOVER MUST SEE EVERY POSITION.
//
// A press position that owns no style_node_claims row of its own also owns no
// process_nodes row until something creates one, and the thing that creates one
// is ChangeoverService.Create — which runs AFTER the plan is built. So on the
// FIRST changeover of any marked press, the paired position was invisible to
// planning: no clearance, no hold, no order, no warning. On the second it
// worked, because the first had left the row behind.
//
// The sim proved it twice on a pristine seed (SIM-REPORT-n1-2026-08-24, N1-a):
// two previews minutes apart, one action then two, with nothing changed but the
// existence of the row. It is the same disease N1 itself was — a marked position
// silently getting no treatment — one layer down.
// ---------------------------------------------------------------------------

// seedMarkedPressScenario builds a press-index cell whose PAIRED position has no
// process_nodes row, which is the shipped state of every press before its first
// changeover: demo.yaml declares the station, the Edge row is created on demand.
func seedMarkedPressScenario(t *testing.T, db *store.DB) (processID, fromStyleID, toStyleID int64) {
	t.Helper()

	processID, err := db.CreateProcess("POSITION-PRESS", "marked press", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	// ONLY the front position gets a row. PRESS-B is named by the claims and by
	// nothing else — exactly the shipped condition.
	if _, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "PRESS-A", Code: "PRESS-A", Name: "PRESS-A",
		Sequence: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("create front node: %v", err)
	}

	fromStyleID, err = db.CreateStyle("POSITION-FROM", "outgoing, marks both positions", processID)
	if err != nil {
		t.Fatalf("create from style: %v", err)
	}
	toStyleID, err = db.CreateStyle("POSITION-TO", "incoming, stages", processID)
	if err != nil {
		t.Fatalf("create to style: %v", err)
	}
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &fromStyleID), "set active style")

	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:             fromStyleID,
		CoreNodeName:        "PRESS-A",
		Role:                protocol.ClaimRoleProduce,
		SwapMode:            protocol.SwapModeTwoRobotPressIndex,
		PayloadCode:         "PART-OLD",
		UOPCapacity:         30,
		PairedCoreNode:      "PRESS-B",
		InboundSource:       "EMPTIES",
		OutboundDestination: "MARKET",
		ChangeoverEvacNodes: &[]string{"PRESS-A", "PRESS-B"},
	}); err != nil {
		t.Fatalf("upsert from claim: %v", err)
	}
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:             toStyleID,
		CoreNodeName:        "PRESS-A",
		Role:                protocol.ClaimRoleProduce,
		SwapMode:            protocol.SwapModeTwoRobotPressIndex,
		PayloadCode:         "PART-OLD",
		UOPCapacity:         30,
		PairedCoreNode:      "PRESS-B",
		InboundSource:       "EMPTIES",
		OutboundDestination: "MARKET",
		InboundStaging:      "IN-STAGE",
	}); err != nil {
		t.Fatalf("upsert to claim: %v", err)
	}
	return processID, fromStyleID, toStyleID
}

// TestToolingFirstChangeoverPreviewsEveryPosition is the two-previews evidence as an
// assertion: the operator's preview must show the work the changeover will
// actually do, on the first arm as on the second.
func TestToolingFirstChangeoverPreviewsEveryPosition(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, toStyleID := seedMarkedPressScenario(t, db)
	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient("http://test-core")

	plan, err := eng.PreviewChangeoverPlan(processID, toStyleID)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	covered := map[string]bool{}
	for _, a := range plan.Actions {
		covered[a.CoreNodeName] = true
	}
	for _, position := range []string{"PRESS-A", "PRESS-B"} {
		if !covered[position] {
			t.Errorf("preview does not cover marked position %s (covers %v).\n"+
				"A position with no process_nodes row is still a position with a bin on it. The row is "+
				"created by ChangeoverService.Create, which runs after planning — so on the "+
				"FIRST changeover of a press the paired position was silently skipped.",
				position, keysOf(covered))
		}
	}
}

// TestToolingFirstChangeoverGivesEveryPositionAnOrder is the same defect one layer
// further down, and it is the one that costs material: even where the plan
// covers a position, the applier drops its action unless the position also has a node
// TASK — and tasks are built from the diffs, which never mention an expanded
// position.
func TestToolingFirstChangeoverGivesEveryPositionAnOrder(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, toStyleID := seedMarkedPressScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.coreClient = NewCoreClient("http://test-core")

	co, err := eng.StartProcessChangeover(processID, toStyleID, "test", "first changeover")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}
	tasks, err := db.ListChangeoverNodeTasks(co.ID)
	if err != nil {
		t.Fatalf("list node tasks: %v", err)
	}
	byNode := map[string]processes.NodeTask{}
	for _, task := range tasks {
		node, err := db.GetProcessNode(task.ProcessNodeID)
		if err != nil {
			t.Fatalf("get node %d: %v", task.ProcessNodeID, err)
		}
		byNode[node.CoreNodeName] = task
	}
	for _, position := range []string{"PRESS-A", "PRESS-B"} {
		task, ok := byNode[position]
		if !ok {
			t.Errorf("marked position %s has no changeover node task (tasks exist for %v).\n"+
				"applyChangeoverPlan finds a task by node id and skips the action when there is "+
				"none, so a position with no task gets no order however well it was planned.",
				position, keysOf(byNode))
			continue
		}
		if task.NextMaterialOrderID == nil {
			t.Errorf("marked position %s got a task in state %q but no order.\n"+
				"Its bin is in the way of the setup and its replacement must hold at staging; "+
				"neither happens without a leg.", position, task.State)
		}
	}
}

// TestToolingPositionsMaterializeOnceIsIdempotent guards the obvious way to get the
// fix wrong: creating the row on every plan would duplicate process_nodes, and
// this schema has been through one duplicate-node cleanup already
// (collapseDuplicateProcessNodes) after PLC ticks were counted three times.
func TestToolingPositionsMaterializeOnceIsIdempotent(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, toStyleID := seedMarkedPressScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.coreClient = NewCoreClient("http://test-core")

	if _, err := eng.PreviewChangeoverPlan(processID, toStyleID); err != nil {
		t.Fatalf("preview: %v", err)
	}
	if _, err := eng.PreviewChangeoverPlan(processID, toStyleID); err != nil {
		t.Fatalf("second preview: %v", err)
	}
	if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", "idempotence"); err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	nodes, err := db.ListProcessNodesByProcess(processID)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	count := map[string]int{}
	for _, n := range nodes {
		count[n.CoreNodeName]++
	}
	for position, n := range count {
		if n != 1 {
			t.Errorf("process node %s exists %d times — position materialization must be idempotent", position, n)
		}
	}
	if count["PRESS-B"] != 1 {
		t.Errorf("PRESS-B has %d process_nodes rows after a preview, a preview and a start; want exactly 1",
			count["PRESS-B"])
	}
}

// TestDeliverMaterialForPositionDeliversSomething is the remedy hole: the button an
// operator presses to unblock a position returned 200, marked the task released,
// and delivered NOTHING.
//
// SynthesizePositionClaim clears InboundStaging — deliberately, because the
// diff pipeline relies on a synthesized Add falling through to a direct retrieve
// that the tooling decorator then adds the hold to. But
// DeliverNewMaterialForChangeover read that same cleared field as "this node
// does not stage", took the no-staging branch, and marked the position released
// while its bin never moved. Observed twice on the sim, both shapes.
//
// The staging node is a property of the CELL, not of one position in it, so the
// position's answer is its parent's answer.
func TestDeliverMaterialForPositionDeliversSomething(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, toStyleID := seedMarkedPressScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.coreClient = NewCoreClient("http://test-core")

	co, err := eng.StartProcessChangeover(processID, toStyleID, "test", "remedy hole")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}
	positionNode, err := db.GetProcessNodeByCoreNodeName("PRESS-B")
	if err != nil || positionNode == nil {
		t.Fatalf("paired position has no node after start: %v", err)
	}
	// Stand in for the position's leg being gone — cancelled, abandoned, or never
	// created — which is the only reason an operator reaches for this button.
	if _, err := db.DB.Exec(
		`UPDATE changeover_node_tasks SET next_material_order_id=NULL WHERE process_changeover_id=? AND process_node_id=?`,
		co.ID, positionNode.ID); err != nil {
		t.Fatalf("clear position leg: %v", err)
	}

	order, err := eng.DeliverNewMaterialForChangeover(processID, positionNode.ID)
	if err != nil {
		t.Fatalf("deliver-material on a marked position: %v", err)
	}
	if order == nil {
		task, terr := db.GetChangeoverNodeTaskByNode(co.ID, positionNode.ID)
		state := "?"
		if terr == nil && task != nil {
			state = string(task.State)
		}
		t.Fatalf("deliver-material created no order for position PRESS-B and left the task %q.\n"+
			"The cell stages its inbound material, so a position of that cell stages too — reporting "+
			"success while delivering nothing is how the operator's one remedy became a no-op.",
			state)
	}
	steps, err := db.GetOrderStepsJSON(order.ID)
	if err != nil {
		t.Fatalf("read steps for order %d: %v", order.ID, err)
	}
	if !strings.Contains(steps, "IN-STAGE") {
		t.Errorf("position delivery does not come from the staging node: steps = %q", steps)
	}
}

// keysOf renders a set for a failure message.
func keysOf[V any](m map[string]V) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return strings.Join(out, ",")
}

// seedDisjointPressScenario is the shape the sim caught: the incoming style
// runs the same press on DIFFERENT nodes, and neither of the new nodes has a
// process_nodes row yet.
//
// PLN_006 is the one that matters. It is an ADD — synthesized by the cross-mode
// fan-out, named by no mark — so a fix that materialises only MARKED nodes
// leaves it exactly where N1-a left the paired node: a task, no order, and a
// hard cutover blocker.
func seedDisjointPressScenario(t *testing.T, db *store.DB) (processID, toStyleID int64) {
	t.Helper()
	processID, err := db.CreateProcess("DISJOINT-PRESS", "moves nodes", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	if _, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "OLD-A", Code: "OLD-A", Name: "OLD-A",
		Sequence: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	fromStyleID, err := db.CreateStyle("DJ-FROM", "outgoing", processID)
	if err != nil {
		t.Fatalf("create from style: %v", err)
	}
	toStyleID, err = db.CreateStyle("DJ-TO", "incoming, elsewhere", processID)
	if err != nil {
		t.Fatalf("create to style: %v", err)
	}
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &fromStyleID), "set active style")

	base := processes.NodeClaimInput{
		Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobotPressIndex,
		PayloadCode: "PART-OLD", UOPCapacity: 30,
		InboundSource: "EMPTIES", OutboundDestination: "MARKET",
	}
	from := base
	from.StyleID, from.CoreNodeName, from.PairedCoreNode = fromStyleID, "OLD-A", "OLD-B"
	from.ChangeoverEvacNodes = &[]string{"OLD-A", "OLD-B"}
	if _, err := db.UpsertStyleNodeClaim(from); err != nil {
		t.Fatalf("upsert from claim: %v", err)
	}
	to := base
	to.StyleID, to.CoreNodeName, to.PairedCoreNode = toStyleID, "NEW-A", "NEW-B"
	to.InboundStaging = "IN-STAGE"
	if _, err := db.UpsertStyleNodeClaim(to); err != nil {
		t.Fatalf("upsert to claim: %v", err)
	}
	return processID, toStyleID
}

// TestDisjointChangeoverGivesEveryTouchedNodeAnOrder is the second half of
// N1-a, found by the spot-check rather than by the round-3 report: the fix
// covered the nodes a MARK names, and the plan touches more nodes than that.
//
// An Add exists because the cross-mode pass synthesized it, so nothing about it
// is marked — but it still has to be planned, and a node with no row at plan
// time gets no action, then a task with no order, then a cutover it blocks.
func TestDisjointChangeoverGivesEveryTouchedNodeAnOrder(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, toStyleID := seedDisjointPressScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.coreClient = NewCoreClient("http://test-core")

	co, err := eng.StartProcessChangeover(processID, toStyleID, "test", "disjoint")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}
	tasks, err := db.ListChangeoverNodeTasks(co.ID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	got := map[string]processes.NodeTask{}
	for _, task := range tasks {
		node, err := db.GetProcessNode(task.ProcessNodeID)
		if err != nil {
			t.Fatalf("get node: %v", err)
		}
		got[node.CoreNodeName] = task
	}
	for _, node := range []string{"OLD-A", "OLD-B", "NEW-A", "NEW-B"} {
		task, ok := got[node]
		if !ok {
			t.Errorf("no task for %s (tasks: %v)", node, keysOf(got))
			continue
		}
		if task.NextMaterialOrderID == nil && task.OldMaterialReleaseOrderID == nil {
			t.Errorf("%s got a task in state %q and no order at all.\n"+
				"Every node this changeover touches must be planned on the FIRST arm — an Add is "+
				"named by no mark, so a fix scoped to marked nodes leaves it blocking the cutover "+
				"with nothing to do.", node, task.State)
		}
	}
}
