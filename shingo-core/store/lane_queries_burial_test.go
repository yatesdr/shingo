//go:build docker

package store_test

import (
	"errors"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// lane_queries_burial_test.go — the burial guard, and the half of it that is a
// deliberate non-guard.
//
// The clause refuses a store slot when a HARD-claimed bin sits deeper in the same
// lane. Hard means bins.claimed_by, which is written immediately before the fleet
// call and cleared at arrival: it means a robot is already on its way. A soft
// (pending) reservation deeper in the lane does NOT refuse, because a soft hold
// is a plan and the held-bin path re-resolves a buried plan into a dig.
//
// Both halves are pinned. The second one especially: a future reader who finds
// the asymmetry surprising will be tempted to "finish" the guard by folding
// reservations in, and that form deadlocks (see the findings doc, and the cycle
// note on findStoreSlot).

// claimBinAt puts a bin at a slot and hard-claims it for a fresh live order,
// returning the claiming order — the state a bin is in between
// ConfirmForDispatch and arrival, which is exactly what the guard respects.
func claimBinAt(t *testing.T, db *store.DB, label string, slotID int64) (*orders.Order, int64) {
	t.Helper()
	bin := testdb.CreateBinAtNode(t, db, "PART-A", slotID, label)
	owner := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "in_transit" })
	testdb.ClaimBinForTest(t, db, bin.ID, owner.ID)
	return owner, bin.ID
}

// TestBurialGuard_RefusesInFrontOfAHardClaim is the guard itself.
//
// Geometry: depth-3 holds a hard-claimed bin, depths 1 and 2 are empty and
// reachable. Without the clause the selector packs back-to-front and returns
// depth 2 — directly in front of a bin a robot is driving to. With it, every
// remaining slot in the lane is in front of that bin, so the lane has nothing to
// offer and says so with a sentinel the callers can act on.
//
// MUTATION: delete the burial clause from findStoreSlot. This returns depth 2
// and the test fires on the slot it got, not on the error.
func TestBurialGuard_RefusesInFrontOfAHardClaim(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	laneID, _, _, slot3ID := laneFixture(t, db, "BG-HARD")

	claimBinAt(t, db, "BIN-BG-HARD", slot3ID)

	got, err := db.FindStoreSlotInLane(laneID)
	if err == nil {
		t.Fatalf("store slot = %s, want refused — placing here walls a bin a robot is "+
			"already en route to, and nothing re-plans a job the fleet already owns", got.Name)
	}
	if !errors.Is(err, nodes.ErrLaneClosedByClaim) {
		t.Errorf("err = %v, want ErrLaneClosedByClaim — a lane closed by a claim is rare, "+
			"self-clearing and worth watching; a full lane is routine. The callers walk on either "+
			"way, so the sentinel is for the diagnosis, not the control flow", err)
	}
}

// TestBurialGuard_SoftHoldIsBuriable is the other half of the design, and it is
// pinned so nobody "completes" the guard.
//
// Same geometry, but the deep bin carries only a PENDING reservation and no hard
// claim — an order parked pre-dispatch, holding the bin it intends to come back
// for. The placement PROCEEDS. What protects that order is not a refusal here: it
// re-resolves next tick and a buried verdict becomes a dig.
//
// Folding soft holds into the clause is the form that deadlocks: soft holds have
// no time bound, so two cross-lane moves parked on each other's holds refuse each
// other forever with both holders alive and correct.
func TestBurialGuard_SoftHoldIsBuriable(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	laneID, _, slot2ID, slot3ID := laneFixture(t, db, "BG-SOFT")

	deep := testdb.CreateBinAtNode(t, db, "BIN-BG-SOFT", slot3ID, "BIN-BG-SOFT")
	parked := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "sourcing" })
	if err := reservations.Acquire(db.DB, parked.ID, parked.ID, deep.ID, "test"); err != nil {
		t.Fatalf("acquire soft hold: %v", err)
	}

	got, err := db.FindStoreSlotInLane(laneID)
	if err != nil {
		t.Fatalf("FindStoreSlotInLane: %v — a soft hold must not close a lane", err)
	}
	if got.ID != slot2ID {
		t.Fatalf("store slot = %d, want slot2=%d — back-to-front packing is unchanged for a bin "+
			"whose holder has not dispatched", got.ID, slot2ID)
	}
}

// TestBurialGuard_OrderPlacesRelativeToItsOwnClaim — the self exemption, taken
// through the SAME excludeOrderID convention the three older clauses use.
//
// It is what lets the lane-gate re-bind work: a gate-staged order re-resolving
// its dropoff must be able to see the slot it is entitled to, and its own claim
// deeper in the lane is its own business.
func TestBurialGuard_OrderPlacesRelativeToItsOwnClaim(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	laneID, _, slot2ID, slot3ID := laneFixture(t, db, "BG-SELF")

	owner, _ := claimBinAt(t, db, "BIN-BG-SELF", slot3ID)

	if _, err := db.FindStoreSlotInLane(laneID); err == nil {
		t.Fatal("fixture: the blind form must refuse, or the exemption below proves nothing")
	}
	got, err := db.FindStoreSlotInLaneExcluding(laneID, owner.ID)
	if err != nil {
		t.Fatalf("FindStoreSlotInLaneExcluding(own claim): %v — an order may always place relative "+
			"to its own claim", err)
	}
	if got.ID != slot2ID {
		t.Fatalf("store slot = %d, want slot2=%d", got.ID, slot2ID)
	}
}

// TestBurialGuard_AFullLaneIsNotReportedAsClosed keeps the sentinel honest.
//
// The attribution re-asks with the clause OFF and only claims "closed" when a
// slot appears without it. So a genuinely full lane — and a lane whose deep
// slots are stranded behind a shallow bin, which is the reachability clause's
// business and not this one's — must come back with the plain error. Otherwise
// every full lane at both plants would log as a burial-guard closure on day one.
func TestBurialGuard_AFullLaneIsNotReportedAsClosed(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	laneID, slot1ID, slot2ID, slot3ID := laneFixture(t, db, "BG-FULL")

	// Every slot occupied, nothing claimed: full, not closed.
	testdb.CreateBinAtNode(t, db, "PART-A", slot1ID, "BIN-BG-FULL-1")
	testdb.CreateBinAtNode(t, db, "PART-A", slot2ID, "BIN-BG-FULL-2")
	testdb.CreateBinAtNode(t, db, "PART-A", slot3ID, "BIN-BG-FULL-3")

	_, err := db.FindStoreSlotInLane(laneID)
	if err == nil {
		t.Fatal("a full lane must refuse")
	}
	if errors.Is(err, nodes.ErrLaneClosedByClaim) {
		t.Errorf("err = %v, want the plain full-lane error — reporting a full lane as claim-closed "+
			"would make the watch signal fire constantly and mean nothing", err)
	}

	// The stranded-deep-slot shape, for the same reason: a claimed bin SHALLOWER
	// than the empties is the reachability clause's refusal, not the guard's.
	shallowLane, sSlot1, _, _ := laneFixture(t, db, "BG-STRAND")
	claimBinAt(t, db, "BIN-BG-STRAND", sSlot1)
	_, err = db.FindStoreSlotInLane(shallowLane)
	if err == nil {
		t.Fatal("a lane whose mouth slot is occupied has no reachable empty slot")
	}
	if errors.Is(err, nodes.ErrLaneClosedByClaim) {
		t.Errorf("err = %v, want the plain error — the deeper empties are unreachable behind a bin, "+
			"which is geometry and was already refused before this clause existed", err)
	}
}

// TestBurialGuard_ClearedClaimReopensTheLane is the releaser, at the level this
// package can prove it: the refusal is a function of live state and nothing else,
// so the instant the claim goes the lane is open again. No retry counter, no
// backoff, no state to unwind.
//
// The event that carries this to a parked order is asserted separately, in
// engine/lane_burial_claim_clear_redrive_docker_test.go
// (TestBurialGuard_ClaimClearEventRedrivesAParkedStore): a store parked on a
// group whose every lane is claim-closed is re-driven by the claim-clear event
// rather than by the 60-second sweep. Here the point is only that there is
// nothing to reset.
func TestBurialGuard_ClearedClaimReopensTheLane(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	laneID, _, slot2ID, slot3ID := laneFixture(t, db, "BG-CLEAR")

	owner, claimed := claimBinAt(t, db, "BIN-BG-CLEAR", slot3ID)
	if _, err := db.FindStoreSlotInLane(laneID); err == nil {
		t.Fatal("fixture: the lane must be closed while the claim is held")
	}

	// The claim clears the way arrival clears it: claimed_by goes to NULL, and
	// the reservation under it goes with it — ReleaseClaimForBin is that coupled
	// pair, which is what arrival runs.
	if err := db.ReleaseClaimForBin(claimed, owner.ID); err != nil {
		t.Fatalf("release claim: %v", err)
	}

	got, err := db.FindStoreSlotInLane(laneID)
	if err != nil {
		t.Fatalf("FindStoreSlotInLane after the claim cleared: %v — the closure must be a pure "+
			"function of the claim, or it is a wedge", err)
	}
	if got.ID != slot2ID {
		t.Fatalf("store slot = %d, want slot2=%d", got.ID, slot2ID)
	}
}
