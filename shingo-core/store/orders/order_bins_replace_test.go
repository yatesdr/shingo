//go:build docker

// Black-box (package orders_test) per the cycle note in orders_test.go.
package orders_test

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestReplaceOrderBins_ReplacesTheSetRatherThanMergingIntoIt is the stale-row
// failure, in the smallest form that produces it.
//
// The junction rows say which bins an order is carrying, and the delivery check
// walks them: applyMultiBinArrivalForOrder refuses any bin the order does not
// hold, and that refusal FAILS the order loud (cargo_ledger_mismatch). So a row
// left over from an allocation that was rolled back does not degrade — it kills
// an order that arrived carrying exactly what it should.
//
// InsertOrderBin's conflict target is (order_id, bin_id), which fixed the
// duplicate case (the same bin re-recorded) and could not touch this one: a
// retry that claims a DIFFERENT bin conflicts with nothing, so it adds a row and
// leaves the old bin's standing. Nothing deletes junction rows outside delivery
// and TerminalizeOrder, so a complex order refused at a lane and re-allocated
// accumulated the union of every bin it ever considered.
//
// Seen in the sim 2027-11-09: order 45 was refused ~40 times at Lane_01
// (lane-occupied), re-allocated on each pass, finally dispatched holding bin 7,
// and failed at arrival on the junction row for bin 24 that an early attempt had
// left behind — bin 24 by then sitting untouched at another node, claimed by
// nobody.
func TestReplaceOrderBins_ReplacesTheSetRatherThanMergingIntoIt(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB
	sd := testdb.SetupStandardData(t, d)

	// Real bins: order_bins carries a foreign key, which is the point — these are
	// rows about material, not labels.
	binA := testdb.CreateBinAtNode(t, d, "PART-A", sd.StorageNode.ID, "BIN-OBR-A").ID
	binB := testdb.CreateBinAtNode(t, d, "PART-A", sd.StorageNode.ID, "BIN-OBR-B").ID
	binC := testdb.CreateBinAtNode(t, d, "PART-A", sd.StorageNode.ID, "BIN-OBR-C").ID

	o := newPendingOrder("uuid-order-bins-replace")
	o.Status = protocol.StatusQueued
	testutil.MustNoErr(t, orders.Create(db, o), "create")

	binsOf := func() []int64 {
		t.Helper()
		rows, err := orders.ListOrderBins(db, o.ID)
		testutil.MustNoErr(t, err, "list order_bins")
		out := make([]int64, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.BinID)
		}
		return out
	}

	// The first allocation: two pickups, bins A and B.
	testutil.MustNoErr(t, orders.ReplaceOrderBins(db, o.ID, []orders.OrderBinRow{
		{BinID: binA, StepIndex: 1, Action: string(protocol.ActionPickup), NodeName: "SMN_011", DestNode: "SMN_010"},
		{BinID: binB, StepIndex: 3, Action: string(protocol.ActionPickup), NodeName: "SMN_012", DestNode: "SMN_010"},
	}), "first allocation")
	if got := binsOf(); len(got) != 2 {
		t.Fatalf("fixture: the first allocation recorded %v, want two rows", got)
	}

	// It is refused at the lane, its claims roll back, and a later pass allocates
	// a DIFFERENT pair. Only bin B is common to both.
	testutil.MustNoErr(t, orders.ReplaceOrderBins(db, o.ID, []orders.OrderBinRow{
		{BinID: binB, StepIndex: 1, Action: string(protocol.ActionPickup), NodeName: "SMN_013", DestNode: "SMN_010"},
		{BinID: binC, StepIndex: 3, Action: string(protocol.ActionPickup), NodeName: "SMN_014", DestNode: "SMN_010"},
	}), "second allocation")

	got := binsOf()
	want := map[int64]bool{binB: true, binC: true}
	if len(got) != 2 {
		t.Fatalf("the junction rows are %v, want exactly the second allocation's two bins.\n"+
			"A bin the order no longer holds is a row the arrival check fails the order on.", got)
	}
	for _, b := range got {
		if !want[b] {
			t.Errorf("bin %d survived an allocation that did not claim it (rows: %v). That row is what "+
				"applyMultiBinArrivalForOrder refuses on, and the refusal is a loud failure of an order "+
				"carrying exactly what it should.", b, got)
		}
	}

	// And the row that IS common carries the SECOND allocation's facts, not the
	// first's — the set is replaced, so nothing survives by having been there.
	rows, err := orders.ListOrderBins(db, o.ID)
	testutil.MustNoErr(t, err, "re-list")
	for _, r := range rows {
		if r.BinID == binB && r.NodeName != "SMN_013" {
			t.Errorf("the re-claimed bin's pickup node is %q, want SMN_013 — it was re-claimed at a different node "+
				"and the row still names the first allocation's", r.NodeName)
		}
	}
}
