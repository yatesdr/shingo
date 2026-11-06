// ab_pair_fallthrough_test.go — the A/B fallthrough per pair, reciprocal only
// (SYNTH-round3 finding 8): one lit side charged per pair per tick, a dark
// side the safety net only when its partner names it back, and a press-index
// front never touching a pair.
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

// pairRig is a process with one consume style, active, and no nodes yet.
type pairRig struct {
	t     *testing.T
	eng   *Engine
	db    *store.DB
	pid   int64
	style int64
	seq   int
	bin   int64
}

func newPairRig(t *testing.T) *pairRig {
	t.Helper()
	db := testEngineDB(t)
	pid, err := db.CreateProcess("PAIR-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "process")
	style, err := db.CreateStyle("PAIR-STYLE", "", pid)
	testutil.MustNoErr(t, err, "style")
	testutil.MustNoErr(t, db.SetActiveStyle(pid, &style), "active")
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	return &pairRig{t: t, eng: eng, db: db, pid: pid, style: style, bin: 9800}
}

// consume adds a consume position at core naming partner (blank for none) in
// mode, lit or dark, with a cart of 50.
func (r *pairRig) consume(core, partner string, mode protocol.SwapMode, lit bool) int64 {
	r.t.Helper()
	r.seq++
	r.bin++
	nodeID, err := r.db.CreateProcessNode(processes.NodeInput{ProcessID: r.pid, CoreNodeName: core, Code: core, Name: core,
		Sequence: r.seq, Enabled: true})
	testutil.MustNoErr(r.t, err, "node "+core)
	in := processes.NodeClaimInput{StyleID: r.style, CoreNodeName: core, Role: "consume", SwapMode: mode,
		PayloadCode: "PART-P", UOPCapacity: 100, InboundSource: "SRC", PairedCoreNode: partner}
	if mode == protocol.SwapModeTwoRobotPressIndex {
		in.InboundStaging, in.OutboundStaging, in.OutboundDestination = "IN-"+core, "OUT-"+core, "DEST"
	}
	claimID, err := upsertClaimRetiredMode(r.db, in)
	testutil.MustNoErr(r.t, err, "claim "+core)
	_, err = r.db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(r.t, err, "runtime "+core)
	testutil.MustNoErr(r.t, r.db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, binPtr(r.bin), 50), "cart "+core)
	testutil.MustNoErr(r.t, r.db.SetActivePull(nodeID, lit), "pull "+core)
	return nodeID
}

func (r *pairRig) tick(delta int64) {
	r.eng.handleCounterDelta(CounterDeltaEvent{ProcessID: r.pid, StyleID: r.style, Delta: delta})
}

func (r *pairRig) left(nodeID int64) int {
	return runtimeOf(r.t, r.eng, nodeID).RemainingUOPCached
}

// TestFallthrough_TwoPairsEachGetTheirOwnNet: one pair with a lit side, one
// with none — the second gets its own safety net. Before, one flag for the
// whole process: the first pair's lit side disarmed the second's net, and its
// stroke was charged nowhere.
func TestFallthrough_TwoPairsEachGetTheirOwnNet(t *testing.T) {
	t.Parallel()
	r := newPairRig(t)
	p1a := r.consume("P1-A", "P1-B", protocol.SwapModeSequential, true)
	p1b := r.consume("P1-B", "P1-A", protocol.SwapModeSequential, false)
	p2a := r.consume("P2-A", "P2-B", protocol.SwapModeSequential, false)
	p2b := r.consume("P2-B", "P2-A", protocol.SwapModeSequential, false)
	r.tick(3)
	if got := []int{r.left(p1a), r.left(p1b), r.left(p2a), r.left(p2b)}; !(got[0] == 47 && got[1] == 50 && got[2] == 47 && got[3] == 50) {
		t.Errorf("carts = %v, want [47 50 47 50]: the lit side of pair 1, the first dark side of pair 2", got)
	}
}

// TestFallthrough_PressIndexFrontDoesNotDisarmASequentialPair: a press-index
// front names its back position in the same field; it is no pair, and a dark
// pair beside it still gets its net.
func TestFallthrough_PressIndexFrontDoesNotDisarmASequentialPair(t *testing.T) {
	t.Parallel()
	r := newPairRig(t)
	front := r.consume("PF", "PF-BACK", protocol.SwapModeTwoRobotPressIndex, true)
	qa := r.consume("Q-A", "Q-B", protocol.SwapModeSequential, false)
	qb := r.consume("Q-B", "Q-A", protocol.SwapModeSequential, false)
	r.tick(2)
	if got := []int{r.left(front), r.left(qa), r.left(qb)}; !(got[0] == 48 && got[1] == 48 && got[2] == 50) {
		t.Errorf("carts = %v, want [48 48 50]: the press front, and the pair's first dark side", got)
	}
}

// TestFallthrough_BothLitChargesOneSide: a new pair is lit on both sides until
// its first release; a stroke takes one cart, not two.
func TestFallthrough_BothLitChargesOneSide(t *testing.T) {
	t.Parallel()
	r := newPairRig(t)
	a := r.consume("L-A", "L-B", protocol.SwapModeSequential, true)
	b := r.consume("L-B", "L-A", protocol.SwapModeSequential, true)
	r.tick(3)
	r.tick(2)
	if got := []int{r.left(a), r.left(b)}; !(got[0] == 45 && got[1] == 50) {
		t.Errorf("carts = %v, want [45 50]: one side per tick", got)
	}
}

// TestFallthrough_OneWayPairNeverDoubleCharges: A names B, B names nothing.
// That is no pair: after A's release B is charged and A, dark, is not a
// fallback. Before, both carts were charged per stroke.
func TestFallthrough_OneWayPairNeverDoubleCharges(t *testing.T) {
	t.Parallel()
	r := newPairRig(t)
	a := r.consume("W-A", "W-B", protocol.SwapModeSequential, false)
	b := r.consume("W-B", "", protocol.SwapModeSequential, true)
	r.tick(3)
	if got := []int{r.left(a), r.left(b)}; !(got[0] == 50 && got[1] == 47) {
		t.Errorf("carts = %v, want [50 47]: B alone", got)
	}
}

// saveClaim writes one claim through the single-claim door, which ends with
// the store's checks.
func (r *pairRig) saveClaim(core, partner string) error {
	r.t.Helper()
	tx, err := r.db.Begin()
	testutil.MustNoErr(r.t, err, "begin")
	_, _, err = processes.UpsertClaimChecked(tx, domain.CoreNodeKinds{}, processes.NodeClaimInput{StyleID: r.style,
		CoreNodeName: core, Role: "consume", SwapMode: protocol.SwapModeSequential, PayloadCode: "PART-P",
		UOPCapacity: 100, InboundSource: "SRC", PairedCoreNode: partner})
	if err != nil {
		testutil.MustNoErr(r.t, tx.Rollback(), "rollback")
		return err
	}
	return tx.Commit()
}

// TestPairCheck_RefusesAContradictionAllowsOneSideAtATime: a claim naming a
// partner whose claim names a third position is refused at save; a pair built
// one side at a time (the single-claim door) passes through a one-way state.
func TestPairCheck_RefusesAContradictionAllowsOneSideAtATime(t *testing.T) {
	t.Parallel()
	r := newPairRig(t)
	testutil.MustNoErr(t, r.saveClaim("S-A", "S-B"), "A names B, B not yet claimed")
	testutil.MustNoErr(t, r.saveClaim("S-B", ""), "B claimed, naming nothing yet")
	testutil.MustNoErr(t, r.saveClaim("S-B", "S-A"), "B names A: the pair")
	err := r.saveClaim("S-C", "S-A")
	if err == nil || !strings.Contains(err.Error(), "S-C names S-A") {
		t.Fatalf("C naming A, which names B = %v, want refused naming both", err)
	}
}

// TestPairWarning_OneWayPairOnTheStrip: a legacy one-way pair is named on the
// process's strip; a pair that names each other is not.
func TestPairWarning_OneWayPairOnTheStrip(t *testing.T) {
	t.Parallel()
	r := newPairRig(t)
	r.consume("O-A", "O-B", protocol.SwapModeSequential, true)
	r.consume("O-B", "", protocol.SwapModeSequential, false)
	r.consume("R-A", "R-B", protocol.SwapModeSequential, true)
	r.consume("R-B", "R-A", protocol.SwapModeSequential, false)
	items, err := r.eng.CatidHealth(r.pid)
	testutil.MustNoErr(t, err, "health")
	var got []string
	for _, it := range items {
		if it.Kind == catidKindOneWayPair {
			got = append(got, it.Headline)
		}
	}
	if len(got) != 1 || !strings.Contains(got[0], "O-A names O-B") {
		t.Errorf("one-way items = %q, want one for O-A", got)
	}
}
