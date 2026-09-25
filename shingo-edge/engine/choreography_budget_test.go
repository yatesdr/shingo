package engine

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// choreography_budget_test.go — what one Edge choreography costs the Pi in
// STATEMENTS. EVIDENCE, NOT A PIN.
//
// Stage A's A0 takes these before any change, and A2 and Stage E read them: A2
// changes how often the sourcing snapshot is replaced, and Stage E decides
// whether the two-robot choreography moves to Core on the number (SYNTH F7:
// "~45 statements per two-robot consume" was a reviewer's estimate, never
// measured). Nothing here asserts a count. The numbers are logged under a
// BUDGET: prefix and written into the Stage A BUDGETS.md.
//
// THE ENGINE IS WIRED AS PRODUCTION WIRES IT for the part that costs statements:
// the order manager emits onto the event bus and the event handlers are
// registered, so what an order's creation triggers on the Edge is counted with
// it. The order sender's outbox write is the Edge's wire; the Kafka send that
// drains it is not a statement and is not here.
//
// RUN ON DEMAND: go test -run 'Budget_Edge' -v ./engine/

// budgetEngine is testEngine on a counting store, with the order manager's
// events going to the bus and the handlers wired, as engine.Start does.
func budgetEngine(t *testing.T) (*Engine, *store.DB, *store.QueryCounter) {
	t.Helper()
	db, counter := testEngineDBCounting(t)
	eng := testEngine(t, db)
	eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
	eng.wireEventHandlers()
	return eng, db, counter
}

// logBudget writes one path's count and its breakdown.
func logBudget(t *testing.T, name string, c *store.QueryCounter) {
	t.Helper()
	t.Logf("BUDGET: %s — %s", name, testutil.StatementBreakdown(c.Statements()))
}

// TestBudget_EdgeConsumeTwoRobot: one REQUEST on a two_robot consume cell (the
// operator's button, and the level keeper's ask, which is the same function).
func TestBudget_EdgeConsumeTwoRobot(t *testing.T) {
	eng, db, counter := budgetEngine(t)
	nodeID := seedLineSwapClaim(t, db, "BUD", protocol.SwapModeTwoRobot)

	counter.Reset()
	res, err := eng.RequestNodeMaterial(nodeID, 1)
	testutil.MustNoErr(t, err, "consume REQUEST")
	if res.OrderA == nil || res.OrderB == nil {
		t.Fatalf("fixture: the REQUEST did not build a two-leg swap")
	}
	logBudget(t, "Edge consume, two_robot (RequestNodeMaterial, both legs)", counter)
}

// TestBudget_EdgeProduceTwoRobot: one produce swap on a two_robot produce cell.
func TestBudget_EdgeProduceTwoRobot(t *testing.T) {
	eng, db, counter := budgetEngine(t)
	_, nodeID, _, _ := seedProduceNode(t, db, protocol.SwapModeTwoRobot)

	counter.Reset()
	res, err := eng.RequestProduceSwap(nodeID)
	testutil.MustNoErr(t, err, "produce swap")
	if res.OrderA == nil || res.OrderB == nil {
		t.Fatalf("fixture: the produce swap did not build two legs (order=%v a=%v b=%v)",
			res.Order, res.OrderA, res.OrderB)
	}
	logBudget(t, "Edge produce, two_robot (RequestProduceSwap, both legs)", counter)
}

// TestBudget_EdgeChangeoverTwoRobot: one changeover start on a process whose one
// cell is two_robot in both styles, with a different part in each — the
// SituationSwap shape, the one that mints the two-leg removal-and-supply pair.
func TestBudget_EdgeChangeoverTwoRobot(t *testing.T) {
	eng, db, counter := budgetEngine(t)
	processID, err := db.CreateProcess("BUDCO-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "BUDCO-LINE", Code: "BUDCO", Name: "BUDCO line", Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	claim := func(style, payload string) (int64, int64) {
		styleID, err := db.CreateStyle(style, "", processID)
		testutil.MustNoErr(t, err, "create style "+style)
		claimID, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
			StyleID: styleID, CoreNodeName: "BUDCO-LINE", Role: protocol.ClaimRoleConsume,
			SwapMode: protocol.SwapModeTwoRobot, PayloadCode: payload, UOPCapacity: 100,
			InboundSource: "BUDCO-MARKET", InboundStaging: "BUDCO-IN-STAGE",
			OutboundStaging: "BUDCO-OUT-STAGE", OutboundDestination: "BUDCO-MARKET",
			PairedCoreNode: "BUDCO-BACK",
		})
		testutil.MustNoErr(t, err, "upsert claim "+style)
		return styleID, claimID
	}
	fromStyle, fromClaim := claim("BUDCO-FROM", "PART-FROM")
	toStyle, _ := claim("BUDCO-TO", "PART-TO")
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &fromStyle), "set active style")
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &fromClaim, 50), "set runtime")

	counter.Reset()
	co, err := eng.StartProcessChangeover(processID, toStyle, "budget", "two_robot swap")
	testutil.MustNoErr(t, err, "start changeover")
	task, err := db.GetChangeoverNodeTaskByNode(co.ID, nodeID)
	testutil.MustNoErr(t, err, "read task")
	if task.Situation != "swap" || task.NextMaterialOrderID == nil || task.OldMaterialReleaseOrderID == nil {
		t.Fatalf("fixture: the changeover task is %q with supply %v and removal %v, want a swap with both legs",
			task.Situation, task.NextMaterialOrderID, task.OldMaterialReleaseOrderID)
	}
	logBudget(t, "Edge changeover start, two_robot swap (StartProcessChangeover, one cell)", counter)
}

// TestBudget_EdgeSourcingStateReplace: one full snapshot of Core's sourcing feed
// (HandleSourcingState with Snapshot set), at the full plant and at one
// station's own processes, and one change delta.
//
// demo.yaml has 19 (process, style) pairs across 12 processes; the largest one
// process's share is PRESS-1's 4. Today every Edge replaces the FULL plant's
// rows on each 2-minute full pass (30 an hour); A2 sends each station only its
// own processes, on registration and a 30-minute backstop (~2 an hour).
func TestBudget_EdgeSourcingStateReplace(t *testing.T) {
	eng, _, counter := budgetEngine(t)
	states := func(n int) []protocol.SourcingState {
		out := make([]protocol.SourcingState, n)
		for i := range out {
			out[i] = protocol.SourcingState{ProcessID: fmt.Sprintf("PROC-%d", i/2), StyleID: fmt.Sprintf("STYLE-%d", i),
				Status: "ok", ComputedAt: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
		}
		return out
	}
	for _, c := range []struct {
		name     string
		n        int
		snapshot bool
	}{
		{"sourcing snapshot replace, full plant (demo: 19 rows)", 19, true},
		{"sourcing snapshot replace, one station's own processes (demo PRESS-1: 4 rows)", 4, true},
		{"sourcing change delta, one style", 1, false},
	} {
		counter.Reset()
		eng.HandleSourcingState(protocol.SourcingStateReport{Snapshot: c.snapshot, States: states(c.n)})
		logBudget(t, "Edge "+c.name, counter)
	}
}
