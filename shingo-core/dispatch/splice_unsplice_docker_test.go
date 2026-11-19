//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// unspliceFixture is the order a refused gated create leaves (Q6): order 7's
// plan stored as spliced, lane waits at 0 and 9, and its four lifts' rows on the
// spliced positions. It returns the order, its two carts, and the plan a pass
// works it as (the waits stripped) with its final drop re-chosen, which is what
// the pass then persists.
func unspliceFixture(t *testing.T, db *store.DB, prefix string) (order *orders.Order, fresh, active int64, replanned []resolvedStep) {
	t.Helper()
	_, _, w, _, bp := clearLaneFixture(t, db, prefix)
	freshBin := createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-"+prefix+"-FRESH")
	activeBin := createTestBinAtNode(t, db, bp.Code, w[1].ID, "BIN-"+prefix+"-ACTIVE")

	spliced := []resolvedStep{
		{Action: protocol.ActionWait, Node: "LSD-M1-WAIT", WaitKind: WaitKindLane, WaitLane: 27},
		{Action: protocol.ActionPickup, Node: "LSD_023"},
		{Action: protocol.ActionDropoff, Node: "SLN_003"},
		{Action: protocol.ActionWait, Node: "ALN_003", WaitKind: WaitKindStation},
		{Action: protocol.ActionPickup, Node: "ALN_003"},
		{Action: protocol.ActionDropoff, Node: "SLN_004"},
		{Action: protocol.ActionPickup, Node: "SLN_003"},
		{Action: protocol.ActionDropoff, Node: "ALN_003"},
		{Action: protocol.ActionPickup, Node: "SLN_004"},
		{Action: protocol.ActionWait, Node: "LSD-M1-WAIT", WaitKind: WaitKindLane, WaitLane: 27},
		{Action: protocol.ActionDropoff, Node: "LSD_022", Group: "LSD"},
	}
	j, err := json.Marshal(spliced)
	testutil.MustNoErr(t, err, "marshal the spliced plan")
	order = testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = OrderTypeComplex
		o.Status = StatusPending
		o.StepsJSON = string(j)
	})
	testdb.SeedCartRow(t, db, order.ID, freshBin.ID, 1, 2)
	testdb.SeedCartRow(t, db, order.ID, activeBin.ID, 4, 5)
	testdb.SeedCartRow(t, db, order.ID, freshBin.ID, 6, 7)
	testdb.SeedCartRow(t, db, order.ID, activeBin.ID, 8, 10)
	order = reloadCarts(t, db, order.ID)

	replanned = withoutInsertedWaits(spliced)
	replanned[len(replanned)-1].Node = "LSD_021" // the pass chose another slot in the group
	return order, freshBin.ID, activeBin.ID, replanned
}

// TestPersistReplannedPlan_TheRowsLeaveTheSpliceWithIt: a pass that rewrites
// the plan of an order a refused create left spliced writes the plan without
// the waits, and its rows go back to the positions their steps have in it.
//
// Found by the dispatch-claim outage sim (2026-11-19): order 858, refused under
// the outage, had its slot taken by a dig; the next pass re-chose and persisted
// a 9-step plan over the 11-step spliced one and stopped at its slot reserve,
// the rows still on the spliced positions. The pass after it read the cart into
// SYN_MARKET at drop step 10 of a 9-step plan (store.cartIntoGroup) and Core
// died: index out of range [10] with length 9.
//
// MUTATION (fires): drop the unspliceCarts call from persistWidenedPlan → the
// rows stay at (1,2) (4,5) (6,7) (8,10) under a 9-step plan.
func TestPersistReplannedPlan_TheRowsLeaveTheSpliceWithIt(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	order, fresh, active, replanned := unspliceFixture(t, db, "UNSPL")

	d.persistWidenedPlan(order, replanned)

	after := reloadCarts(t, db, order.ID)
	stored, ok := decodeSteps(after.StepsJSON)
	if !ok || len(stored) != len(replanned) || stored[len(stored)-1].Node != "LSD_021" {
		t.Fatalf("fixture: the re-chosen plan was not written (%s)", after.StepsJSON)
	}
	if err := cartsPlanMismatch(replanned, after.Carts); err != nil {
		t.Errorf("the rows describe a different plan than the one stored: %v", err)
	}
	got := cartAt(after)
	want := map[int64][][2]int{fresh: {{0, 1}, {5, 6}}, active: {{3, 4}, {7, 8}}}
	for bin, lifts := range want {
		if len(got[bin]) != len(lifts) || got[bin][0] != lifts[0] || got[bin][1] != lifts[1] {
			t.Errorf("cart %d's lifts are at %v, want %v: the plan lost its waits and the rows kept "+
				"their positions in it", bin, got[bin], lifts)
		}
	}
	if mem := cartAt(order); mem[active][1] != got[active][1] {
		t.Errorf("the order in hand says %v and the table %v: the pass goes on with rows it did not write",
			mem[active], got[active])
	}
	if _, _, err := db.BinTypeForOrder(order.ID, "LSD"); err != nil {
		t.Errorf("the bin type read refuses the rows the persist left: %v", err)
	}
}

// TestBinTypeForOrder_ARowPastThePlanIsAnErrorNotACrash: rows that describe a
// longer plan than the stored one answer the bin type read with an error (the
// resolve goes on untyped and logs it), never an index past the plan. Before,
// it was the panic that killed Core's scanner goroutine and Core with it.
//
// MUTATION (fires): drop the bounds check in store.cartIntoGroup → the test
// panics.
func TestBinTypeForOrder_ARowPastThePlanIsAnErrorNotACrash(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	order, _, _, replanned := unspliceFixture(t, db, "PASTPLAN")
	j, err := json.Marshal(replanned)
	testutil.MustNoErr(t, err, "marshal the re-chosen plan")
	// The plan written alone, as a writer that does not move the rows writes it.
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(order.ID, string(j)), "write the plan without its rows")

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the bin type read panicked on rows past the plan: %v", r)
		}
	}()
	if _, _, err := db.BinTypeForOrder(order.ID, "LSD"); err == nil {
		t.Error("rows past the end of the plan read as an answer; they describe a plan that is not stored")
	}
}
