package engine

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// sequential_release_flip_test.go — the flip that a release IS (owner
// ruling 2026-09-27, fact-owners Lane G).
//
// Pins (a) and (b) were written FIRST, at the pre-change tree, to
// characterise the two-click order (flip, then release) so the predicted
// diff (evidence-lead/predicted-diff-laneG.md) could name exactly what
// changes when the flip moves into the release itself. They still pass
// post-change: (a) because mid-changeover the partner's order has not
// delivered (flipTargetReady's reason), so the refusal stays — only its
// sentence changed. (b) drove an explicit, since deleted, Engine.FlipABNode before the
// release; the flip door was deleted with its last caller (FixH), so (b) now
// pins the changeover order without it: the parked side's order delivers,
// and the active side's release moves the line with no prior flip.
//
// The tests below the pins are the new behaviour: a steady-state pair with
// no changeover running releases the finished side with NO prior flip (the
// release writes the pull side), and a partner that cannot feed the line
// refuses with the reason and confirms through — the confirm releases AND
// still writes the pull side.

// Pin (a): TODAY the trunk refuses the active side's order outright — no
// flip has happened, the line is pulling from SEQ-A — and the refusal names
// the position so the operator knows which aisle to look at.
func TestSequentialReleaseFlip_TodayTrunkRefusesActiveSideWithoutFlip(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, _, activeNodeID, parkedNodeID, co := seedSequentialScenario(t, db, false)
	activeOrder, _ := seqTaskOrders(t, db, co.ID, activeNodeID, parkedNodeID)
	testutil.MustNoErr(t, db.UpdateOrderStatus(activeOrder, string(orders.StatusStaged)), "stage")

	err := eng.ReleaseOrderWithLineside(activeOrder, ReleaseDisposition{CalledBy: "op"})
	if err == nil {
		t.Fatal("the trunk released the position the line is pulling from with no flip and no " +
			"confirm — the guard is gone")
	}
	if !strings.Contains(err.Error(), "SEQ-A") {
		t.Fatalf("the refusal does not name the pulling position: %v", err)
	}
	// And the pair is untouched — a refused click writes nothing.
	if !activePullOf(t, db, activeNodeID) || activePullOf(t, db, parkedNodeID) {
		t.Fatal("the refused release changed the pull bits — a refusal must not write the pair")
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

	processID, err := db.CreateProcess("SEQ-SS-PROC", "steady-state sequential", "active_production", "", "", false)
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

// TestSequentialReleaseFlip_PartnerNotReadyRefusesThenConfirmsThrough covers
// the other half of the model in the steady-state fixture: the partner has
// NO bin, so it cannot feed the line. The refusal must carry the reason
// (not a pull-bit sentence) and end with the confirm marker the UI keys on;
// the confirm releases anyway AND the flip still happens — the operator's
// confirm answered the partner's readiness, not the line's move.
func TestSequentialReleaseFlip_PartnerNotReadyRefusesThenConfirmsThrough(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, _, aNodeID, bNodeID, aOrder := seedSteadyStateSequentialPair(t, db)

	// Take the bin off the partner: it now cannot feed the line.
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(bNodeID, nil), "no bin on SS-B")

	err := eng.ReleaseOrderWithLineside(aOrder, ReleaseDisposition{CalledBy: "op"})
	if err == nil {
		t.Fatal("released the pulled side while the partner had nothing to feed the line — the " +
			"guard's question is gone")
	}
	if !strings.Contains(err.Error(), "SS-B") || !strings.Contains(err.Error(), "no bin") {
		t.Fatalf("refusal does not carry the partner's reason: %v", err)
	}
	if !strings.Contains(err.Error(), "confirm to release anyway") {
		t.Fatalf("refusal does not end with the confirm marker the UI keys on: %v", err)
	}
	if activePullOf(t, db, bNodeID) || !activePullOf(t, db, aNodeID) {
		t.Fatal("the refused release wrote the pair — a refusal must not flip anything")
	}

	// The second click: confirm releases anyway AND flips.
	if err := eng.ReleaseOrderWithLineside(aOrder, ReleaseDisposition{CalledBy: "op", ConfirmActivePull: true}); err != nil {
		t.Fatalf("confirmed release: %v", err)
	}
	if activePullOf(t, db, aNodeID) {
		t.Error("SS-A still active-pull after a confirmed release — the confirm answered readiness, " +
			"not the flip; the release must still write the pull side")
	}
	if !activePullOf(t, db, bNodeID) {
		t.Error("SS-B not active-pull after a confirmed release — the line moved; the pair must say so")
	}
}
