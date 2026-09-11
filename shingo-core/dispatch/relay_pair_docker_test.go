//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
)

// relay_pair_docker_test.go — a single-robot changeover's stage leg and swap
// leg are a relay, not a pair: the swap leg collects what the stage leg
// delivers, so Core takes the two unpaired. Helpers are
// pair_rule_helpers_docker_test.go's.

// ── census 24: the single-robot changeover pair is a relay ──────────────────

// singleRobotChangeoverLegs are buildSingleRobotChangeoverSwap's two legs: a
// stage leg that brings the new carrier to inbound staging, and a swap leg that
// lifts the old one, collects the staged one, sets it down, and takes the old
// one away. The swap leg's source for the new carrier IS the stage leg's
// delivery, which makes the two a relay rather than a pair: the Edge sends them
// to Core unpaired (TestChangeoverApplier_ARelayPairGoesOutUnpairedAtCore), and
// Core runs them one after the other.
func singleRobotChangeoverLegs(src, inStage, line, outStage, outDest string) (stage, swap []protocol.ComplexOrderStep) {
	stage = []protocol.ComplexOrderStep{prPick(src), prDropExcl(inStage)}
	swap = []protocol.ComplexOrderStep{
		prWait(line), prPick(line), prDropExcl(outStage),
		prPick(inStage), prDrop(line), prPick(outStage), prDrop(outDest),
	}
	return stage, swap
}

// TestRelayPair_StageGoesAloneAndTheSwapFollowsOnceTheCarrierLands: the stage
// leg dispatches on its own; the swap leg waits until the stage leg's carrier is
// standing at inbound staging, then goes holding both carriers — the old one it
// lifts off the line and the new one it will set there.
//
// Sent as siblings, the same two legs never go: the swap leg cannot source until
// the stage leg delivers, and the pair rule will not dispatch the stage leg
// without it. That was census 24 at bcbde0d2 — both legs parked reserve-holding,
// every pass, with nothing that could end it.
//
// COVERAGE PIN. Passes at bcbde0d2: the defect was the Edge pairing the legs,
// and this is what Core does with them unpaired. MUTATION: submit the two legs
// naming each other — neither goes.
func TestRelayPair_StageGoesAloneAndTheSwapFollowsOnceTheCarrierLands(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	src := prNode(t, db, "PR24-SRC")
	inStage := prNode(t, db, "PR24-IN-STAGE")
	outStage := prNode(t, db, "PR24-OUT-STAGE")
	outDest := prNode(t, db, "PR24-OUT")
	fresh := testdb.CreateBinAtNode(t, db, sd.Payload.Code, src.ID, "PR24-NEW")
	old := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "PR24-OLD")
	stageSteps, swapSteps := singleRobotChangeoverLegs(src.Name, inStage.Name, sd.LineNode.Name,
		outStage.Name, outDest.Name)
	prSubmitLeg(d, "pr24-stage", "", sd.Payload.Code, sd.LineNode.Name, stageSteps...)
	prSubmitLeg(d, "pr24-swap", "", sd.Payload.Code, sd.LineNode.Name, swapSteps...)

	prScanPass(t, d, db, "pr24-stage", "pr24-swap")

	stage, swap := prReloadUUID(t, db, "pr24-stage"), prReloadUUID(t, db, "pr24-swap")
	if stage.VendorOrderID == "" {
		t.Fatalf("the stage leg did not go on its own: %q/%q (%q)", stage.Status, stage.QueueCause, stage.QueueReason)
	}
	if swap.VendorOrderID != "" {
		t.Fatalf("the swap leg went (%s) before its carrier reached %s — it would lift the old carrier with "+
			"nothing to set down", swap.VendorOrderID, inStage.Name)
	}
	if protocol.IsTerminal(swap.Status) {
		t.Fatalf("the swap leg went %q while its carrier was on its way — that is a wait, not an end", swap.Status)
	}

	// The stage leg delivers: its carrier stands at inbound staging and the leg is done.
	if err := db.MoveBinClearingStaging(fresh.ID, inStage.ID, false); err != nil {
		t.Fatalf("land the new carrier: %v", err)
	}
	if _, err := db.TerminalizeOrder(stage.ID, protocol.StatusConfirmed, "delivered to inbound staging"); err != nil {
		t.Fatalf("complete the stage leg: %v", err)
	}

	prScanPass(t, d, db, "pr24-swap")

	swap = prReloadUUID(t, db, "pr24-swap")
	if swap.VendorOrderID == "" {
		t.Fatalf("the carrier landed and the swap leg still did not go: %q/%q (%q)",
			swap.Status, swap.QueueCause, swap.QueueReason)
	}
	claimed, err := db.ListBinsByClaim(swap.ID)
	if err != nil {
		t.Fatalf("list the swap leg's claims: %v", err)
	}
	var holdsOld, holdsNew bool
	for _, b := range claimed {
		holdsOld = holdsOld || b.ID == old.ID
		holdsNew = holdsNew || b.ID == fresh.ID
	}
	if !holdsOld || !holdsNew {
		t.Errorf("the swap leg holds %d bin(s) (old carrier: %t, new carrier: %t), want both — the new one is "+
			"what it sets on the line, and census 25's rebind finds it through this claim", len(claimed), holdsOld, holdsNew)
	}
}
