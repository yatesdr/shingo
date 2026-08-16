//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// dig_parks_onto_its_own_demand_docker_test.go — the burial the guard could not
// see, measured on the lane-stress rig 2026-08-13 and reproduced here from its
// rows.
//
// ── THE SPECIMEN ──────────────────────────────────────────────────────────
//
// Dig 8 was raised for order 1 and parked its first blocker, bin 6, at LSD_010 —
// the back of LS_D2, a parking lane. A blocker sitting in a parking slot is an
// ordinary reachable bin of an ordinary style, and it was the most reachable one
// of its style in the group, so ORDER 1 RESOLVED ONTO IT: bin_id 6, source_node
// LSD_010. Forty-one seconds later the same dig's next leg parked bin 8 at
// LSD_007, two slots in front of it, and walled in the bin its own demand had
// just decided to come for. Order 1 sat `staged` under lane-target-buried for the
// rest of the run.
//
// ── WHY THE GUARD SAID NOTHING ────────────────────────────────────────────
//
// It asked claimed_by, and bin 6's claimed_by was NULL. A claim is taken and
// released around each movement — the dig's leg claimed the bin to carry it and
// let go when it placed it — while bin_id is the resolve's durable record of
// which bin an order is for. The burial landed in the interval between two
// claims, which is not a narrow window at all: it is the whole time an order
// spends waiting to be dispatched.
//
// Asked with bin_id as well, the same query on the same rows returns LSD_006
// through LSD_009 — the four slots in front of bin 6, one of which is the slot
// the burial used.
//
// ── WHAT THE FIXTURE HAS TO CONTAIN, AND WHY EACH PART ────────────────────
//
// A parking lane with a bin at the BACK and free slots in front of it. A live
// order aiming at that bin through bin_id, with NO CLAIM — which is the state the
// rig was frozen in, and the one the old guard is blind to. And a dig that needs
// somewhere to park.
//
// MUTATION: drop `OR holder.bin_id = held_bin.id` from SlotsBlockedByHardClaims.
// The pool offers the slots in front of the bin again and the assertion fires —
// which is the rig's own failure, reproduced.
func TestFindShuffleSlots_WillNotWallInABinAnOrderHasResolvedOnto(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	newTestDispatcher(t, db, testdb.NewSuccessBackend())
	grp, dug, park, _, dugSlots, parkSlots, bp := setupDwellGroup(t, db, "AIMBURY", 4, false)

	// The dig's lane: a blocker in front of a target, so there is something to dig.
	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "AIMBURY-BLK")
	createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "AIMBURY-TGT")

	// THE PARKED BLOCKER, at the back of the parking lane, exactly where a dig's
	// deepest-first pass puts one. Unclaimed, because a leg lets go of a bin when
	// it places it.
	parked := createTestBinAtNode(t, db, bp.Code, parkSlots[len(parkSlots)-1].ID, "AIMBURY-PARKED")
	reloaded, err := db.GetBin(parked.ID)
	testutil.MustNoErr(t, err, "reload the parked bin")
	if reloaded.ClaimedBy != nil {
		t.Fatalf("the parked bin is claimed by order %d — the fixture is not the specimen. The whole "+
			"point is a bin between claims", *reloaded.ClaimedBy)
	}

	// AND THE DEMAND THAT HAS RESOLVED ONTO IT. bin_id and source_node are what a
	// resolve writes; the claim is not held while it waits to be dispatched.
	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "aimbury-demand"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.Status = protocol.StatusQueued
		o.BinID = &parked.ID
		o.SourceNode = parkSlots[len(parkSlots)-1].Name
	})

	// The pool the dig would be offered, asked exactly as planUnbury asks it.
	slots, err := findShuffleSlots(db, dug.ID, grp.ID, 1, digAskerFor(demand), nil)

	// EVERY SLOT IN THAT LANE IS IN FRONT OF THE BIN, so a correct pool offers
	// none of them. There is no other parking in this group, so the honest answer
	// is congestion — which waits and retries, and is what law 1 asks for.
	for _, s := range slots {
		if s.ParentID != nil && *s.ParentID == park.ID {
			t.Fatalf("the pool offered %s, which is in front of %s — the bin order %d has resolved "+
				"onto (bin_id %d, source_node %s). Parking there walls that order out of the bin it "+
				"is coming for, and the order has no way to say so: it is not holding a claim, it is "+
				"waiting to be dispatched. This is the rig's own burial, reproduced",
				s.Name, parkSlots[len(parkSlots)-1].Name, demand.ID, parked.ID, demand.SourceNode)
		}
	}
	if err == nil && len(slots) == 0 {
		t.Fatal("findShuffleSlots returned no slots and no error — a shortfall must name itself, or " +
			"the caller cannot tell congestion from geometry")
	}

	// AND THE PROTECTION ENDS WITH THE ORDER, not with the bin. bin_id survives
	// terminalization where claimed_by does not, so without a liveness test on the
	// holder this would wall the lane off forever.
	testutil.MustNoErr(t, db.FailOrderAtomic(demand.ID, "demand went away"), "cancel the demand")
	after, err := findShuffleSlots(db, dug.ID, grp.ID, 1, digAskerFor(demand), nil)
	testutil.MustNoErr(t, err, "ask for parking again after the demand was cancelled")
	if len(after) == 0 {
		t.Fatal("the parking lane is still excluded after the order aiming at the bin was cancelled. " +
			"bin_id is not reaped the way claimed_by is, so a terminal order's aim would protect a " +
			"bin for the life of the plant")
	}
}
