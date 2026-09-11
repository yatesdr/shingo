package engine

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
)

// TestPlacingLegGate_HoldsAPairTheProduceDoorCreated is census 10: the release-time
// collision guard (refusePlacingLegWhileSiblingPending), reached through a door.
//
// Its own tests (release_placing_leg_gate_test.go, release_gate_backfill_seat_test.go)
// seed the runtime slots by hand, and they seed them the way ResolveSwapPair reads
// them: Active = supply, Staged = evac. The produce door writes Active = leg A =
// R1 and Staged = R2 — for press-index that is the evac in the ACTIVE slot, the
// opposite of the positional reading. What makes the guard see the right legs
// from a door-written slot order is classifySwapLegsBySteps re-labelling them by
// their steps just before the guard runs. Nothing drove that combination.
//
// An unflipped R1 is staged and would set a carrier on the backfill position; R2,
// the leg that lifts the on-deck carrier off that position, is still queued. The
// release must be held.
//
// COVERAGE PIN. Expected to pass at bcbde0d2. MUTATION: drop the
// refusePlacingLegWhileSiblingPending call from ReleaseStagedOrders — the
// release goes through and this fails.
//
// WHAT IT DOES NOT PIN: the classifySwapLegsBySteps re-label. The guard holds a
// staged leg whose partner is queued in either arrangement of this pair (both
// were run: R1 staged with R2 queued, and R2 staged with R1 queued), and its
// refusal names only the node and the partner's status, so dropping the re-label
// changes nothing this door can observe.
func TestPlacingLegGate_HoldsAPairTheProduceDoorCreated(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, node, claim := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	_, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	claimID := claim.ID
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, 50), "seed a produced count")
	eng := testEngine(t, db)

	res, err := eng.RequestProduceSwap(nodeID)
	testutil.MustNoErr(t, err, "REQUEST SWAP on "+node.Name)
	if res.OrderA == nil || res.OrderB == nil {
		t.Fatalf("fixture: the produce door did not build a two-leg press-index swap")
	}
	r1, r2 := res.OrderA.ID, res.OrderB.ID

	testutil.MustNoErr(t, db.UpdateOrderStatus(r1, string(orders.StatusStaged)), "R1 staged")
	testutil.MustNoErr(t, db.UpdateOrderStatus(r2, string(orders.StatusQueued)), "R2 queued")

	err = eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{CalledBy: "operator:test"})
	var notReady *SwapPairNotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("release returned %v, want a *SwapPairNotReadyError — R1 would set a carrier on the "+
			"backfill position R2 has not cleared, on a pair whose runtime slots the produce door wrote", err)
	}
}
