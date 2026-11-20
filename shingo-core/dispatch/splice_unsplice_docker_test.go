//go:build docker

package dispatch

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet"
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

// refuseCreatesFor refuses every create for one external id: the fleet is down
// for that order for the whole test, as it was for order 392 in the outage.
type refuseCreatesFor struct {
	*testdb.MockTrackingBackend
	uuid string
}

func (f *refuseCreatesFor) CreateOrder(req fleet.CreateOrderRequest) (fleet.TransportOrderResult, error) {
	if req.ExternalID == f.uuid {
		return fleet.TransportOrderResult{}, fmt.Errorf("mock: the fleet refuses %s", f.uuid)
	}
	return f.MockTrackingBackend.CreateOrder(req)
}

// TestSlotRevert_TheRowsLeaveTheSpliceWithThePlan: the allocator's fungible
// slot revert writes the pass's plan, which carries none of the waits a refused
// create left spliced into the stored one, and the rows go back with it.
//
// Order 392 in the dispatch-claim outage sim (outage-tip3, 2026-11-19): a
// two-cart swap into a marked group, refused by the fleet (its spliced plan and
// rows kept, its paper kept); order 283 took its paper by rank; 392's next pass
// ended at "incomplete slot reserve" after the slot revert stored the plan
// without its waits, and the rows were left past the end of it ("its plan has 9
// steps"). At 317e1785 that state is the cartIntoGroup panic; at 49467b40 the
// bin type read answers it with an error. Here X is 392 and Y is 283, in a
// group whose only free slot is the one X's paper is on.
//
// MUTATION (fires): drop the unspliceCarts call from reserveComplexSlots' revert
// → X's last row sets down past the end of its stored plan, and the bin type
// read refuses the rows.
func TestSlotRevert_TheRowsLeaveTheSpliceWithThePlan(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	const xUUID = "slrevx-y" // c2cAskAtAcquiring's uuid for prefix SLREVX
	backend := &refuseCreatesFor{MockTrackingBackend: testdb.NewTrackingBackend(), uuid: xUUID}
	d := NewDispatcher(db, backend, &mockEmitter{}, "core", "shingo.dispatch", &DefaultResolver{DB: db})
	g, s := c2cOneFreeSlotGroup(t, db, "SLREV", sd.Payload.Code)

	// Refused: X asks G, is given S, and the fleet refuses its create.
	x := c2cAskAtAcquiring(t, d, db, "SLREVX", sd.Payload.Code, sd.BinType.ID, 0, g)
	spliced, ok := decodeSteps(x.StepsJSON)
	if !ok || len(spliceStepShift(spliced)) == 0 || !protocol.IsAcquiring(x.Status) {
		t.Fatalf("fixture: X is %s with plan %s; want it refused, acquiring, its spliced plan stored", x.Status, x.StepsJSON)
	}
	if cartsPlanMismatch(withoutInsertedWaits(spliced), x.Carts) == nil {
		t.Fatalf("fixture: X's rows %v already describe the plan without its waits; the refusal left nothing to move",
			cartAt(x))
	}
	if h := c2cSlotHolder(t, db, s); h != x.ID {
		t.Fatalf("fixture: S's paper is held by order %d, want the refused X (%d)", h, x.ID)
	}

	// Paper taken by rank: Y outranks X, asks G, takes S and is sent.
	y := c2cAskAtAcquiring(t, d, db, "SLREVY", sd.Payload.Code, sd.BinType.ID, 100, g)
	if h := c2cSlotHolder(t, db, s); h != y.ID {
		t.Fatalf("fixture: S is held by order %d, want Y (%d), which took it by rank", h, y.ID)
	}

	// X's next pass ends at its slot reserve.
	var mu sync.Mutex
	var lines []string
	d.DebugLog = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	prScanPass(t, d, db, xUUID)
	d.DebugLog = nil
	held := fmt.Sprintf("order %d held — incomplete slot reserve", x.ID)
	mu.Lock()
	reached := false
	for _, l := range lines {
		reached = reached || strings.Contains(l, held)
	}
	mu.Unlock()
	if !reached {
		t.Fatalf("fixture: X's pass did not end at its slot reserve (%q not logged)", held)
	}

	after := reloadCarts(t, db, x.ID)
	stored, ok := decodeSteps(after.StepsJSON)
	reverted := false
	for _, st := range stored {
		reverted = reverted || (st.Action == protocol.ActionDropoff && st.Group == g.grp.Name && st.Node == g.grp.Name)
	}
	if !ok || !reverted || len(spliceStepShift(stored)) != 0 {
		t.Fatalf("fixture: X's stored plan is not the slot revert's (its drop back on %s, no waits): %s",
			g.grp.Name, after.StepsJSON)
	}
	if err := cartsPlanMismatch(stored, after.Carts); err != nil {
		t.Errorf("the slot revert stored a plan of %d steps and left X's rows %v on the spliced positions: %v",
			len(stored), cartAt(after), err)
	}
	if _, _, err := db.BinTypeForOrder(x.ID, g.grp.Name); err != nil {
		t.Errorf("the bin type read refuses the rows the slot revert left: %v", err)
	}
}
