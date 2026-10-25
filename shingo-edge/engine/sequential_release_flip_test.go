package engine

import (
	"testing"

	"shingo/protocol"
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
		&aNodeID, false, 1, "SS-A", "", "", false, "PART-SS", "", "")
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
	eng.applyHoldAndReplay(node, rt, 3, +1, func(*processes.RuntimeState) tickParts { return tickParts{} })
	rt, err = db.GetProcessNodeRuntime(bNodeID)
	testutil.MustNoErr(t, err, "re-read SS-B runtime")
	if rt.PendingUOPDelta != 3 {
		t.Errorf("SS-B pending_uop_delta = %d, want 3 — the count waits for SS-B's bin", rt.PendingUOPDelta)
	}
}

// ── The flip carries A's bucket to B ──
//
// A sequential release moves the line from A to B (commitFlip). Between A's bin
// reaching its pack and the operator's RELEASE the operator was loading B, so
// A's bucket of the part (the strokes a strict bin at its pack held) moves to B
// in the flip, under countMu and in one transaction guarded on both seats: B's
// bound bin takes what fits under B's pack (all of it when B's claim is not
// strict) as one produce_tick, the rest stays in B's bucket capped at one pack
// (the excess dropped with a flip_carry_overflow row), and A's bucket goes to
// 0. An unbound B takes it as its hold, replayed when B's bin binds. The pack is
// 132; A's bin is at the pack with 5 held, unless a pin says otherwise.

// flipCarryPair seeds a sequential produce pair FC-A / FC-B of WIDGET-C (pack
// 132, strict_pack on A and as strictB says on B, each seat naming its claim),
// the line on A with A's bin at 132 and aHeld held of aPart, and B's bin at
// bCount with bHeld of WIDGET-C held (no bin on B when bBound is false).
func flipCarryPair(t *testing.T, aHeld int, aPart string, bBound bool, bCount, bHeld int, strictB bool) (eng *Engine, sink *fakeDeltaSink, a, b *processes.Node) {
	t.Helper()
	db := testEngineDB(t)
	pid, err := db.CreateProcess("FC-PROC", "flip carry", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	styleID, err := db.CreateStyle("FC-STYLE", "", pid)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, db.SetActiveStyle(pid, &styleID), "set active style")
	ids := map[string]int64{}
	for i, c := range []struct {
		own, partner string
		strict       bool
	}{{"FC-A", "FC-B", true}, {"FC-B", "FC-A", strictB}} {
		nid, err := db.CreateProcessNode(processes.NodeInput{
			ProcessID: pid, CoreNodeName: c.own, Code: c.own, Name: c.own, Sequence: i + 1, Enabled: true,
		})
		testutil.MustNoErr(t, err, "create "+c.own)
		claimID, err := upsertClaimRetiredMode(db, processes.NodeClaimInput{
			StyleID: styleID, CoreNodeName: c.own, Role: protocol.ClaimRoleProduce,
			SwapMode: protocol.SwapModeSequential, PayloadCode: "WIDGET-C", UOPCapacity: 132,
			InboundSource: "MARKET", OutboundDestination: "DEST", PairedCoreNode: c.partner,
		})
		testutil.MustNoErr(t, err, "claim "+c.own)
		_, err = db.DB.Exec(`UPDATE style_node_claims SET strict_pack=? WHERE id=?`, c.strict, claimID)
		testutil.MustNoErr(t, err, "strict_pack "+c.own)
		_, err = db.EnsureProcessNodeRuntime(nid)
		testutil.MustNoErr(t, err, "runtime "+c.own)
		testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nid, &claimID, nil, 0), "seat "+c.own)
		ids[c.own] = nid
	}
	binA, binB := int64(7701), int64(7702)
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(ids["FC-A"], &binA), "A's bin")
	testutil.MustNoErr(t, db.UpdateProcessNodeUOP(ids["FC-A"], 132), "A at its pack")
	if aHeld != 0 {
		testutil.MustNoErr(t, processes.AddPendingUOPDelta(db.DB, ids["FC-A"], aHeld, aPart), "A's bucket")
	}
	if bBound {
		testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(ids["FC-B"], &binB), "B's bin")
		testutil.MustNoErr(t, db.UpdateProcessNodeUOP(ids["FC-B"], bCount), "B's count")
	}
	if bHeld != 0 {
		testutil.MustNoErr(t, processes.AddPendingUOPDelta(db.DB, ids["FC-B"], bHeld, "WIDGET-C"), "B's bucket")
	}
	eng = testEngine(t, db)
	eng.wireEventHandlers()
	testutil.MustNoErr(t, eng.writePullSide(ids["FC-A"], ids["FC-B"]), "the line on A")
	sink = &fakeDeltaSink{}
	eng.SetInventoryDeltaSink(sink)
	a, err = db.GetProcessNode(ids["FC-A"])
	testutil.MustNoErr(t, err, "read A")
	b, err = db.GetProcessNode(ids["FC-B"])
	testutil.MustNoErr(t, err, "read B")
	return eng, sink, a, b
}

// flipDeltas is what was emitted on B's bin, each checked to be a
// produce_tick of WIDGET-C.
func flipDeltas(t *testing.T, sink *fakeDeltaSink) []int {
	t.Helper()
	var out []int
	for _, bc := range sink.binCalls {
		if bc.BinID != 7702 || bc.Reason != protocol.ReasonProduceTick || bc.PayloadCode != "WIDGET-C" {
			t.Errorf("delta %+v, want a produce_tick of WIDGET-C on B's bin 7702", bc)
		}
		out = append(out, bc.Delta)
	}
	return out
}

// TestFlipCarry_ABucketMovesOntoTheBoundPartner — A at its pack with 5 held;
// B's bin takes the 5 in one produce_tick, A's bucket goes to 0. A strict B at
// 0 has room for all of it; a non-strict B at 130 takes all 5 (no pack caps
// it).
func TestFlipCarry_ABucketMovesOntoTheBoundPartner(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		bCount, after int
		strictB       bool
	}{
		{"strict B at 0", 0, 5, true},
		{"non-strict B at 130", 130, 135, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng, sink, a, b := flipCarryPair(t, 5, "WIDGET-C", true, tc.bCount, 0, tc.strictB)
			testutil.MustNoErr(t, eng.releaseFlipPartner(a), "release A")
			assertSeat(t, eng, a.ID, 132, 0, "")
			assertSeat(t, eng, b.ID, tc.after, 0, "")
			if d := flipDeltas(t, sink); len(d) != 1 || d[0] != 5 {
				t.Errorf("B's deltas = %v, want one produce_tick of 5", d)
			}
			assertDrops(t, eng.db)
		})
	}
}

// TestFlipCarry_AStrictPartnerTakesOnlyItsRoom — B's bin at 130 has room for
// 2: it takes 2 (one produce_tick), and the other 3 wait in B's bucket.
func TestFlipCarry_AStrictPartnerTakesOnlyItsRoom(t *testing.T) {
	t.Parallel()
	eng, sink, a, b := flipCarryPair(t, 5, "WIDGET-C", true, 130, 0, true)
	testutil.MustNoErr(t, eng.releaseFlipPartner(a), "release A")
	assertSeat(t, eng, a.ID, 132, 0, "")
	assertSeat(t, eng, b.ID, 132, 3, "WIDGET-C")
	if d := flipDeltas(t, sink); len(d) != 1 || d[0] != 2 {
		t.Errorf("B's deltas = %v, want one produce_tick of 2", d)
	}
	assertDrops(t, eng.db)
}

// TestFlipCarry_PastOnePackTheExcessIsDropped — B's bin at its pack already
// holds 130: the 5 carried would make 135, so B holds one pack and the 3 over
// it are dropped with one flip_carry_overflow row.
func TestFlipCarry_PastOnePackTheExcessIsDropped(t *testing.T) {
	t.Parallel()
	eng, sink, a, b := flipCarryPair(t, 5, "WIDGET-C", true, 132, 130, true)
	testutil.MustNoErr(t, eng.releaseFlipPartner(a), "release A")
	assertSeat(t, eng, a.ID, 132, 0, "")
	assertSeat(t, eng, b.ID, 132, 132, "WIDGET-C")
	if len(sink.binCalls) != 0 {
		t.Errorf("deltas = %+v, want none: B's bin had no room", sink.binCalls)
	}
	assertDrops(t, eng.db, protocol.HoldDropped{Reason: protocol.HoldDropFlipCarryOverflow, Qty: 3,
		PayloadCode: "WIDGET-C", Node: "FC-B", CalledBy: "edge:flip"})
}

// TestFlipCarry_AnUnboundPartnerHoldsItForItsBin — no bin on B at the flip:
// the 5 become B's hold, and B's first tick after its bin binds replays them
// (5 + 1 in one produce_tick).
func TestFlipCarry_AnUnboundPartnerHoldsItForItsBin(t *testing.T) {
	t.Parallel()
	eng, sink, a, b := flipCarryPair(t, 5, "WIDGET-C", false, 0, 0, true)
	testutil.MustNoErr(t, eng.releaseFlipPartner(a), "release A")
	assertSeat(t, eng, a.ID, 132, 0, "")
	assertSeat(t, eng, b.ID, 0, 5, "WIDGET-C")
	if len(sink.binCalls) != 0 {
		t.Errorf("deltas = %+v, want none: nothing on B to charge", sink.binCalls)
	}

	bin := int64(7702)
	testutil.MustNoErr(t, eng.db.SetProcessNodeActiveBinID(b.ID, &bin), "B's bin binds")
	proc, err := eng.db.GetProcess(b.ProcessID)
	testutil.MustNoErr(t, err, "read process")
	charSPTick(eng, b.ProcessID, *proc.ActiveStyleID, 1)
	assertSeat(t, eng, b.ID, 6, 0, "")
	if d := flipDeltas(t, sink); len(d) != 1 || d[0] != 6 {
		t.Errorf("B's deltas = %v, want one produce_tick of 6", d)
	}
}

// TestFlipCarry_AForeignBucketStaysOnA — A's bucket is of another part: it is
// not carried (B makes WIDGET-C) and stays on A for a bin of its own part.
func TestFlipCarry_AForeignBucketStaysOnA(t *testing.T) {
	t.Parallel()
	eng, sink, a, b := flipCarryPair(t, 5, "OTHER-PART", true, 0, 0, true)
	testutil.MustNoErr(t, eng.releaseFlipPartner(a), "release A")
	assertSeat(t, eng, a.ID, 132, 5, "OTHER-PART")
	assertSeat(t, eng, b.ID, 0, 0, "")
	if len(sink.binCalls) != 0 {
		t.Errorf("deltas = %+v, want none", sink.binCalls)
	}
	assertDrops(t, eng.db)
}
