package engine

import (
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// sequential_release_flip_test.go — the flip that a release IS (owner
// ruling 2026-09-27, fact-owners Lane G), under the one-press rule (owner,
// 2026-09-30): the line always goes to the partner.
//
// Pin (a) first characterised the two-click order: the trunk refused the
// active side while the partner could not feed. Under the one-press rule
// that refusal is gone for a partner with no bin — the line moves and the
// partner's count waits for its bin — so (a) now pins the release moving the
// line mid-changeover, before the partner's order delivers. Pin (b) is the
// changeover order with the partner delivered first.
//
// The tests below the pins are steady state: a pair with no changeover
// running releases the finished side with no prior flip (the release writes
// the pull side), and a partner with no bin takes the line, its count held
// in pending_uop_delta until its bin binds.

// Pin (a): mid-changeover, SEQ-B's new carrier has not arrived and no bin is
// bound there. RELEASE on SEQ-A moves the line onto SEQ-B and releases SEQ-A.
func TestSequentialReleaseFlip_ReleaseMovesTheLineBeforeThePartnerDelivers(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, _, activeNodeID, parkedNodeID, co := seedSequentialScenario(t, db, false)
	activeOrder, _ := seqTaskOrders(t, db, co.ID, activeNodeID, parkedNodeID)
	testutil.MustNoErr(t, db.UpdateOrderStatus(activeOrder, string(orders.StatusStaged)), "stage")

	if err := eng.ReleaseOrderWithLineside(activeOrder, ReleaseDisposition{CalledBy: "op"}); err != nil {
		t.Fatalf("the trunk refused the active side: %v — the line goes to the partner and its count "+
			"waits for its bin", err)
	}
	if activePullOf(t, db, activeNodeID) || !activePullOf(t, db, parkedNodeID) {
		t.Fatal("the release did not move the line onto SEQ-B")
	}
}

// Pin (b): a changeover's parked-then-active order. The parked side's order
// delivers first; then the depleted active side releases through the trunk
// with no confirm and no prior flip, and the release itself moves the line
// onto the delivered partner.
func TestSequentialReleaseFlip_ChangeoverParkedThenActiveReleaseMovesTheLine(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, _, activeNodeID, parkedNodeID, co := seedSequentialScenario(t, db, false)
	activeOrder, parkedOrder := seqTaskOrders(t, db, co.ID, activeNodeID, parkedNodeID)
	testutil.MustNoErr(t, db.UpdateOrderStatus(activeOrder, string(orders.StatusStaged)), "stage")

	// The parked side's order must be terminal before the readiness guard
	// will let the line move onto it.
	markOrderTerminal(db, parkedOrder)

	if err := eng.ReleaseOrderWithLineside(activeOrder, ReleaseDisposition{CalledBy: "op"}); err != nil {
		t.Fatalf("release of the depleted side with a delivered partner: %v", err)
	}

	if activePullOf(t, db, activeNodeID) {
		t.Error("SEQ-A still active-pull after its release — the released side must be dark")
	}
	if !activePullOf(t, db, parkedNodeID) {
		t.Error("SEQ-B is not active-pull after the release — the partner must be live")
	}
}

// seedSteadyStateSequentialPair builds a sequential A/B pair in STEADY
// STATE: both positions claimed for the running style, the line pulling
// from A, and — the fact flipTargetReady's steady-state arm asks for — a
// bin standing on the partner B. No changeover is started, which is what
// makes this fixture different from seedSequentialScenario (that one always
// runs one, and mid-changeover the partner is never ready).
func seedSteadyStateSequentialPair(t *testing.T, db *store.DB) (eng *Engine, processID, aNodeID, bNodeID, aOrder int64) {
	t.Helper()

	processID, err := db.CreateProcess("SEQ-SS-PROC", "steady-state sequential", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	aNodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "SS-A", Code: "SSA", Name: "Steady A", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node A: %v", err)
	}
	bNodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "SS-B", Code: "SSB", Name: "Steady B", Sequence: 2, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node B: %v", err)
	}
	db.EnsureProcessNodeRuntime(aNodeID)
	db.EnsureProcessNodeRuntime(bNodeID)
	testutil.MustNoErr(t, db.SetActivePull(aNodeID, true), "pull from SS-A")
	testutil.MustNoErr(t, db.SetActivePull(bNodeID, false), "SS-B parked")

	styleID, err := db.CreateStyle("SEQ-SS-STYLE", "steady style", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &styleID), "set active style")

	for _, c := range []struct{ own, partner string }{{"SS-A", "SS-B"}, {"SS-B", "SS-A"}} {
		if _, err := upsertClaimRetiredMode(db, processes.NodeClaimInput{
			StyleID: styleID, CoreNodeName: c.own, Role: "produce", SwapMode: "sequential",
			PayloadCode: "PART-SS", UOPCapacity: 100,
			InboundSource: "MARKET", OutboundDestination: "DEST",
			PairedCoreNode: c.partner,
		}); err != nil {
			t.Fatalf("upsert claim %s: %v", c.own, err)
		}
	}

	// The partner holds a bin — flipTargetReady's steady-state question and
	// the whole reason this release may flip the line onto it.
	binID := int64(4201)
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(bNodeID, &binID), "bin on SS-B")

	// A staged order against the side the line pulls from: the finished bin
	// the operator is about to release.
	aOrder, err = db.CreateOrder("uuid-ss-release", orders.TypeComplex,
		&aNodeID, false, 1, "SS-A", "", "", "", false, "PART-SS", "", "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(aOrder, string(orders.StatusStaged)), "stage")

	eng = testEngine(t, db)
	eng.wireEventHandlers()
	return eng, processID, aNodeID, bNodeID, aOrder
}

// TestSequentialReleaseFlip_SteadyStateReleaseFlipsWithNoPriorFlip is the
// brief's own sentence: "a steady-state release of the finished side
// succeeds with no prior flip". The partner holds a bin and no changeover
// is running, so the answer to "can the partner feed the line" is yes — the
// release proceeds unconfirmed and IT writes the pull side: the released
// side goes dark, the partner goes live.
func TestSequentialReleaseFlip_SteadyStateReleaseFlipsWithNoPriorFlip(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, _, aNodeID, bNodeID, aOrder := seedSteadyStateSequentialPair(t, db)

	if err := eng.ReleaseOrderWithLineside(aOrder, ReleaseDisposition{CalledBy: "op"}); err != nil {
		t.Fatalf("steady-state release of the finished side: %v — the partner holds a bin and no "+
			"changeover is running, so nothing refuses this click", err)
	}

	if activePullOf(t, db, aNodeID) {
		t.Error("SS-A is still active-pull after its release — the released side must go dark")
	}
	if !activePullOf(t, db, bNodeID) {
		t.Error("SS-B is not active-pull after the release — the release is the flip; the partner " +
			"must go live in the same click")
	}
}

// TestSequentialReleaseFlip_PartnerWithNoBinTakesTheLineAndHoldsItsCount
// covers the other half of the model in the steady-state fixture: the partner
// has NO bin. The release is not refused: the line moves to SS-B, and a tick
// after the release is held on SS-B (pending_uop_delta) rather than landing
// on SS-A's departing bin or on no bin at all.
func TestSequentialReleaseFlip_PartnerWithNoBinTakesTheLineAndHoldsItsCount(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, _, aNodeID, bNodeID, aOrder := seedSteadyStateSequentialPair(t, db)

	// Take the bin off the partner: its count must wait for the next one.
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(bNodeID, nil), "no bin on SS-B")

	if err := eng.ReleaseOrderWithLineside(aOrder, ReleaseDisposition{CalledBy: "op"}); err != nil {
		t.Fatalf("release with a partner that has no bin was refused: %v", err)
	}
	if activePullOf(t, db, aNodeID) {
		t.Error("SS-A still active-pull after its release — the released side must go dark")
	}
	if !activePullOf(t, db, bNodeID) {
		t.Error("SS-B not active-pull after the release — the line goes to the partner")
	}

	// The next tick lands on SS-B's hold, to replay when its bin binds.
	node, err := db.GetProcessNode(bNodeID)
	testutil.MustNoErr(t, err, "get SS-B")
	rt, err := db.GetProcessNodeRuntime(bNodeID)
	testutil.MustNoErr(t, err, "get SS-B runtime")
	eng.applyHoldAndReplay(node, rt, 3, +1)
	rt, err = db.GetProcessNodeRuntime(bNodeID)
	testutil.MustNoErr(t, err, "re-read SS-B runtime")
	if rt.PendingUOPDelta != 3 {
		t.Errorf("SS-B pending_uop_delta = %d, want 3 — the count waits for SS-B's bin", rt.PendingUOPDelta)
	}
}
