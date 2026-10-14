package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// changeover_already_done_test.go — a changeover planned again plans each node
// from what stands on it.
//
// The style flips only at cutover, so a changeover cancelled after some nodes
// swapped, and started again, planned those nodes from the outgoing style's
// claims and swapped them a second time (SPR 2026-10-14: ALN_003's 767D2
// carrier, 42 minutes on the line, pulled off and parked as a partial).

func onePosConsume(node, payload string) *processes.NodeClaim {
	return &processes.NodeClaim{CoreNodeName: node, PayloadCode: payload, Role: protocol.ClaimRoleConsume}
}

func TestApplyNodeState(t *testing.T) {
	t.Parallel()
	from, to := onePosConsume("N", "OLD"), onePosConsume("N", "NEW")
	produceFrom := &processes.NodeClaim{CoreNodeName: "N", PayloadCode: "OLD", Role: protocol.ClaimRoleProduce}
	produceTo := &processes.NodeClaim{CoreNodeName: "N", PayloadCode: "NEW", Role: protocol.ClaimRoleProduce}
	pairedTo := onePosConsume("N", "NEW")
	pairedTo.PairedCoreNode = "N_B"
	holdsNew := NodeState{Seat: SeatBin, Part: "NEW", PartKnown: true, Drain: NotDrained}

	for _, tc := range []struct {
		name      string
		situation ChangeoverSituation
		from, to  *processes.NodeClaim
		state     NodeState
		want      ChangeoverSituation
		done      bool
	}{
		{"swap, no bin: just deliver", SituationSwap, from, to, NodeState{Seat: SeatEmpty}, SituationAdd, false},
		{"swap, a bin of the old part", SituationSwap, from, to, NodeState{Seat: SeatBin, Part: "OLD", PartKnown: true}, SituationSwap, false},
		{"swap, unknown seat", SituationSwap, from, to, NodeState{}, SituationSwap, false},
		{"swap, holds the new part: done", SituationSwap, from, to, holdsNew, SituationUnchanged, true},
		{"swap, holds the new part, no counter to ask: done", SituationSwap, from, to,
			NodeState{Seat: SeatBin, Part: "NEW", PartKnown: true, Drain: DrainUnknown}, SituationUnchanged, true},
		{"swap, the new part but drained: a starved line, swap", SituationSwap, from, to,
			NodeState{Seat: SeatBin, Part: "NEW", PartKnown: true, Drain: Drained}, SituationSwap, false},
		{"swap, part not known: swap", SituationSwap, from, to,
			NodeState{Seat: SeatBin, Part: "NEW", PartKnown: false}, SituationSwap, false},
		{"produce: an empty says nothing, swap", SituationSwap, produceFrom, produceTo, holdsNew, SituationSwap, false},
		{"paired positions: decided for the cell, swap", SituationSwap, from, pairedTo, holdsNew, SituationSwap, false},
		{"add, already delivered: done", SituationAdd, nil, to, holdsNew, SituationUnchanged, true},
		{"add, nothing there yet", SituationAdd, nil, to, NodeState{Seat: SeatEmpty}, SituationAdd, false},
		{"evacuate is tooling's, untouched", SituationEvacuate, from, to, holdsNew, SituationEvacuate, false},
		{"drop is untouched", SituationDrop, from, nil, holdsNew, SituationDrop, false},
	} {
		diffs := []ChangeoverNodeDiff{{CoreNodeName: "N", Situation: tc.situation, FromClaim: tc.from, ToClaim: tc.to}}
		got := ApplyNodeState(diffs, func(string) NodeState { return tc.state })[0]
		if got.Situation != tc.want || got.AlreadyDone != tc.done {
			t.Errorf("%s: situation %s done %v, want %s done %v", tc.name, got.Situation, got.AlreadyDone, tc.want, tc.done)
		}
	}
}

// holdPartAt puts a carrier of part on the node the way a landing does: the
// seat's pointer and the identity, through the doorway.
func holdPartAt(t *testing.T, eng *Engine, nodeID int64, coreNode, part string) {
	t.Helper()
	bin := int64(77)
	testutil.MustNoErr(t, eng.db.SetProcessNodeActiveBinID(nodeID, &bin), "seat the carrier")
	eng.recordLinesideCarrier(nodeID, coreNode, domain.KnownCarrier(domain.LinesidePayloadCode(part)), domain.CarrierFromLanding)
}

// TestChangeoverRestart_DoesNotSwapANodeAlreadyHoldingTheNewPart: the node
// already holds the incoming style's part, so the restarted changeover moves
// nothing there and still starts. An order the outgoing style left at that node
// is cancelled — the node is still a changing node to the start's gates — so
// it cannot deliver the old part onto a node the new style runs.
func TestChangeoverRestart_DoesNotSwapANodeAlreadyHoldingTheNewPart(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)
	holdPartAt(t, eng, nodeID, "CO-NODE", "PART-NEW")
	stale := seedLiftOnlyAt(t, db, nodeID, "CO-NODE", "uuid-stale-old-style", orders.StatusQueued)

	plan, err := eng.planChangeover(processID, toStyleID, false)
	testutil.MustNoErr(t, err, "plan")
	d := findDiff(plan.diffs, "CO-NODE")
	if d == nil || d.Situation != SituationUnchanged || !d.AlreadyDone {
		t.Fatalf("CO-NODE diff = %+v, want Unchanged and AlreadyDone — it already holds PART-NEW", d)
	}

	co, err := eng.StartProcessChangeover(processID, toStyleID, "test", "")
	if err != nil {
		t.Fatalf("START with every change already done: %v — the style still has to flip at cutover", err)
	}
	task, err := db.GetChangeoverNodeTaskByNode(co.ID, nodeID)
	testutil.MustNoErr(t, err, "node task")
	if task.NextMaterialOrderID != nil || task.OldMaterialReleaseOrderID != nil {
		t.Errorf("CO-NODE got orders (supply %v, evac %v); it already holds the new part",
			task.NextMaterialOrderID, task.OldMaterialReleaseOrderID)
	}
	if got := reloadStatus(t, db, stale); got != orders.StatusCancelled {
		t.Errorf("the outgoing style's order at CO-NODE = %s after START, want cancelled — left alive it "+
			"lifts the new part off and brings the old one back", got)
	}
}

// TestChangeoverPlan_AimsTheOldCarrierAtItsOwnHome: a node holding a part of a
// third style sends its carrier where that part belongs, the same aim REQUEST
// takes (residentEvacDest), not to the outgoing style's destination.
func TestChangeoverPlan_AimsTheOldCarrierAtItsOwnHome(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	homeStyle, err := db.CreateStyle("Style-HOME", "the foreign part's style", processID)
	testutil.MustNoErr(t, err, "create style")
	_, err = upsertClaimRetiredMode(db, processes.NodeClaimInput{
		StyleID: homeStyle, CoreNodeName: "CO-NODE", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-X", UOPCapacity: 100, InboundSource: "SOURCE-X", OutboundDestination: "DEST-X",
	})
	testutil.MustNoErr(t, err, "claim PART-X at CO-NODE")
	eng := testEngine(t, db)
	holdPartAt(t, eng, nodeID, "CO-NODE", "PART-X")

	plan, err := eng.planChangeover(processID, toStyleID, false)
	testutil.MustNoErr(t, err, "plan")
	d := findDiff(plan.diffs, "CO-NODE")
	if d == nil || d.FromClaim == nil {
		t.Fatalf("CO-NODE diff = %+v, want a swap with a from-claim", d)
	}
	if d.FromClaim.OutboundDestination != "DEST-X" {
		t.Errorf("old carrier goes to %q, want DEST-X — it holds PART-X, whose home that is, not DEST-OLD",
			d.FromClaim.OutboundDestination)
	}
}
