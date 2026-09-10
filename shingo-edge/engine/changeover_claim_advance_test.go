package engine

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// changeover_claim_advance_test.go — the missing line in the twin.
//
// applyStagedDelivery has always advanced the node's claim when the incoming
// style's material lands at a staging slot. applyChangeoverRelease, its
// declared twin for the DIRECT delivery, advanced the task state and nothing
// else. 82% of consume-node changeovers at Springfield take their material
// before the changeover completes — median 18 minutes ahead of it — so the
// pointer sat on the outgoing style's claim for the whole gap, and at ALN_007
// for two days and six hours.

// seedDirectChangeover builds a changeover whose to-claim has NO InboundStaging,
// so the supply order's completion lands on changeover_release rather than
// staged_delivery.
func seedDirectChangeover(t *testing.T, db *store.DB) (processID, nodeID, toStyleID, fromClaimID, toClaimID int64) {
	t.Helper()
	processID, err := db.CreateProcess("DIRECT-CO", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "ALN_007", Code: "D1",
		Name: "Direct Node", Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")

	fromStyleID, err := db.CreateStyle("DIRECT-FROM", "", processID)
	testutil.MustNoErr(t, err, "create from style")
	toStyleID, err = db.CreateStyle("DIRECT-TO", "", processID)
	testutil.MustNoErr(t, err, "create to style")
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &fromStyleID), "set active style")

	fromClaimID, err = upsertClaimRetiredMode(db, processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: "ALN_007", Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeSimple, PayloadCode: "74871-6SA1A.06", UOPCapacity: 4500,
		InboundSource: "SOURCE-OLD", OutboundDestination: "DEST-OLD",
	})
	testutil.MustNoErr(t, err, "upsert from claim")

	// No InboundStaging: the supply lands at the node itself, which is what
	// puts its completion on changeover_release rather than staged_delivery.
	toClaimID, err = upsertClaimRetiredMode(db, processes.NodeClaimInput{
		StyleID: toStyleID, CoreNodeName: "ALN_007", Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeSimple, PayloadCode: "63125-6TA0A.06", UOPCapacity: 4500,
		InboundSource: "SOURCE-NEW", OutboundDestination: "DEST-NEW",
	})
	testutil.MustNoErr(t, err, "upsert to claim")

	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &fromClaimID, 3000), "seed runtime on the from-claim")
	return processID, nodeID, toStyleID, fromClaimID, toClaimID
}

// releaseCtx starts the changeover through the real planner and builds the
// completion context for its supply order, exactly as loadOrderCompletionCtx
// would when that order completes.
func releaseCtx(t *testing.T, eng *Engine, db *store.DB, processID, nodeID, toStyleID int64) *orderCompletionCtx {
	t.Helper()
	_, task := startChangeover(t, eng, db, processID, toStyleID)
	if task.NextMaterialOrderID == nil {
		t.Fatalf("fixture: the changeover planned no supply order for this node (situation=%q)", task.Situation)
	}
	order, err := db.GetOrder(*task.NextMaterialOrderID)
	testutil.MustNoErr(t, err, "read supply order")
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "read node")
	runtime, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")

	return &orderCompletionCtx{
		order: order, node: node, runtime: runtime, e: eng,
		toStyleID: toStyleID, nodeTask: task,
	}
}

// THE FIX. The incoming style's material has landed at the node; the changeover
// has not completed, so requestedClaimAtNode still answers with the outgoing
// style. The release is where the intent catches up.
func TestApplyChangeoverRelease_AdvancesTheClaimToTheIncomingStyle(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	processID, nodeID, toStyleID, fromClaimID, toClaimID := seedDirectChangeover(t, db)
	ctx := releaseCtx(t, eng, db, processID, nodeID, toStyleID)

	if !matchChangeoverRelease(ctx) {
		t.Fatal("the fixture does not reach changeover_release — the direct path is what is under test")
	}
	applyChangeoverRelease(eng, ctx)

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.ActiveClaimID == nil {
		t.Fatal("claim pointer cleared — the incoming style DOES claim this node")
	}
	if *rt.ActiveClaimID == fromClaimID {
		t.Fatalf("claim still on the outgoing style (%d). This is ALN_007: the reporter joins on "+
			"this pointer, and it named 74871-6SA1A.06 — of which zero existed plant-wide — for "+
			"two days and six hours.", fromClaimID)
	}
	if *rt.ActiveClaimID != toClaimID {
		t.Errorf("ActiveClaimID = %d, want %d (the to-style's claim at this node)", *rt.ActiveClaimID, toClaimID)
	}
	if rt.RemainingUOPCached != 3000 {
		t.Errorf("RemainingUOPCached = %d, want 3000. The advance is about whose claim the node "+
			"is on; the delivery already wrote the count from Core's envelope.", rt.RemainingUOPCached)
	}
}

// NIL IS AN ANSWER. The incoming style does not claim this node — ALN_002 and
// ALN_004's shape. Leaving the pointer on the outgoing style is the stale
// pointer this function exists to end.
func TestApplyChangeoverRelease_ClearsWhenTheIncomingStyleClaimsNothingHere(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	processID, nodeID, toStyleID, fromClaimID, toClaimID := seedDirectChangeover(t, db)
	ctx := releaseCtx(t, eng, db, processID, nodeID, toStyleID)

	// The incoming style stops claiming this node — ALN_002/ALN_004's shape,
	// where the running style claims the node not at all. The lookup then
	// answers "no row", which is an answer.
	testutil.MustNoErr(t, db.DeleteStyleNodeClaim(toClaimID), "drop the to-claim")
	ctx.toClaimResolved = false
	ctx.toClaim = nil

	applyChangeoverRelease(eng, ctx)

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.ActiveClaimID != nil {
		t.Fatalf("ActiveClaimID = %d, want nil. The incoming style claims this node not at all, "+
			"so nil is the honest answer — requestedClaimAtNode's own doc rules it so. %d is the "+
			"outgoing style's claim left standing.", *rt.ActiveClaimID, fromClaimID)
	}
}

// A FAILED LOOKUP IS NOT AN ANSWER. Clearing on a read error would take a node
// off its claim for a reason that has nothing to do with the node.
func TestApplyChangeoverRelease_LeavesThePointerAloneWhenTheLookupFails(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	processID, nodeID, toStyleID, fromClaimID, _ := seedDirectChangeover(t, db)
	ctx := releaseCtx(t, eng, db, processID, nodeID, toStyleID)

	// Seat the cache as a failed read rather than a missing row.
	ctx.toClaimResolved = true
	ctx.toClaim = nil
	ctx.toClaimErr = errors.New("database is locked")

	applyChangeoverRelease(eng, ctx)

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.ActiveClaimID == nil || *rt.ActiveClaimID != fromClaimID {
		t.Fatalf("ActiveClaimID = %v, want %d unchanged. sql.ErrNoRows means the style claims "+
			"nothing here; any other error means nobody could tell, and the two must not act alike.",
			rt.ActiveClaimID, fromClaimID)
	}
}

// The discrimination itself, in one assertion: a missing row is an answer, a
// read failure is not.
func TestToStyleClaimsNothingHere_TellsNoRowFromNoAnswer(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	processID, nodeID, toStyleID, _, toClaimID := seedDirectChangeover(t, db)
	ctx := releaseCtx(t, eng, db, processID, nodeID, toStyleID)

	if ctx.ToStyleClaimsNothingHere() {
		t.Error("a to-style that DOES claim this node read as claiming nothing")
	}

	testutil.MustNoErr(t, db.DeleteStyleNodeClaim(toClaimID), "drop the to-claim")
	ctx.toClaimResolved = false
	ctx.toClaim = nil
	if !ctx.ToStyleClaimsNothingHere() {
		t.Error("a to-style with no claim at this node must read as an answer")
	}

	ctx.toClaimResolved = true
	ctx.toClaimErr = errors.New("disk I/O error")
	if ctx.ToStyleClaimsNothingHere() {
		t.Error("a failed read must not read as an answer")
	}
}

// THE AB-CYCLING SIDE EFFECT. The flip readiness check compares the node's
// claim pointer against the to-style's claim and refuses when they differ. With
// the incoming carrier already delivered and the pointer left on the outgoing
// style, it refused a flip that was ready — telling the operator to release
// material they had already released.
func TestFlipReadiness_StopsRefusingOnceTheClaimAdvances(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	processID, nodeID, toStyleID, fromClaimID, _ := seedDirectChangeover(t, db)
	ctx := releaseCtx(t, eng, db, processID, nodeID, toStyleID)

	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "read node")

	// The incoming carrier arrived early: the material is here and it is the
	// incoming style's, but the pointer is still on the outgoing claim.
	// The order has delivered; the readiness check gets past its first arm.
	markOrderTerminal(db, ctx.order.ID)
	const binID int64 = 4500
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeForDeliveredBin(nodeID, &fromClaimID, binID, 1, 4500),
		"seed the stale pointer over a full carrier")
	if reason := eng.flipTargetReady(node); reason == "" {
		t.Fatal("fixture: the stale pointer should still refuse — otherwise this test proves nothing")
	}

	ctx.runtime, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "re-read runtime")
	applyChangeoverRelease(eng, ctx)

	if reason := eng.flipTargetReady(node); reason != "" {
		t.Errorf("flip still refused after the claim advanced: %q. The material is here and it is "+
			"the incoming style's; the refusal was reading a pointer nobody had moved.", reason)
	}
}

// THE CLEAR IS AN IDENTITY EVENT. Before this it was not one: the previous
// occupant's part number stayed on the row after the operator emptied the
// carrier, and binAtNode's fallback then stamped the claim's part on ticks
// against it.
func TestClearBin_RecordsAKnownEmptyCarrier(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _, _, _ := seedDirectChangeover(t, db)

	eng.recordLinesideCarrier(nodeID, "ALN_007", domain.KnownCarrier("63125-6TA0A.06"), domain.CarrierFromDelivery)
	// ClearBin's Core round-trip is not what is under test; the doorway call it
	// makes is. Assert the shape ClearBin writes.
	eng.recordLinesideCarrier(nodeID, "ALN_007", domain.KnownCarrier(""), domain.CarrierFromOperator)

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.LinesidePayloadCode != "" {
		t.Errorf("LinesidePayloadCode = %q, want empty — the operator emptied this carrier", rt.LinesidePayloadCode)
	}
	if !rt.LinesidePayloadKnown {
		t.Fatal("LinesidePayloadKnown = false. An empty carrier the operator is looking at is an " +
			"ANSWER; writing it as unknown re-arms binAtNode's claim fallback and puts the claim's " +
			"part straight back on it.")
	}
	if rt.LinesideSource != string(domain.CarrierFromOperator) {
		t.Errorf("LinesideSource = %q, want operator", rt.LinesideSource)
	}
	if rt.LinesideAt.IsZero() {
		t.Error("LinesideAt not stamped — identity staleness is a different question from the count's")
	}
}

// binAtNode's fallback asks the known bit, not the empty string. A cleared
// carrier must not be re-stamped with the claim's part number.
func TestBinAtNode_KnownEmptyCarrierDoesNotFallBackToTheClaim(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _, fromClaimID, _ := seedDirectChangeover(t, db)

	const binID int64 = 4242
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeForDeliveredBin(nodeID, &fromClaimID, binID, 1, 0), "bind carrier")
	eng.recordLinesideCarrier(nodeID, "ALN_007", domain.KnownCarrier(""), domain.CarrierFromOperator)

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	claim, err := db.GetStyleNodeClaim(fromClaimID)
	testutil.MustNoErr(t, err, "read claim")

	gotBin, gotPayload, _ := eng.binAtNode(rt, claim)
	if gotBin != binID {
		t.Fatalf("bin = %d, want %d", gotBin, binID)
	}
	if gotPayload == claim.PayloadCode {
		t.Errorf("payload = %q — the claim's part number came back on a carrier the operator "+
			"emptied. That is the cross product this pair was split to end.", gotPayload)
	}
	if gotPayload != "" {
		t.Errorf("payload = %q, want empty", gotPayload)
	}
}

// A carrier nobody has identified still takes the claim fallback, which is the
// behaviour every row written before the known bit existed already had.
func TestBinAtNode_UnestablishedCarrierStillFallsBackToTheClaim(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _, fromClaimID, _ := seedDirectChangeover(t, db)

	const binID int64 = 4243
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeForDeliveredBin(nodeID, &fromClaimID, binID, 1, 100), "bind carrier")

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	claim, err := db.GetStyleNodeClaim(fromClaimID)
	testutil.MustNoErr(t, err, "read claim")

	if _, gotPayload, _ := eng.binAtNode(rt, claim); gotPayload != claim.PayloadCode {
		t.Errorf("payload = %q, want %q — an older Core, or a node not delivered to since the "+
			"upgrade, must keep sending the value it always sent", gotPayload, claim.PayloadCode)
	}
}

// SWITCH DOES NOT WRITE OVER A MEASUREMENT. When a carrier is bound the count on
// the row is a measurement of something physically standing there; both the old
// capacity seed and a bare zero destroy it.
func TestSwitchNode_KeepsTheCountOfABoundCarrier(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	processID, nodeID, _, toStyleID, fromClaimID, _ := seedChangeoverScenario(t, db)
	eng.wireEventHandlers()
	_, _ = startChangeover(t, eng, db, processID, toStyleID)

	const binID int64 = 777
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeForDeliveredBin(nodeID, &fromClaimID, binID, 1, 1750),
		"a carrier with 1750 parts is standing here")

	testutil.MustNoErr(t, eng.SwitchNodeToTarget(processID, nodeID), "switch")

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.RemainingUOPCached != 1750 {
		t.Errorf("RemainingUOPCached = %d, want 1750. 200 is the to-claim's capacity seed coming "+
			"back; 0 is the opposite lie about the same carrier. Advance the claim, leave the "+
			"measurement alone.", rt.RemainingUOPCached)
	}
}

// THE FIFTH DOOR. A Core-admin order has no Edge row, so the delivery resolves
// its node by dot-name and binds the runtime "exactly as for a normal
// delivery" — which did not include the carrier's identity. The envelope
// carried the payload the whole way and the fallback emit dropped it, so a
// straight-drop during a changeover produced the incident's mechanism with no
// identity record to correct it from.
func TestFallbackDelivered_RecordsTheCarrier(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _, _, _ := seedDirectChangeover(t, db)

	payload := "63125-6TA0A.06"
	binID := int64(9001)
	eng.handleFallbackDelivered(OrderDeliveredEvent{
		BinID:          &binID,
		BinPayloadCode: &payload,
		BinEpoch:       3,
		DeliveryNode:   "ALN_007",
	})

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.ActiveBinID == nil || *rt.ActiveBinID != binID {
		t.Fatalf("ActiveBinID = %v, want %d — the fallback bind itself must still work", rt.ActiveBinID, binID)
	}
	if string(rt.LinesidePayloadCode) != payload {
		t.Errorf("LinesidePayloadCode = %q, want %q. Core named the carrier on the envelope; "+
			"this is the one delivery path with no Edge order row to reconstruct it from.",
			rt.LinesidePayloadCode, payload)
	}
	if !rt.LinesidePayloadKnown {
		t.Error("LinesidePayloadKnown = false for an identity Core stated outright")
	}
	if rt.LinesideSource != string(domain.CarrierFromDelivery) {
		t.Errorf("LinesideSource = %q, want delivery", rt.LinesideSource)
	}
}

// An older Core sends no payload on the fallback envelope. That records as an
// UNKNOWN carrier rather than being skipped: leaving the previous occupant's
// identity standing would be a confident wrong answer about this one.
func TestFallbackDelivered_NoPayloadClearsRatherThanInherits(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _, _, _ := seedDirectChangeover(t, db)

	eng.recordLinesideCarrier(nodeID, "ALN_007", domain.KnownCarrier("PREVIOUS-OCCUPANT"), domain.CarrierFromDelivery)
	binID := int64(9002)
	eng.handleFallbackDelivered(OrderDeliveredEvent{BinID: &binID, BinEpoch: 4, DeliveryNode: "ALN_007"})

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.LinesidePayloadKnown {
		t.Error("LinesidePayloadKnown = true with no payload on the envelope")
	}
	if rt.LinesidePayloadCode == "PREVIOUS-OCCUPANT" {
		t.Error("the previous occupant's identity survived a new carrier landing on the node")
	}
}
