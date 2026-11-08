package engine

import (
	"errors"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// guardCountRig is commonCellRig on a counting database: a changeover A -> B
// is opened by the test, the PLC on B's 1002, N-COMMON (PART-C under both),
// N-MIRROR (PART-L -> PART-R).
func guardCountRig(t *testing.T) (eng *Engine, counter interface {
	Reset()
	Count() int64
}, pid, a, b int64, nodes map[string]*processes.Node) {
	t.Helper()
	db, c := testEngineDBCounting(t)
	pid, _, a, _ = seedProduceNode(t, db, "two_robot")
	eng = testEngine(t, db)
	eng.plcMgr = seedRigManager()
	testutil.MustNoErr(t, db.SetProcessCounter(pid, "plc", "MES_Cell.Prod_Counter_01", true, "", false), "counter")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(a, "1001"), "pin A")
	b, err := db.CreateStyle("STYLE-B", "", pid)
	testutil.MustNoErr(t, err, "style B")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(b, "1002"), "pin B")
	cm := &catidMonitor{eng: eng, states: map[int64]*catidState{}}
	eng.catidMon = cm
	setLive(cm, pid, "1002")
	cartNode(t, eng, pid, a, "N-COMMON", "PART-C", "PART-C", 9761)
	_, err = upsertClaimRetiredMode(db, processes.NodeClaimInput{StyleID: b, CoreNodeName: "N-COMMON", Role: "consume",
		SwapMode: "simple", PayloadCode: "PART-C", UOPCapacity: 100, InboundSource: "SRC"})
	testutil.MustNoErr(t, err, "B's claim at N-COMMON")
	mirrorNode(t, eng, pid, a, b, "N-MIRROR", "PART-L", "PART-R", 9763)
	ns, err := db.ListProcessNodesByProcess(pid)
	testutil.MustNoErr(t, err, "nodes")
	nodes = map[string]*processes.Node{}
	for i := range ns {
		nodes[ns[i].CoreNodeName] = &ns[i]
	}
	return eng, c, pid, a, b, nodes
}

// openAB opens the changeover A -> B with the given tasks, every task's node a
// task participant.
func openAB(t *testing.T, eng *Engine, pid, a, b int64, tasks map[string]string) {
	t.Helper()
	ns, err := eng.db.ListProcessNodesByProcess(pid)
	testutil.MustNoErr(t, err, "nodes")
	var in []processes.NodeTaskInput
	var parts []domain.ParticipantInput
	for node, situation := range tasks {
		in = append(in, processes.NodeTaskInput{ProcessID: pid, CoreNodeName: node, Situation: situation, State: "swap_required"})
		parts = append(parts, domain.ParticipantInput{CoreNodeName: node, Role: domain.ParticipantRoleTask})
	}
	_, err = eng.changeoverService.Create(pid, &a, b, "test", "", nil, in, parts, ns)
	testutil.MustNoErr(t, err, "the changeover A -> B")
}

// TestGuards_ADoorCallReadsOnce — decision 48. During a changeover the
// sweep's door at a common position asks the participant gate, A2, A5 and A2
// again under the cell's lock; each read the changeover row, the process, both
// styles' claims and the task for itself (21 statements). One door call's
// reads (guardReads) are now shared: the gate reads them (9, its own reads
// included), A2 nothing, A5 only its own two (the style and the process's
// CATID sets), the re-ask the changeover row alone (12). The verdicts are the
// fresh reads' — and the re-ask still sees a changeover armed while the
// request waited for the cell.
func TestGuards_ADoorCallReadsOnce(t *testing.T) {
	t.Parallel()
	eng, counter, pid, a, b, nodes := guardCountRig(t)
	openAB(t, eng, pid, a, b, map[string]string{"N-COMMON": string(SituationUnchanged), "N-MIRROR": string(SituationSwap)})
	common := nodes["N-COMMON"]
	claim := claimOf(t, eng, a, "N-COMMON")

	counter.Reset()
	g := newGuardReads(common)
	ok, why := eng.canAcceptOrdersIn(g, common.ID)
	gate := counter.Count()
	e1 := eng.guardChangeover(g, common, claim)
	a2 := counter.Count() - gate
	e2 := eng.guardCatidMismatchIn(g, common, claim)
	a5 := counter.Count() - gate - a2
	g.recheck(eng)
	e3 := eng.guardChangeover(g, common, claim)
	again := counter.Count() - gate - a2 - a5
	t.Logf("one door call: the gate %d, A2 %d, A5 %d, the re-ask %d — %d statements", gate, a2, a5, again, counter.Count())
	if !ok || e1 != nil || e2 != nil || e3 != nil {
		t.Fatalf("the common position under A: gate %t %q, A2 %v, A5 %v, re-ask %v; want all admitted", ok, why, e1, e2, e3)
	}
	if a2 != 0 || a5 != 2 || again != 1 {
		t.Errorf("A2 %d, A5 %d, the re-ask %d statements; want 0, 2, 1 (the reads shared)", a2, a5, again)
	}
	if fresh, _ := eng.CanAcceptOrders(common.ID); !fresh || eng.guardChangeover(nil, common, claim) != nil ||
		eng.guardCatidMismatch(common, claim) != nil {
		t.Error("the fresh reads disagree with the shared ones")
	}

	mirror := nodes["N-MIRROR"]
	mclaim := claimOf(t, eng, a, "N-MIRROR")
	var armed *ChangeoverArmedError
	if err := eng.guardChangeover(newGuardReads(mirror), mirror, mclaim); !errors.As(err, &armed) {
		t.Errorf("the mirror position under A: %v, want the changeover-armed refusal", err)
	}
}

// TestGuards_TheReaskSeesAChangeoverArmedMeanwhile: a door's reads taken with
// no changeover open, a changeover armed before the re-ask under the cell's
// lock — the re-ask reads the row again and refuses the outgoing style's
// request at the position whose part changes.
func TestGuards_TheReaskSeesAChangeoverArmedMeanwhile(t *testing.T) {
	t.Parallel()
	eng, _, pid, a, b, nodes := guardCountRig(t)
	mirror := nodes["N-MIRROR"]
	claim := claimOf(t, eng, a, "N-MIRROR")
	g := newGuardReads(mirror)
	if err := eng.guardChangeover(g, mirror, claim); err != nil {
		t.Fatalf("no changeover open: %v", err)
	}
	openAB(t, eng, pid, a, b, map[string]string{"N-MIRROR": string(SituationSwap)})
	g.recheck(eng)
	var armed *ChangeoverArmedError
	if err := eng.guardChangeover(g, mirror, claim); !errors.As(err, &armed) {
		t.Errorf("the re-ask after a changeover armed: %v, want the changeover-armed refusal", err)
	}
}

// TestChangesNothingAt_AnAbsentTaskIsAChange — decision 48. Every diff mints a
// task row, the unchanged ones included, so a same-part position with no task
// is one whose claims changed after the changeover was planned: A2 and the
// participant gate refuse it. Before, an absent task read as unchanged.
func TestChangesNothingAt_AnAbsentTaskIsAChange(t *testing.T) {
	t.Parallel()
	eng, _, pid, a, b, nodes := guardCountRig(t)
	ns, err := eng.db.ListProcessNodesByProcess(pid)
	testutil.MustNoErr(t, err, "nodes")
	_, err = eng.changeoverService.Create(pid, &a, b, "test", "", nil, nil,
		[]domain.ParticipantInput{{CoreNodeName: "N-COMMON", Role: domain.ParticipantRoleTask}}, ns)
	testutil.MustNoErr(t, err, "a changeover with no task at N-COMMON")
	common := nodes["N-COMMON"]
	var armed *ChangeoverArmedError
	if err := eng.guardChangeover(nil, common, claimOf(t, eng, a, "N-COMMON")); !errors.As(err, &armed) {
		t.Errorf("A2 at the common position with no task: %v, want the changeover-armed refusal", err)
	}
	if ok, why := eng.CanAcceptOrders(common.ID); ok {
		t.Errorf("the participant gate at the common position with no task: admitted (%q), want refused", why)
	}
}
