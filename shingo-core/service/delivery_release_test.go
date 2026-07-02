//go:build docker

// delivery_release_test.go — Rule-B: delivery releases the reservation AND the
// destination slot (commit 3). TerminalizeOrder is the terminal backstop, so
// without these DIRECT tests, deleting the delivery-time releases would leave the
// suite green while silently re-opening the Delivered->Confirmed window that the
// chokepoint work set out to close. Each assertion is RED if its release is cut.

package service

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingo/shared/clock"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

func TestDeliveryReleasesReservationAndSlot(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)
	binSvc := newBinSvc(db)

	reAcquirable := func(t *testing.T, binID int64) {
		t.Helper()
		probe := testdb.CreateOrder(t, db)
		if err := reservations.Acquire(db, probe.ID, binID, "test", "reacq", clock.Now().Add(time.Minute)); err != nil {
			t.Errorf("bin %d not re-acquirable after delivery: %v (reservation leaked?)", binID, err)
		}
	}
	countRes := func(t *testing.T, binID int64) int {
		t.Helper()
		var n int
		testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM reservations WHERE bin_id=$1`, binID).Scan(&n), "count reservations")
		return n
	}

	t.Run("ApplyArrival", func(t *testing.T) {
		destSlot := &nodes.Node{Name: "DELV-SLOT-1", Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(destSlot), "create dest slot")
		bin := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-DEL-1")
		order := testdb.CreateOrder(t, db)
		testutil.MustNoErr(t, svc.ClaimForDispatch(bin.ID, order.ID, nil), "ClaimForDispatch")
		testutil.MustNoErr(t, db.ClaimSlot(destSlot.ID, order.ID), "ClaimSlot")

		if _, err := binSvc.ApplyArrival(bin.ID, destSlot.ID, false, nil); err != nil {
			t.Fatalf("ApplyArrival: %v", err)
		}

		if n := countRes(t, bin.ID); n != 0 {
			t.Errorf("reservations for bin = %d after delivery, want 0 (ReleaseByBin removed?)", n)
		}
		reAcquirable(t, bin.ID)
		if slot, _ := db.GetNode(destSlot.ID); slot.ClaimedBy != nil {
			t.Errorf("destination slot claimed_by = %v after delivery, want nil (slot release removed?)", slot.ClaimedBy)
		}
	})

	t.Run("ApplyMultiBinArrival", func(t *testing.T) {
		slotA := &nodes.Node{Name: "DELV-SLOT-A", Enabled: true}
		slotB := &nodes.Node{Name: "DELV-SLOT-B", Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(slotA), "create slotA")
		testutil.MustNoErr(t, db.CreateNode(slotB), "create slotB")
		binA := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-DEL-A")
		binB := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-DEL-B")
		order := testdb.CreateOrder(t, db)
		testutil.MustNoErr(t, svc.ClaimForDispatch(binA.ID, order.ID, nil), "claim A")
		testutil.MustNoErr(t, svc.ClaimForDispatch(binB.ID, order.ID, nil), "claim B")
		testutil.MustNoErr(t, db.ClaimSlot(slotA.ID, order.ID), "ClaimSlot A")
		testutil.MustNoErr(t, db.ClaimSlot(slotB.ID, order.ID), "ClaimSlot B")

		testutil.MustNoErr(t, db.ApplyMultiBinArrival([]orders.BinArrivalInstruction{
			{BinID: binA.ID, ToNodeID: slotA.ID},
			{BinID: binB.ID, ToNodeID: slotB.ID},
		}), "ApplyMultiBinArrival")

		for _, p := range []struct{ bin, slot int64 }{{binA.ID, slotA.ID}, {binB.ID, slotB.ID}} {
			if n := countRes(t, p.bin); n != 0 {
				t.Errorf("reservations for bin %d = %d after multi-arrival, want 0", p.bin, n)
			}
			reAcquirable(t, p.bin)
			if slot, _ := db.GetNode(p.slot); slot.ClaimedBy != nil {
				t.Errorf("slot %d claimed_by = %v after multi-arrival, want nil", p.slot, slot.ClaimedBy)
			}
		}
	})
}
