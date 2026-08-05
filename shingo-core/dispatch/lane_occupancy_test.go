//go:build docker

package dispatch

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/reservations"
)

func occupants(t *testing.T, db *store.DB, laneID int64) []int64 {
	t.Helper()
	got, err := reservations.OccupantsOf(db.DB, laneID)
	if err != nil {
		t.Fatalf("OccupantsOf(%d): %v", laneID, err)
	}
	return got
}

// TestLaneOccupancy_SpansPickupAndEndsAtDropoff pins Hold B's lifetime, which is
// the entire reason it is a second hold rather than a rename of the first.
//
// Hold A — the dig's claim, a mouth row owned by the compound parent — spans the
// whole reshuffle. Hold B is owned by ONE leg and answers a different question:
// is a robot physically inside the lane right now.
//
// The boundary that matters is the release. After a PICKUP the robot is still in
// the lane, holding the bin it just lifted; it is out once it has PLACED at the
// destination. Releasing at pickup would declare the lane free with a robot
// standing in it — which is the exact failure this hold exists to prevent, and
// the reason the release rides handleStoreBlockCompleted rather than the transit
// event that Hold A's early handoff uses.
func TestLaneOccupancy_SpansPickupAndEndsAtDropoff(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := mirrorLane(t, db, "LANE-OCC", 3)
	slots, err := db.ListLaneSlots(lane)
	if err != nil || len(slots) < 2 {
		t.Fatalf("list lane slots: %v (got %d)", err, len(slots))
	}

	child := testdb.CreateOrder(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// Dispatched into the lane: inside from the moment Core commits to sending it.
	d.TakeLaneOccupancy(child.ID, slots[0], nil)
	if got := occupants(t, db, lane); len(got) != 1 || got[0] != child.ID {
		t.Fatalf("occupants after dispatch = %v, want [%d]", got, child.ID)
	}

	// Taking it again is a no-op, not a second row. Re-entry into
	// AdvanceCompoundOrder is routine, and a hold that counts entries rather than
	// occupants would never reach zero.
	d.TakeLaneOccupancy(child.ID, slots[0], nil)
	if got := occupants(t, db, lane); len(got) != 1 {
		t.Fatalf("occupants after a repeated take = %v, want exactly one row", got)
	}

	// THE PICKUP. The bin leaves the slot — and the robot does NOT leave the lane.
	// This is the event that releases Hold A's per-block mouth hold, and it must
	// not release Hold B.
	d.HandleTransitForLaneGate(child.ID, slots[0].ID)
	if got := occupants(t, db, lane); len(got) != 1 {
		t.Fatalf("occupants after PICKUP = %v, want the leg still inside — it is holding the bin it "+
			"just lifted and has not left the lane", got)
	}

	// THE DROPOFF. Now it is out.
	d.ReleaseLaneOccupancy(child.ID)
	if got := occupants(t, db, lane); len(got) != 0 {
		t.Fatalf("occupants after DROPOFF = %v, want empty", got)
	}
}

// TestLaneOccupancy_TerminalChildLeavesNothingBehind: a leg that fails or is
// cancelled is not inside any lane either.
//
// There is no separate arm for this and there must not be one. TerminalizeOrder
// releases reservations by ORDER and is kind-agnostic, in the same transaction
// that ends the order — so occupancy cannot outlive its owner even for the width
// of a window. A second cleanup path would be a second writer for one fact,
// which is the failure this whole brief is unwinding.
func TestLaneOccupancy_TerminalChildLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := mirrorLane(t, db, "LANE-OCC-TERM", 3)
	slots, err := db.ListLaneSlots(lane)
	if err != nil || len(slots) == 0 {
		t.Fatalf("list lane slots: %v", err)
	}

	child := testdb.CreateOrder(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	d.TakeLaneOccupancy(child.ID, slots[0])
	if got := occupants(t, db, lane); len(got) != 1 {
		t.Fatalf("occupants after dispatch = %v, want one", got)
	}

	if err := db.FailOrderAtomic(child.ID, "leg failed mid-lane"); err != nil {
		t.Fatalf("FailOrderAtomic: %v", err)
	}

	if got := occupants(t, db, lane); len(got) != 0 {
		t.Fatalf("occupants after the leg failed = %v, want empty — a lane cannot stay occupied by an "+
			"order that no longer exists to leave it", got)
	}
}

// TestLaneOccupancy_TwoLegsTwoLanes: occupancy is per (order, node), so a leg
// that spans two lanes is inside both, and one leg leaving does not evict
// another.
func TestLaneOccupancy_TwoLegsTwoLanes(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	laneA := mirrorLane(t, db, "LANE-OCC-A", 3)
	laneB := mirrorLane(t, db, "LANE-OCC-B", 3)
	slotsA, _ := db.ListLaneSlots(laneA)
	slotsB, _ := db.ListLaneSlots(laneB)

	legOne := testdb.CreateOrder(t, db)
	legTwo := testdb.CreateOrder(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// A dig leg picks out of A and places into B: it is inside both while it works.
	d.TakeLaneOccupancy(legOne.ID, slotsA[0], slotsB[0])
	if got := occupants(t, db, laneA); len(got) != 1 {
		t.Fatalf("lane A occupants = %v, want the leg", got)
	}
	if got := occupants(t, db, laneB); len(got) != 1 {
		t.Fatalf("lane B occupants = %v, want the leg", got)
	}

	d.TakeLaneOccupancy(legTwo.ID, slotsB[1])
	if got := occupants(t, db, laneB); len(got) != 2 {
		t.Fatalf("lane B occupants = %v, want both legs (recording, not arbitrating, at this step)", got)
	}

	d.ReleaseLaneOccupancy(legOne.ID)
	if got := occupants(t, db, laneA); len(got) != 0 {
		t.Fatalf("lane A occupants after leg one placed = %v, want empty", got)
	}
	if got := occupants(t, db, laneB); len(got) != 1 || got[0] != legTwo.ID {
		t.Fatalf("lane B occupants = %v, want only leg two — one leg leaving must not evict another", got)
	}
}
