package engine

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// ---------------------------------------------------------------------------
// The collision guard has to cover the BACKFILL position, not just the front one.
//
// An unflipped press-index swap is:
//
//	R1  wait@front, pickup front, dropoff outbound, pickup inbound, dropoff BACKFILL
//	R2  wait@paired, pickup paired, dropoff FRONT [, pickup second, dropoff paired]
//
// Both legs place a bin on the press. classifySwapLegsBySteps asks only about
// the FRONT position, so it correctly labels R2 the supply leg — and the guard,
// asking the same front-position question, protected only R2. Releasing R1 while R2
// was still queued sent a robot to set a bin down on the backfill position that
// nothing had lifted the on-deck carrier off.
//
// Under the IndexRobotSupplies flip R1 stops at the destination and places
// nowhere on the press, which is why that case must stay releasable.
// ---------------------------------------------------------------------------

// seedUnflippedPressIndexPair builds a real 2-position unflipped press-index
// pair from the production step builder, so the shapes are the ones the plant
// gets rather than hand-written fixtures.
func seedUnflippedPressIndexPair(t *testing.T, flipped bool) (*Engine, *store.DB, int64, int64, int64) {
	t.Helper()
	db := testEngineDB(t)
	nodeID, _, claim := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	if flipped {
		yes := true
		_, err := db.UpsertStyleNodeClaim(claimInputFrom(claim, &yes))
		testutil.MustNoErr(t, err, "flip index_robot_supplies")
		node, err := db.GetProcessNode(nodeID)
		testutil.MustNoErr(t, err, "re-read node")
		claim = findActiveClaim(db, node)
		if claim == nil || !claim.IndexRobotSupplies {
			t.Fatal("the flip did not take")
		}
	}
	_, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")

	r1Steps, r2Steps := BuildTwoRobotPressIndexSwapSteps(claim)
	if len(r1Steps) == 0 || len(r2Steps) == 0 {
		t.Fatal("the step builder produced no legs — the claim fixture is wrong")
	}
	r1 := mkSwapLeg(t, db, nodeID, "uuid-r1", r1Steps, claim.OutboundDestination)
	r2 := mkSwapLeg(t, db, nodeID, "uuid-r2", r2Steps, claim.CoreNodeName)
	testutil.MustNoErr(t, db.LinkOrderSiblings(r1.ID, r2.ID), "link siblings")

	eng := testEngine(t, db)
	return eng, db, nodeID, r1.ID, r2.ID
}

// claimInputFrom rebuilds an upsert body from a persisted claim, flipping
// index_robot_supplies. Only the fields the fixture set are named — everything
// absent is absent-means-untouched.
func claimInputFrom(c *processes.NodeClaim, indexRobotSupplies *bool) processes.NodeClaimInput {
	return processes.NodeClaimInput{
		StyleID:              c.StyleID,
		CoreNodeName:         c.CoreNodeName,
		Role:                 c.Role,
		SwapMode:             c.SwapMode,
		PayloadCode:          c.PayloadCode,
		UOPCapacity:          c.UOPCapacity,
		InboundSource:        c.InboundSource,
		InboundStaging:       c.InboundStaging,
		OutboundStaging:      c.OutboundStaging,
		OutboundDestination:  c.OutboundDestination,
		PairedCoreNode:       c.PairedCoreNode,
		SecondPairedCoreNode: c.SecondPairedCoreNode,
		IndexRobotSupplies:   indexRobotSupplies,
	}
}

// TestPlacingLegGate_UnflippedEvacAtBackfillPositionIsHeld is the F6 regression: R1
// is staged and would set a bin down on the backfill position; R2, the leg that
// lifts the on-deck carrier off that position, has not run.
func TestPlacingLegGate_UnflippedEvacAtBackfillPositionIsHeld(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, r1ID, r2ID := seedUnflippedPressIndexPair(t, false)

	testutil.MustNoErr(t, db.UpdateOrderStatus(r1ID, string(protocol.StatusStaged)), "R1 staged")
	testutil.MustNoErr(t, db.UpdateOrderStatus(r2ID, string(protocol.StatusQueued)), "R2 queued")
	// Runtime slots: Staged -> evac, Active -> supply. R1 IS the evac.
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &r2ID, &r1ID), "runtime slots")

	err := eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{CalledBy: "operator:test"})
	if err == nil {
		t.Fatal("want a hold: R1 would place a bin on the backfill position that R2 has not cleared")
	}
	var notReady *SwapPairNotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("want a *SwapPairNotReadyError; got %T (%v)", err, err)
	}
	if notReady.SiblingState != string(protocol.StatusQueued) {
		t.Errorf("refusal reports sibling state %q, want %q", notReady.SiblingState, protocol.StatusQueued)
	}
}

// TestPlacingLegGate_FlippedEvacPlacesNowhereAndIsReleasable is the other side
// of the same widening, and the reason the evac arm asks for evidence instead
// of assuming. Under the flip R1 ends at the outbound destination; it sets
// nothing down on the press, so nothing can collide and holding it would strand
// the swap.
func TestPlacingLegGate_FlippedEvacPlacesNowhereAndIsReleasable(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, r1ID, r2ID := seedUnflippedPressIndexPair(t, true)

	testutil.MustNoErr(t, db.UpdateOrderStatus(r1ID, string(protocol.StatusStaged)), "R1 staged")
	testutil.MustNoErr(t, db.UpdateOrderStatus(r2ID, string(protocol.StatusQueued)), "R2 queued")
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &r2ID, &r1ID), "runtime slots")

	if err := eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{CalledBy: "operator:test"}); err != nil {
		t.Fatalf("a flipped R1 places nothing on the press and must be releasable: %v", err)
	}
}
