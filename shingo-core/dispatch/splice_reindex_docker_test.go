//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// reloadCarts is the order read back with its rows.
func reloadCarts(t *testing.T, db *store.DB, id int64) *orders.Order {
	t.Helper()
	o, err := db.GetOrder(id)
	testutil.MustNoErr(t, err, "reload the order")
	return o
}

// cartAt is each cart's (pick, drop) per lift, keyed by bin, in row order.
func cartAt(o *orders.Order) map[int64][][2]int {
	out := map[int64][][2]int{}
	for _, c := range o.Carts {
		drop := -100
		if c.DropStep != nil {
			drop = *c.DropStep
		}
		out[c.BinID] = append(out[c.BinID], [2]int{*c.PickStep, drop})
	}
	return out
}

// TestCartsInvariant_RunsOnTheDispatchPath is R2-4: the invariant the splice
// broke is checked on the dispatch path, not only in tests. Pre-shift the row
// is stale by construction (its pick step is the spliced plan's wait), which
// is what proves the post-shift pass is not vacuous; and the rows the shift
// leaves on the order in memory are the rows on the table.
func TestCartsInvariant_RunsOnTheDispatchPath(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, _, w, _, bp := clearLaneFixture(t, db, "JINV")
	binOne := createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-JINV-1")
	cell := lineNode(t, db, "JINV-CELL")

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = OrderTypeComplex
		o.Status = "staged"
		o.SourceNode, o.DeliveryNode = w[0].Name, cell.Name
	})
	// Filed at the PRE-splice positions, as the allocator writes them.
	testdb.SeedCartRow(t, db, order.ID, binOne.ID, 0, 1)
	order = reloadCarts(t, db, order.ID)

	spliced := []resolvedStep{
		{Action: protocol.ActionWait, Node: "JINV-WALL-WAIT", WaitKind: WaitKindLane, WaitLane: 1},
		{Action: protocol.ActionPickup, Node: w[0].Name},
		{Action: protocol.ActionDropoff, Node: cell.Name},
	}
	if cartsPlanMismatch(spliced, order.Carts) == nil {
		t.Fatal("the invariant passed on rows that have NOT been re-indexed yet: a row whose pick " +
			"step is a WAIT read as valid, so it would pass on the drift it exists to catch")
	}

	testutil.MustNoErr(t, d.reindexCartsForSplice(order, spliced), "reindex")

	if mErr := cartsPlanMismatch(spliced, order.Carts); mErr != nil {
		t.Errorf("after the re-index the order's carts still disagree with the plan: %v", mErr)
	}
	after := reloadCarts(t, db, order.ID)
	if got, mem := cartAt(after), cartAt(order); got[binOne.ID][0] != mem[binOne.ID][0] {
		t.Errorf("the table says %v and the order in memory %v: moveCarts did not follow its own write", got, mem)
	}
}

// TestReindexFailure_ParksTheDemandInsteadOfKillingIt is R2-3.
//
// Every error reindexCartsForSplice can return is a DATABASE error — a failed
// UPDATE inside the shift transaction. The arm that received them called
// failOrderInternal(order, "invalid_steps"), which is wrong twice over:
//
//	TERMINAL — demand died for congestion, which is the one disposition
//	           wait-not-fail forbids. F-04's shape, one commit old.
//	MISLABELLED — "invalid_steps" sends the next reader to the planner to debug
//	           a plan that is perfectly well-formed, during a database outage.
//
// A DB error is the definition of a condition that resolves by itself, so the
// order holds its place and the scanner replays it. CauseReadFailed is the
// cause this class already uses (compound.go's two node reads,
// complex_reshuffle.go's) and its declared releaser is "the database answers
// again".
//
// MUTATION (fires): restore failOrderInternal(order, "invalid_steps", …) →
// assertion (a) fails with the order terminal, which is the demand gone.
func TestReindexFailure_ParksTheDemandInsteadOfKillingIt(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, _, w, _, bp := clearLaneFixture(t, db, "REIDXFAIL")
	cell := lineNode(t, db, "REIDXFAIL-CELL")
	binOne := createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-REIDXFAIL-1")

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = OrderTypeComplex
		o.Status = StatusPending
		o.SourceNode = w[0].Name
		o.DeliveryNode = cell.Name
	})
	// A row makes the re-index do real work: the splice inserts a lane wait
	// ahead of the pickup, so this row's recorded positions move.
	testdb.SeedCartRow(t, db, order.ID, binOne.ID, 0, 1)
	order = reloadCarts(t, db, order.ID)

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: w[0].Name},
		{Action: protocol.ActionDropoff, Node: cell.Name},
	}

	// Break the shift's write and nothing else: the park statement lifts the
	// rows far above any real position, and this refuses exactly that.
	_, err := db.DB.Exec(`CREATE FUNCTION refuse_shift() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'the shift is refused'; END $$;
		CREATE TRIGGER refuse_shift BEFORE UPDATE ON order_bins FOR EACH ROW
		WHEN (NEW.pick_step >= 999999) EXECUTE FUNCTION refuse_shift()`)
	testutil.MustNoErr(t, err, "break the shift")

	if dErr := d.dispatchComplexToFleet(order, steps, Hand{}); dErr == nil {
		t.Fatal("the re-index failure was swallowed — the order would dispatch with rows " +
			"describing a different plan than the one that runs, which is the 44-minute wedge")
	}

	after := reloadCarts(t, db, order.ID)

	// (a) THE DEMAND IS STILL ALIVE.
	if protocol.IsTerminal(after.Status) {
		t.Errorf("order is %s — a database that did not answer for one tick TERMINATED the demand. "+
			"Nothing re-plans it and nobody is told the work is still needed; wait-not-fail exists "+
			"for exactly this", after.Status)
	}

	// (b) AND THE ROW SAYS WHY, IN THE VOCABULARY OF THE THING THAT ACTUALLY WENT
	// WRONG. "invalid_steps" would send the reader to the planner.
	if after.QueueCause != string(CauseReadFailed) {
		t.Errorf("queue_cause = %q, want %q. The plan is well-formed; the database is not "+
			"answering, and those are debugged in different files", after.QueueCause, CauseReadFailed)
	}
}

// TestReindexCarts_LandsTheRowsOnTheSplicedPlan reproduces ORDER 7 — the
// 44-minute wedge — against a real database, end to end.
//
// The rig's own numbers: the plan's pickups were at 0 (LSD_023) and 3
// (ALN_003) while the persisted plan had lane waits at 0 and 9, putting them at
// 1 and 4. Repairing exactly those rows on the live box moved both wedged
// orders from staged to in_transit inside one evaluator pass. Every lift is a
// row now, the relays' too, so all four move.
func TestReindexCarts_LandsTheRowsOnTheSplicedPlan(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, _, w, _, bp := clearLaneFixture(t, db, "REIDX")
	// The two carts order 7 carried: the fresh one from the lane and the active
	// one at the machine.
	fresh := createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-REIDX-FRESH")
	active := createTestBinAtNode(t, db, bp.Code, w[1].ID, "BIN-REIDX-ACTIVE")

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = OrderTypeComplex
		o.Status = "staged"
		o.StepsJSON = `[{"action":"pickup","node":"LSD_023"},{"action":"dropoff","node":"SLN_003"},` +
			`{"action":"wait","node":"ALN_003"},{"action":"pickup","node":"ALN_003"},{"action":"dropoff","node":"SLN_004"},` +
			`{"action":"pickup","node":"SLN_003"},{"action":"dropoff","node":"ALN_003"},` +
			`{"action":"pickup","node":"SLN_004"},{"action":"dropoff","node":"LSD_022"}]`
	})
	// The allocator's write: PRE-splice positions, one row per lift.
	testdb.SeedCartRow(t, db, order.ID, fresh.ID, 0, 1)
	testdb.SeedCartRow(t, db, order.ID, active.ID, 3, 4)
	testdb.SeedCartRow(t, db, order.ID, fresh.ID, 5, 6)
	testdb.SeedCartRow(t, db, order.ID, active.ID, 7, 8)
	order = reloadCarts(t, db, order.ID)

	// The plan as persisted, after spliceLaneWait inserted both lane waits.
	spliced := []resolvedStep{
		{Action: protocol.ActionWait, Node: "LSD-M1-WAIT", WaitKind: WaitKindLane, WaitLane: 27},
		{Action: protocol.ActionPickup, Node: "LSD_023"},
		{Action: protocol.ActionDropoff, Node: "SLN_003"},
		{Action: protocol.ActionWait, Node: "ALN_003"},
		{Action: protocol.ActionPickup, Node: "ALN_003"},
		{Action: protocol.ActionDropoff, Node: "SLN_004"},
		{Action: protocol.ActionPickup, Node: "SLN_003"},
		{Action: protocol.ActionDropoff, Node: "ALN_003"},
		{Action: protocol.ActionPickup, Node: "SLN_004"},
		{Action: protocol.ActionWait, Node: "LSD-M1-WAIT", WaitKind: WaitKindLane, WaitLane: 27},
		{Action: protocol.ActionDropoff, Node: "LSD_022"},
	}

	testutil.MustNoErr(t, d.reindexCartsForSplice(order, spliced), "reindex")

	after := reloadCarts(t, db, order.ID)
	if err := cartsPlanMismatch(spliced, after.Carts); err != nil {
		t.Fatalf("the rows still disagree with the plan after re-indexing: %v", err)
	}
	got := cartAt(after)
	want := map[int64][][2]int{fresh.ID: {{1, 2}, {6, 7}}, active.ID: {{4, 5}, {8, 10}}}
	for bin, lifts := range want {
		if len(got[bin]) != len(lifts) || got[bin][0] != lifts[0] || got[bin][1] != lifts[1] {
			t.Errorf("cart %d's lifts landed at %v, want %v — the gate reads these positions to decide "+
				"which cart a lane entry is for", bin, got[bin], lifts)
		}
	}
}

// TestReindexCarts_ShiftThroughAnOccupiedPosition covers the write itself.
//
// A shift moves positions UPWARD, so a row's destination is often a position
// another row still holds — here two waits at the front move the first lift's
// (0, 1) onto (2, 3), which the second lift holds. Updating row by row would
// collide mid-flight on the unique (order, pick) and (order, drop); the
// park-then-land transaction is what makes it safe.
func TestReindexCarts_ShiftThroughAnOccupiedPosition(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, _, w, _, bp := clearLaneFixture(t, db, "REIDXOCC")
	first := createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-REIDXOCC-1")
	second := createTestBinAtNode(t, db, bp.Code, w[1].ID, "BIN-REIDXOCC-2")

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = OrderTypeComplex
		o.Status = "staged"
		o.StepsJSON = `[{"action":"pickup","node":"A"},{"action":"dropoff","node":"B"},` +
			`{"action":"pickup","node":"C"},{"action":"dropoff","node":"D"}]`
	})
	testdb.SeedCartRow(t, db, order.ID, first.ID, 0, 1)
	testdb.SeedCartRow(t, db, order.ID, second.ID, 2, 3)
	order = reloadCarts(t, db, order.ID)

	spliced := []resolvedStep{
		{Action: protocol.ActionWait, Node: "MARK", WaitKind: WaitKindLane, WaitLane: 5},
		{Action: protocol.ActionWait, Node: "MARK2", WaitKind: WaitKindLane, WaitLane: 6},
		{Action: protocol.ActionPickup, Node: "A"},
		{Action: protocol.ActionDropoff, Node: "B"},
		{Action: protocol.ActionPickup, Node: "C"},
		{Action: protocol.ActionDropoff, Node: "D"},
	}
	testutil.MustNoErr(t, d.reindexCartsForSplice(order, spliced), "reindex through an occupied position")

	got := cartAt(reloadCarts(t, db, order.ID))
	if len(got[first.ID]) != 1 || len(got[second.ID]) != 1 ||
		got[first.ID][0] != [2]int{2, 3} || got[second.ID][0] != [2]int{4, 5} {
		t.Errorf("carts landed at %v, want cart %d at (2, 3) and cart %d at (4, 5)", got, first.ID, second.ID)
	}
}

// TestReindexCarts_ACartInHandMovesWithItsDrop: a cart already in the robot's
// hands when the plan starts (pick step domain.InHand, -1) moves its set-down
// with the plan. Its pick step parks one below the offset, so the landing has
// to take it too.
func TestReindexCarts_ACartInHandMovesWithItsDrop(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, _, w, _, bp := clearLaneFixture(t, db, "REIDXHAND")
	held := createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-REIDXHAND-1")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = OrderTypeComplex
		o.Status = "staged"
		o.StepsJSON = `[{"action":"dropoff","node":"D"}]`
	})
	testdb.SeedCartRow(t, db, order.ID, held.ID, -1, 0)
	order = reloadCarts(t, db, order.ID)

	spliced := []resolvedStep{
		{Action: protocol.ActionWait, Node: "MARK", WaitKind: WaitKindLane, WaitLane: 5},
		{Action: protocol.ActionDropoff, Node: "D"},
	}
	testutil.MustNoErr(t, d.reindexCartsForSplice(order, spliced), "reindex a cart in hand")

	if got := cartAt(reloadCarts(t, db, order.ID)); len(got[held.ID]) != 1 || got[held.ID][0] != [2]int{-1, 1} {
		t.Errorf("the cart in hand = %v, want (-1, 1): its set-down moved with the plan", got[held.ID])
	}
}

// TestReindexCarts_AnOrderWithNoCartsWritesNothing — an order that carries no
// cart has nothing to move, and that path must stay silent rather than report
// a problem.
func TestReindexCarts_AnOrderWithNoCartsWritesNothing(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	order := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "staged" })
	spliced := []resolvedStep{
		{Action: protocol.ActionWait, Node: "MARK", WaitKind: WaitKindLane, WaitLane: 5},
		{Action: protocol.ActionPickup, Node: "A"},
	}
	if err := d.reindexCartsForSplice(order, spliced); err != nil {
		t.Errorf("an order with no carts reported an error: %v", err)
	}
}
