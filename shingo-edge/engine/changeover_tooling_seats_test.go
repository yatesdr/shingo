package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// ---------------------------------------------------------------------------
// N1-a — THE FIRST CHANGEOVER MUST SEE EVERY SEAT.
//
// A press seat that owns no style_node_claims row of its own also owns no
// process_nodes row until something creates one, and the thing that creates one
// is ChangeoverService.Create — which runs AFTER the plan is built. So on the
// FIRST changeover of any marked press, the paired seat was invisible to
// planning: no clearance, no hold, no order, no warning. On the second it
// worked, because the first had left the row behind.
//
// The sim proved it twice on a pristine seed (SIM-REPORT-n1-2026-08-24, N1-a):
// two previews minutes apart, one action then two, with nothing changed but the
// existence of the row. It is the same disease N1 itself was — a marked seat
// silently getting no treatment — one layer down.
// ---------------------------------------------------------------------------

// seedMarkedPressScenario builds a press-index cell whose PAIRED seat has no
// process_nodes row, which is the shipped state of every press before its first
// changeover: demo.yaml declares the station, the Edge row is created on demand.
func seedMarkedPressScenario(t *testing.T, db *store.DB) (processID, fromStyleID, toStyleID int64) {
	t.Helper()

	processID, err := db.CreateProcess("SEAT-PRESS", "marked press", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	// ONLY the front seat gets a row. PRESS-B is named by the claims and by
	// nothing else — exactly the shipped condition.
	if _, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "PRESS-A", Code: "PRESS-A", Name: "PRESS-A",
		Sequence: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("create front node: %v", err)
	}

	fromStyleID, err = db.CreateStyle("SEAT-FROM", "outgoing, marks both seats", processID)
	if err != nil {
		t.Fatalf("create from style: %v", err)
	}
	toStyleID, err = db.CreateStyle("SEAT-TO", "incoming, stages", processID)
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
		ChangeoverEvacSeats: &[]string{domain.EvacSeatFront, domain.EvacSeatPaired},
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

// TestToolingFirstChangeoverPreviewsEverySeat is the two-previews evidence as an
// assertion: the operator's preview must show the work the changeover will
// actually do, on the first arm as on the second.
func TestToolingFirstChangeoverPreviewsEverySeat(t *testing.T) {
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
	for _, seat := range []string{"PRESS-A", "PRESS-B"} {
		if !covered[seat] {
			t.Errorf("preview does not cover marked seat %s (covers %v).\n"+
				"A seat with no process_nodes row is still a seat with a bin on it. The row is "+
				"created by ChangeoverService.Create, which runs after planning — so on the "+
				"FIRST changeover of a press the paired seat was silently skipped.",
				seat, keysOf(covered))
		}
	}
}

// TestToolingFirstChangeoverGivesEverySeatAnOrder is the same defect one layer
// further down, and it is the one that costs material: even where the plan
// covers a seat, the applier drops its action unless the seat also has a node
// TASK — and tasks are built from the diffs, which never mention an expanded
// seat.
func TestToolingFirstChangeoverGivesEverySeatAnOrder(t *testing.T) {
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
	for _, seat := range []string{"PRESS-A", "PRESS-B"} {
		task, ok := byNode[seat]
		if !ok {
			t.Errorf("marked seat %s has no changeover node task (tasks exist for %v).\n"+
				"applyChangeoverPlan finds a task by node id and skips the action when there is "+
				"none, so a seat with no task gets no order however well it was planned.",
				seat, keysOf(byNode))
			continue
		}
		if task.NextMaterialOrderID == nil {
			t.Errorf("marked seat %s got a task in state %q but no order.\n"+
				"Its bin is in the way of the setup and its replacement must hold at staging; "+
				"neither happens without a leg.", seat, task.State)
		}
	}
}

// TestToolingSeatsMaterializeOnceIsIdempotent guards the obvious way to get the
// fix wrong: creating the row on every plan would duplicate process_nodes, and
// this schema has been through one duplicate-node cleanup already
// (collapseDuplicateProcessNodes) after PLC ticks were counted three times.
func TestToolingSeatsMaterializeOnceIsIdempotent(t *testing.T) {
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
	for seat, n := range count {
		if n != 1 {
			t.Errorf("process node %s exists %d times — seat materialization must be idempotent", seat, n)
		}
	}
	if count["PRESS-B"] != 1 {
		t.Errorf("PRESS-B has %d process_nodes rows after a preview, a preview and a start; want exactly 1",
			count["PRESS-B"])
	}
}

// TestDeliverMaterialForSeatDeliversSomething is the remedy hole: the button an
// operator presses to unblock a seat returned 200, marked the task released,
// and delivered NOTHING.
//
// SynthesizePressPositionClaim clears InboundStaging — deliberately, because the
// diff pipeline relies on a synthesized Add falling through to a direct retrieve
// that the tooling decorator then adds the hold to. But
// DeliverNewMaterialForChangeover read that same cleared field as "this node
// does not stage", took the no-staging branch, and marked the seat released
// while its bin never moved. Observed twice on the sim, both shapes.
//
// The staging node is a property of the CELL, not of one position in it, so the
// seat's answer is its parent's answer.
func TestDeliverMaterialForSeatDeliversSomething(t *testing.T) {
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
	seatNode, err := db.GetProcessNodeByCoreNodeName("PRESS-B")
	if err != nil || seatNode == nil {
		t.Fatalf("paired seat has no node after start: %v", err)
	}
	// Stand in for the seat's leg being gone — cancelled, abandoned, or never
	// created — which is the only reason an operator reaches for this button.
	if _, err := db.DB.Exec(
		`UPDATE changeover_node_tasks SET next_material_order_id=NULL WHERE process_changeover_id=? AND process_node_id=?`,
		co.ID, seatNode.ID); err != nil {
		t.Fatalf("clear seat leg: %v", err)
	}

	order, err := eng.DeliverNewMaterialForChangeover(processID, seatNode.ID)
	if err != nil {
		t.Fatalf("deliver-material on a marked seat: %v", err)
	}
	if order == nil {
		task, terr := db.GetChangeoverNodeTaskByNode(co.ID, seatNode.ID)
		state := "?"
		if terr == nil && task != nil {
			state = string(task.State)
		}
		t.Fatalf("deliver-material created no order for seat PRESS-B and left the task %q.\n"+
			"The cell stages its inbound material, so a seat of that cell stages too — reporting "+
			"success while delivering nothing is how the operator's one remedy became a no-op.",
			state)
	}
	steps, err := db.GetOrderStepsJSON(order.ID)
	if err != nil {
		t.Fatalf("read steps for order %d: %v", order.ID, err)
	}
	if !strings.Contains(steps, "IN-STAGE") {
		t.Errorf("seat delivery does not come from the staging node: steps = %q", steps)
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
