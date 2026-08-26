//go:build docker

package store_test

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// store_burial_aim_docker_test.go — an ordinary store walled in a demand that
// had chosen its bin but was not holding a claim on it.
//
// ── THE SPECIMEN, LANE-STRESS RIG 2026-08-13 ──────────────────────────────
//
// Order 22 had resolved onto bin 30 at LSC_033 and was parked under
// reserve-holding, waiting to be dispatched. Order 29 — an ordinary complex
// store with no relationship to it — was offered LSC_032, one slot in front, and
// took it. Order 22 spent the rest of the window walled in behind a bin that
// arrived after it had chosen.
//
// The guard was asked and said yes, because it asks claimed_by and at that
// instant nothing had bin 30 claimed. A claim is written immediately before the
// fleet call and cleared at arrival — it means a robot is already driving. bin_id
// is the resolve's durable record of WHICH BIN this order is for, and it is set
// long before any robot exists. The gap between them is not a sliver: it is all
// the time an order spends waiting to be dispatched.
//
// ── IT IS THE THIRD CASE, NOT A WIDENING OF THE SECOND ────────────────────
//
// The file next door pins the two that were already decided: a HARD CLAIM deeper
// in the lane refuses (a robot is en route), and a SOFT RESERVATION does not (a
// plan can be re-resolved into a dig, and honouring it deadlocks). This is
// neither. An order that has resolved onto a bin is not planning to want it and
// is not driving to it — it has chosen, and the choice is recorded. Burying it
// does not send it to a dig, it sends it to a WAIT with no releaser: the demand
// keeps pointing at a bin that only gets deeper.
//
// MUTATION: drop the aim term from findStoreSlot's burial clause. The selector
// offers the slot in front again — the rig's own burial, reproduced.
//
// MUTATION: keep the aim term but drop its source_node half. This one passes
// here and breaks TestStoreBurst_DivertedOntoAMarkedLane instead: a store's
// bin_id goes on pointing at its bin after it has PLACED it, so a bare aim lets
// an order close a lane behind itself and the dwellers behind it never enter.
func TestBurialGuard_RefusesInFrontOfABinAnOrderHasResolvedOnto(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	laneID, _, _, slot3ID := laneFixture(t, db, "BG-AIM")

	// The demand's bin, deepest, with the slots in front of it empty. NO CLAIM —
	// that is the whole fixture.
	bin := testdb.CreateBinAtNode(t, db, "PART-A", slot3ID, "BIN-BG-AIM")
	if b, err := db.GetBin(bin.ID); err != nil || b == nil || b.ClaimedBy != nil {
		t.Fatalf("the fixture bin is claimed (%v) — this test is about a bin between claims", err)
	}

	// THE DEMAND THAT HAS CHOSEN IT. bin_id and source_node together are what a
	// resolve writes, and both are needed: bin_id says which bin, source_node says
	// this order is coming TO that slot rather than having delivered something
	// there. No claim is held while it waits for a robot.
	slot3, err := db.GetNode(slot3ID)
	if err != nil || slot3 == nil {
		t.Fatalf("read the deep slot: %v", err)
	}
	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = protocol.StatusSourcing
		o.BinID = &bin.ID
		o.SourceNode = slot3.Name
	})

	// FindStoreSlotInLane is owner-BLIND (excludeOrderID 0), so nothing here is
	// exempt and the burial clause is the only thing that can refuse.
	got, err := db.FindStoreSlotInLane(laneID)
	if err == nil {
		t.Fatalf("store slot = %s, with order %d's bin standing behind it. Storing there walls that "+
			"demand out of the bin it has already chosen, and it cannot object — it is not holding a "+
			"claim, it is waiting for a robot", got.Name, demand.ID)
	}
	if !errors.Is(err, nodes.ErrLaneClosedByClaim) {
		t.Errorf("err = %v, want ErrLaneClosedByClaim. The slot is empty and usable, so a caller told "+
			"the lane is FULL goes looking for room that exists; closed-by-claim is rare, "+
			"self-clearing and worth watching", err)
	}

	// ── AND IT ENDS WITH THE ORDER ──────────────────────────────────────────
	//
	// claimed_by is reaped when an order terminalizes and bin_id is not, so
	// without a liveness test on the holder a finished order's aim would close
	// this lane to stores for the life of the plant.
	if err := db.FailOrderAtomic(demand.ID, "demand went away"); err != nil {
		t.Fatalf("terminalize the demand: %v", err)
	}
	after, err := db.FindStoreSlotInLane(laneID)
	if err != nil || after == nil {
		t.Fatalf("the lane is still closed to stores (%v) after the only order aiming at that bin "+
			"reached a terminal status. Nothing is coming for it any more", err)
	}
}
