//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestJunctionInvariant_RunsOnTheDispatchPathNotOnlyInTests is R2-4.
//
// assertJunctionMatchesPlan was written as "the invariant that would have caught
// this on the day it landed" and then wired to nothing: its only callers were
// tests. Its analog assertEachWaitGatesItsEntry runs INSIDE the splice, so the
// asymmetry meant the next position-shifting transform would ship with no
// runtime seam check — which is the exact gap that let this one live.
//
// Two halves, and the second is the one that makes it real:
//
//	(a) the seam agrees with the store's rows — the adapter the dispatch path
//	    calls returns nil for a junction that matches its plan;
//	(b) it is checked against the rows AS THEY ARE AFTER THE SHIFT. The rows the
//	    function read before the UPDATE still carry the old indices, so an
//	    invariant asserted against them would pass while proving nothing. That is
//	    the failure mode of a check that reads its own input, and it is why the
//	    live path re-reads.
func TestJunctionInvariant_RunsOnTheDispatchPathNotOnlyInTests(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, _, w, _, bp := clearLaneFixture(t, db, "JINV")
	binOne := createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-JINV-1")
	cell := lineNode(t, db, "JINV-CELL")

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = OrderTypeComplex
		o.Status = "staged"
	})
	// Filed at the PRE-splice position, as the allocator writes it.
	testutil.MustNoErr(t, db.InsertOrderBin(order.ID, binOne.ID, 0, protocol.ActionPickup, w[0].Name, cell.Name), "junction")

	spliced := []resolvedStep{
		{Action: protocol.ActionWait, Node: "JINV-WALL-WAIT", WaitKind: WaitKindLane, WaitLane: 1},
		{Action: protocol.ActionPickup, Node: w[0].Name},
		{Action: protocol.ActionDropoff, Node: cell.Name},
	}

	// PRE-SHIFT the rows are stale by construction — the row says step 0 and step
	// 0 of the spliced plan is the wait. This is what the invariant must catch,
	// and asserting it here is what proves the post-shift pass below is not
	// vacuous.
	before, err := db.ListOrderBins(order.ID)
	testutil.MustNoErr(t, err, "list junction before")
	if junctionPlanMismatch(spliced, before) == nil {
		t.Fatal("the invariant passed on rows that have NOT been re-indexed yet — it is not " +
			"actually comparing the row's node against the step it is filed under, so it would " +
			"pass on the drift it exists to catch")
	}

	testutil.MustNoErr(t, d.reindexOrderBinsForSplice(order.ID, spliced), "reindex")

	after, err := db.ListOrderBins(order.ID)
	testutil.MustNoErr(t, err, "list junction after")
	if mErr := junctionPlanMismatch(spliced, after); mErr != nil {
		t.Errorf("after the re-index the junction still disagrees with the plan: %v", mErr)
	}
}

// TestReindexFailure_ParksTheDemandInsteadOfKillingIt is R2-3.
//
// Every error reindexOrderBinsForSplice can return is a DATABASE error — a
// failed read of order_bins, or a failed UPDATE inside the shift transaction.
// The arm that received them called failOrderInternal(order, "invalid_steps"),
// which is wrong twice over:
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
		o.Status = StatusSourcing
		o.SourceNode = w[0].Name
		o.DeliveryNode = cell.Name
	})
	// A junction row makes the re-index do real work: the splice inserts a lane
	// wait ahead of the pickup, so this row's recorded position moves.
	testutil.MustNoErr(t, db.InsertOrderBin(order.ID, binOne.ID, 0, protocol.ActionPickup, w[0].Name, cell.Name), "junction")
	order, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload")

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: w[0].Name},
		{Action: protocol.ActionDropoff, Node: cell.Name},
	}

	// Break the junction read, the same way TestAdvanceCompoundOrder_SurfacesDBError does.
	_, err = db.DB.Exec(`ALTER TABLE order_bins RENAME COLUMN step_index TO step_index_broken`)
	testutil.MustNoErr(t, err, "break the junction read")

	if dErr := d.dispatchComplexToFleet(order, steps); dErr == nil {
		t.Fatal("the re-index failure was swallowed — the order would dispatch with a junction " +
			"describing a different plan than the one that runs, which is the 44-minute wedge")
	}

	after, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload after")

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

// TestReindexOrderBins_LandsTheJunctionOnTheSplicedPlan reproduces ORDER 7 —
// the 44-minute wedge — against a real database, end to end.
//
// The rig's own numbers: the junction held step 0 (pickup LSD_023) and step 3
// (pickup ALN_003) while the persisted plan had lane waits at 0 and 9, putting
// those pickups at 1 and 4. Repairing exactly these two rows on the live box
// moved both wedged orders from staged to in_transit inside one evaluator pass,
// which is what this pins.
func TestReindexOrderBins_LandsTheJunctionOnTheSplicedPlan(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, _, w, _, bp := clearLaneFixture(t, db, "REIDX")
	// The two bins order 7 carried: the fresh one from the lane and the active
	// one at the machine.
	fresh := createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-REIDX-FRESH")
	active := createTestBinAtNode(t, db, bp.Code, w[1].ID, "BIN-REIDX-ACTIVE")

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = OrderTypeComplex
		o.Status = "staged"
	})

	// The allocator's write: PRE-splice positions.
	testutil.MustNoErr(t, db.InsertOrderBin(order.ID, fresh.ID, 0, protocol.ActionPickup, "LSD_023", "ALN_003"), "junction pre-splice 0")
	testutil.MustNoErr(t, db.InsertOrderBin(order.ID, active.ID, 3, protocol.ActionPickup, "ALN_003", "LSD_022"), "junction pre-splice 3")

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

	testutil.MustNoErr(t, d.reindexOrderBinsForSplice(order.ID, spliced), "reindex")

	rows, err := db.ListOrderBins(order.ID)
	testutil.MustNoErr(t, err, "list junction")
	if len(rows) != 2 {
		t.Fatalf("junction has %d rows, want 2", len(rows))
	}

	// Every row must now name the step it is filed under. This is the whole
	// point: an index alone always looks valid, so the node is the check.
	jr := make([]junctionRow, 0, len(rows))
	for _, r := range rows {
		jr = append(jr, junctionRow{BinID: r.BinID, StepIndex: r.StepIndex, NodeName: r.NodeName})
	}
	if err := assertJunctionMatchesPlan(spliced, jr); err != nil {
		t.Fatalf("junction still disagrees with the plan after re-indexing: %v", err)
	}

	want := map[int64]int{fresh.ID: 1, active.ID: 4}
	for _, r := range rows {
		if r.StepIndex != want[r.BinID] {
			t.Errorf("bin %d landed at step %d, want %d — the gate reads this index to decide "+
				"which bin a lane entry is for, and a miss sends it to order.BinID (the bin at "+
				"the MACHINE) and refuses the entry forever", r.BinID, r.StepIndex, want[r.BinID])
		}
	}
}

// TestReindexOrderBins_ShiftThroughAnOccupiedIndex covers the write itself.
//
// A shift moves indices UPWARD, so a row's destination is frequently a position
// another row still holds — here 0→1 while a row sits at 1. Updating row by row
// would collide mid-flight; the park-then-land transaction is what makes it
// safe, and this is the shape that proves it.
func TestReindexOrderBins_ShiftThroughAnOccupiedIndex(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, _, w, _, bp := clearLaneFixture(t, db, "REIDXOCC")
	first := createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-REIDXOCC-1")
	second := createTestBinAtNode(t, db, bp.Code, w[1].ID, "BIN-REIDXOCC-2")

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = OrderTypeComplex
		o.Status = "staged"
	})
	testutil.MustNoErr(t, db.InsertOrderBin(order.ID, first.ID, 0, protocol.ActionPickup, "A", "B"), "row at 0")
	testutil.MustNoErr(t, db.InsertOrderBin(order.ID, second.ID, 1, protocol.ActionPickup, "C", "D"), "row at 1")

	// One lane wait at the front: every recorded position slides up by one, so
	// 0→1 lands on the index 72 currently occupies and 1→2 vacates it.
	spliced := []resolvedStep{
		{Action: protocol.ActionWait, Node: "MARK", WaitKind: WaitKindLane, WaitLane: 5},
		{Action: protocol.ActionPickup, Node: "A"},
		{Action: protocol.ActionPickup, Node: "C"},
	}
	testutil.MustNoErr(t, d.reindexOrderBinsForSplice(order.ID, spliced), "reindex through an occupied index")

	rows, err := db.ListOrderBins(order.ID)
	testutil.MustNoErr(t, err, "list junction")
	got := map[int64]int{}
	for _, r := range rows {
		got[r.BinID] = r.StepIndex
	}
	if got[first.ID] != 1 || got[second.ID] != 2 {
		t.Errorf("bins landed at %v, want bin %d at step 1 and bin %d at step 2",
			got, first.ID, second.ID)
	}
}

// TestReindexOrderBins_NoJunctionIsNotAnError — the junction is written only for
// multi-bin complex orders, so almost every spliced order has no rows at all.
// That path must stay silent and cheap rather than reporting a problem.
func TestReindexOrderBins_NoJunctionIsNotAnError(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	order := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "staged" })
	spliced := []resolvedStep{
		{Action: protocol.ActionWait, Node: "MARK", WaitKind: WaitKindLane, WaitLane: 5},
		{Action: protocol.ActionPickup, Node: "A"},
	}
	if err := d.reindexOrderBinsForSplice(order.ID, spliced); err != nil {
		t.Errorf("an order with no junction rows reported an error: %v", err)
	}
}
