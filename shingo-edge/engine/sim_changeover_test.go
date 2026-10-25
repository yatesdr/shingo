//go:build sim

package engine

import (
	"context"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingoedge/config"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// The sim operator runs a scheduled changeover as a person at the station does
// (sim_changeover.go): START at its time, CUTOVER once the changeover's own gate
// has read can_complete with no blockers for the operator's reaction time, and a
// refusal read as a person reads it.
//
// RED before sim_changeover.go: the operator has no changeover schedule
// (compile-red: driveChangeovers, newChangeoverRuns).

var coT0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// coOperator is a sim operator over eng with one scheduled changeover of
// process to style at sim time at, on a manual clock started at coT0.
func coOperator(eng *Engine, process, style string, at time.Duration) (*simOperator, *clock.Manual) {
	clk := clock.NewManual(coT0)
	return &simOperator{
		e: eng, clk: clk, ctx: context.Background(),
		ops:     config.SimOperatorsConfig{SwapRelease: 3 * time.Second},
		pending: map[int64]bool{}, releasing: map[int64]bool{}, confirming: map[int64]bool{},
		startedAt:   coT0,
		changeovers: newChangeoverRuns([]config.SimChangeover{{Process: process, ToStyle: style, At: at}}),
	}, clk
}

// seedUnchangedChangeover is a process whose two styles claim its one node
// with the same part: the changeover changes nothing at the node, so its gate
// can complete as soon as it starts.
func seedUnchangedChangeover(t *testing.T, db *store.DB) (processID, toStyleID int64) {
	t.Helper()
	processID, err := db.CreateProcess("SIMCO", "", "", "", false)
	testutil.MustNoErr(t, err, "process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{ProcessID: processID, CoreNodeName: "SIMCO-N", Code: "S1", Name: "SIMCO-N", Sequence: 1, Enabled: true})
	testutil.MustNoErr(t, err, "node")
	fromID, err := db.CreateStyle("SIMCO-A", "", processID)
	testutil.MustNoErr(t, err, "style A")
	toStyleID, err = db.CreateStyle("SIMCO-B", "", processID)
	testutil.MustNoErr(t, err, "style B")
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &fromID), "active A")
	var fromClaim int64
	for _, sid := range []int64{fromID, toStyleID} {
		id, err := upsertClaimRetiredMode(db, processes.NodeClaimInput{
			StyleID: sid, CoreNodeName: "SIMCO-N", Role: "consume", SwapMode: "simple",
			PayloadCode: "PART-S", UOPCapacity: 20, InboundSource: "SRC", OutboundDestination: "DST",
		})
		testutil.MustNoErr(t, err, "claim")
		if sid == fromID {
			fromClaim = id
		}
	}
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &fromClaim, 10), "count")
	return processID, toStyleID
}

func TestSimChangeover_StartsOnTimeAndCutsOverWhenTheGateAllows(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, toStyleID := seedUnchangedChangeover(t, db)
	eng := testEngine(t, db)
	op, clk := coOperator(eng, "SIMCO", "SIMCO-B", 40*time.Minute)

	clk.Advance(39 * time.Minute)
	op.driveChangeovers()
	if co, err := db.GetActiveProcessChangeover(processID); err == nil && co != nil {
		t.Fatalf("a changeover started at sim-minute 39, before its time (40)")
	}

	clk.Advance(time.Minute)
	op.driveChangeovers()
	co, err := db.GetActiveProcessChangeover(processID)
	if err != nil || co == nil {
		t.Fatalf("no changeover at its time: %v", err)
	}
	if co.ToStyleID != toStyleID || co.CalledBy != "sim-operator" {
		t.Errorf("changeover to %d called by %q, want %d by sim-operator", co.ToStyleID, co.CalledBy, toStyleID)
	}
	if can, blockers, err := eng.ChangeoverGateStatus(processID); err != nil || !can || len(blockers) > 0 {
		t.Fatalf("fixture: the gate does not allow the cutover (%v, %v, %v)", can, blockers, err)
	}

	// The gate allows it: the operator notes it, waits its reaction time, presses.
	op.driveChangeovers()
	if co, _ := db.GetActiveProcessChangeover(processID); co == nil {
		t.Fatal("CUTOVER pressed on the first sweep that saw the gate open; want the reaction time first")
	}
	clk.Advance(3 * time.Second)
	op.driveChangeovers()
	if co, err := db.GetActiveProcessChangeover(processID); err == nil && co != nil {
		t.Fatalf("still in changeover %d after the reaction time with the gate open", co.ID)
	}
	p, err := db.GetProcess(processID)
	testutil.MustNoErr(t, err, "process")
	if p.ActiveStyleID == nil || *p.ActiveStyleID != toStyleID {
		t.Errorf("active style %v, want %d", p.ActiveStyleID, toStyleID)
	}
	if op.changeovers[0].state != coDone {
		t.Errorf("run state %d, want done", op.changeovers[0].state)
	}
}

func TestSimChangeover_NoCutoverWhileTheGateBlocks(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, _ := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	p, err := db.GetProcess(processID)
	testutil.MustNoErr(t, err, "process")
	styles, err := db.ListStylesByProcess(processID)
	testutil.MustNoErr(t, err, "styles")
	target := ""
	for _, s := range styles {
		if p.ActiveStyleID == nil || s.ID != *p.ActiveStyleID {
			target = s.Name
		}
	}
	op, clk := coOperator(eng, p.Name, target, 0)
	op.driveChangeovers()
	co, err := db.GetActiveProcessChangeover(processID)
	if err != nil || co == nil {
		t.Fatalf("no changeover started: %v", err)
	}
	if can, _, _ := eng.ChangeoverGateStatus(processID); can {
		t.Fatal("fixture: the gate allows the cutover; the test checks nothing")
	}
	for i := 0; i < 5; i++ {
		clk.Advance(10 * time.Second)
		op.driveChangeovers()
	}
	if co, _ := db.GetActiveProcessChangeover(processID); co == nil {
		t.Error("the changeover was cut over while its gate named blockers")
	}
}

// A START refused over a carrier on its way is waited out and pressed again;
// one refused over something nothing at the screen can clear is left.
func TestSimChangeover_ARefusalIsReadAsAPersonReadsIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		status protocol.Status
		left   bool
	}{
		{"a carrier on its way is waited out", protocol.StatusInTransit, false},
		{"a faulted order is left", protocol.StatusFaulted, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			processID, nodeID, _, _, _, _ := seedChangeoverScenario(t, db)
			eng := testEngine(t, db)
			id, err := db.CreateOrder("co-blocker", "retrieve", &nodeID, false, 1, "CO-NODE", "", "SOURCE-OLD", protocol.Height{}, false, "PART-OLD", "", "")
			testutil.MustNoErr(t, err, "the blocking order")
			testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(c.status)), "its status")
			op, clk := coOperator(eng, "CO-PROC", "Style-TO", 0)

			op.driveChangeovers()
			if co, err := db.GetActiveProcessChangeover(processID); err == nil && co != nil {
				t.Fatalf("fixture: START was accepted with order %d %s at the node", id, c.status)
			}
			want := coWaiting
			if c.left {
				want = coLeft
			}
			if got := op.changeovers[0].state; got != want {
				t.Fatalf("after the refusal the run is %d, want %d", got, want)
			}
			if c.left {
				return
			}
			testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(protocol.StatusConfirmed)), "the carrier lands")
			clk.Advance(10 * time.Second)
			op.driveChangeovers()
			if co, err := db.GetActiveProcessChangeover(processID); err != nil || co == nil {
				t.Errorf("START was not pressed again once the carrier landed: %v", err)
			}
		})
	}
}
