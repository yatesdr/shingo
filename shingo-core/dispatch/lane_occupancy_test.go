//go:build docker

package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
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

// mustTake is the take-and-assert form for the tests below, which are about
// occupancy's LIFETIME and need the write to have happened before they read.
func mustTake(t *testing.T, d *Dispatcher, orderID int64, ns ...*nodes.Node) {
	t.Helper()
	if err := d.TakeLaneOccupancy(orderID, ns...); err != nil {
		t.Fatalf("TakeLaneOccupancy(order %d): %v", orderID, err)
	}
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
	mustTake(t, d, child.ID, slots[0], nil)
	if got := occupants(t, db, lane); len(got) != 1 || got[0] != child.ID {
		t.Fatalf("occupants after dispatch = %v, want [%d]", got, child.ID)
	}

	// Taking it again is a no-op, not a second row. Re-entry into
	// AdvanceCompoundOrder is routine, and a hold that counts entries rather than
	// occupants would never reach zero.
	mustTake(t, d, child.ID, slots[0], nil)
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
	mustTake(t, d, child.ID, slots[0])
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
	mustTake(t, d, legOne.ID, slotsA[0], slotsB[0])
	if got := occupants(t, db, laneA); len(got) != 1 {
		t.Fatalf("lane A occupants = %v, want the leg", got)
	}
	if got := occupants(t, db, laneB); len(got) != 1 {
		t.Fatalf("lane B occupants = %v, want the leg", got)
	}

	mustTake(t, d, legTwo.ID, slotsB[1])
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

// blockOccupancyFor makes AcquireOccupancy fail for exactly one order, through
// the production SQL, and restores the table on cleanup.
//
// Scoped to one order id on purpose. The obvious ways to break this write —
// closing the pool, renaming the table — are table-wide, and these tests share a
// database and run in parallel; a blast radius wider than the case under test
// would fail its NEIGHBOURS and read as this test passing. NOT VALID skips the
// existing-row scan, so the constraint applies to new inserts only and adding it
// cannot fail on rows another test is mid-way through writing.
func blockOccupancyFor(t *testing.T, db *store.DB, orderID int64) {
	t.Helper()
	name := fmt.Sprintf("tmp_block_occ_%d", orderID)
	_, err := db.Exec(fmt.Sprintf(
		`ALTER TABLE reservations ADD CONSTRAINT %s
		 CHECK (NOT (resource_kind = 'occupancy' AND order_id = %d)) NOT VALID`, name, orderID))
	if err != nil {
		t.Fatalf("install occupancy-write block for order %d: %v", orderID, err)
	}
	t.Cleanup(func() {
		if _, dErr := db.Exec(fmt.Sprintf(`ALTER TABLE reservations DROP CONSTRAINT IF EXISTS %s`, name)); dErr != nil {
			t.Errorf("drop occupancy-write block %s: %v", name, dErr)
		}
	})
}

// TestCompound_UnrecordableOccupancyHoldsTheChild is F4: brief 4 step 6 promoted
// Hold B from advisory to enforcing, and the WRITE did not move with the read.
//
// The read fails closed — laneOccupiedForChild treats an unreadable lane as a
// busy one. The write logged and carried on, so a lane whose occupancy could not
// be RECORDED read as empty to the next leg: the same two-robots-in-one-corridor
// collision, reached from the other side. Both dispositions have to agree, and
// "hold the child" is the one that agrees with the read.
//
// TWO ASSERTIONS, and the second is the one that makes the guard's POSITION
// load-bearing rather than stylistic:
//
//  1. the child is not dispatched — no robot goes into a lane Core cannot say
//     it is in;
//  2. the child is still PENDING, so a later re-drive picks it up. A hold is
//     only fail-closed if the thing held can be released. GetNextChildOrder
//     selects status='pending', and no transition out of `sourcing` returns to
//     pending, so a child held one line further down — after MoveToSourcing —
//     is invisible to every re-drive and its parent sits in `reshuffling`
//     forever. That is a wedge, not a hold.
//
// DESIGN §16 rule 7: the take is the FIRST refusal this call can hit. Everything
// upstream is satisfied on purpose — the child is pending with both nodes set,
// both node lookups resolve, the lane is empty (leg one of an empty lane), and
// the exactly-once re-read passes because no vendor id exists yet. If any of
// those refused first the test would pass with the fix reverted.
//
// MUTATION 1 (verified): restore the log-and-continue body of TakeLaneOccupancy.
// BOTH assertions fire. Assertion 1 — "leg one was dispatched into a lane its
// occupancy could not be recorded for" — and assertion 2 reports the leg at
// `dispatched`, because with the write swallowed nothing stops the call. The
// resulting state is a child in flight with NO occupancy row, which is exactly
// what the next leg's read cannot tell apart from an empty lane.
//
// MUTATION 2 (verified): keep the returned error, move the take back below
// MoveToSourcing. Assertion 1 PASSES — the child is genuinely not dispatched and
// the lane is genuinely empty — and only assertion 2 fires, reporting `sourcing`.
// That split is the reason this test is two assertions and not one: the obvious
// fix passes half of it and leaves the leg stranded where no re-drive can find
// it. A one-assertion version of this test would have shipped that.
func TestCompound_UnrecordableOccupancyHoldsTheChild(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	parent, children, lane, _ := twoLegCompound(t, db, "OCCFAIL")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	blockOccupancyFor(t, db, children[0].ID)

	if err := d.AdvanceCompoundOrder(parent.ID); err != nil {
		t.Fatalf("advance with an unwritable occupancy row must hold the child, not error out: %v", err)
	}

	// 1. Nothing was sent.
	if inFlight(t, db, children[0].ID) {
		t.Error("leg one was dispatched into a lane its occupancy could not be recorded for — the next " +
			"leg's laneOccupiedForChild read sees an empty lane and follows it in")
	}
	if got := occupants(t, db, lane); len(got) != 0 {
		t.Errorf("lane occupants = %v, want none", got)
	}

	// 2. And it is still claimable. This is the position assertion.
	held, err := db.GetOrder(children[0].ID)
	if err != nil {
		t.Fatalf("reload leg one: %v", err)
	}
	if held.Status != protocol.StatusPending {
		t.Fatalf("leg one is %s, want %s — a held child must stay pending or GetNextChildOrder "+
			"(status='pending') never returns it again, and no transition out of sourcing goes back. "+
			"The leg is stranded and the parent stays in reshuffling forever",
			held.Status, protocol.StatusPending)
	}

	// The re-drive proves it: same call, working write, leg one goes.
	next, err := db.GetNextChildOrder(parent.ID)
	if err != nil || next == nil || next.ID != children[0].ID {
		t.Fatalf("GetNextChildOrder after the hold = %v (err %v), want leg one (%d)", next, err, children[0].ID)
	}
}

// blockSourcingFor makes the pending→sourcing status write fail for exactly one
// order, through the production CAS, and restores the table on cleanup. Scoped
// to one id for the reason blockOccupancyFor is: these tests share a database.
func blockSourcingFor(t *testing.T, db *store.DB, orderID int64) func() {
	t.Helper()
	name := fmt.Sprintf("tmp_block_sourcing_%d", orderID)
	drop := func() {
		if _, dErr := db.Exec(fmt.Sprintf(`ALTER TABLE orders DROP CONSTRAINT IF EXISTS %s`, name)); dErr != nil {
			t.Errorf("drop sourcing block %s: %v", name, dErr)
		}
	}
	_, err := db.Exec(fmt.Sprintf(
		`ALTER TABLE orders ADD CONSTRAINT %s
		 CHECK (NOT (id = %d AND status = 'sourcing')) NOT VALID`, name, orderID))
	if err != nil {
		t.Fatalf("install sourcing block for order %d: %v", orderID, err)
	}
	t.Cleanup(drop)
	return drop
}

// TestCompound_RefusedSourcingClaimDoesNotDispatch is F1: the compare-and-set
// that decides which caller owns this child already exists, and its verdict was
// being discarded.
//
// AdvanceCompoundOrder has five goroutines that can reach it and no serializer.
// It does not need one. transition() compare-and-swaps on the status the caller
// loaded, and GetNextChildOrder selects `status='pending' … LIMIT 1` — so two
// concurrent callers resolve to the SAME child and exactly one CAS matches a
// row. pending→sourcing IS the claim. This line used to log the refusal and
// dispatch regardless, and nothing downstream caught it: the loser's struct
// still reads `pending`, so Dispatch fails IllegalTransition, while the
// orphan-mission guard that cancels the vendor order is scoped to
// IsConcurrentTransition. Two real fleet orders, one row, one untracked robot.
//
// This drives the refusal through the production CAS rather than through two
// goroutines, deliberately. A racing test would reproduce the interleaving only
// sometimes and would be read as flaky when it caught the bug; a refused CAS is
// the state that interleaving PRODUCES, and it is the state the guard is written
// against. What it does not claim is reachability — see §17.9.
//
// THREE ASSERTIONS. The third is the one bare `return nil` fails:
//
//  1. the child is not dispatched;
//  2. its occupancy row is GONE. Occupancy was taken before the status move
//     and laneOccupiedForChild counts any occupant INCLUDING the child itself,
//     so returning while that row stands leaves the next re-drive reading the
//     lane as busy — busy with the leg it is trying to send;
//  3. the leg still goes once the refusal lifts. A hold that no re-drive can
//     clear is a wedge with better logging.
//
// DESIGN §16 rule 7: the status write is the FIRST refusal reachable here. The
// child is pending with both nodes set, both lookups resolve, the lane is empty,
// the exactly-once re-read passes (no vendor id yet), and the occupancy take
// succeeds — that last one matters, because if it refused instead the test would
// pass against F4's guard and say nothing about this one.
//
// MUTATION 1 (verified): restore the log-and-continue body — drop the
// `return nil` and the release. Assertion 1 fires: the child dispatches with a
// vendor order id despite the database having refused the claim.
//
// MUTATION 2 (verified): keep the `return nil`, delete
// `d.ReleaseLaneOccupancy(next.ID)`. Assertions 2 and 3 both fire — the row
// survives and the re-drive then holds the leg against its own occupancy,
// forever. That is the arm that makes the release a separate claim rather than
// tidying up after the return.
func TestCompound_RefusedSourcingClaimDoesNotDispatch(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	parent, children, lane, _ := twoLegCompound(t, db, "CASREFUSE")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	unblock := blockSourcingFor(t, db, children[0].ID)

	if err := d.AdvanceCompoundOrder(parent.ID); err != nil {
		t.Fatalf("a refused claim must hold the child, not error out: %v", err)
	}

	// 1. The loser sends nothing.
	if inFlight(t, db, children[0].ID) {
		c0, _ := db.GetOrder(children[0].ID)
		t.Errorf("leg one was dispatched as %q after the database REFUSED its pending→sourcing claim — "+
			"that refusal is another caller already owning this child, and dispatching anyway is two "+
			"robots sent to do one leg", c0.VendorOrderID)
	}

	// 2. And leaves no occupancy behind. Without this the hold is self-inflicted.
	if got := occupants(t, db, lane); len(got) != 0 {
		t.Errorf("lane occupants after the refused claim = %v, want none — laneOccupiedForChild counts "+
			"the child's OWN row, so leaving it means the next re-drive holds leg one against itself", got)
	}

	// 3. Liveness: lift the refusal and the same call sends the leg.
	unblock()
	if err := d.AdvanceCompoundOrder(parent.ID); err != nil {
		t.Fatalf("re-drive after the refusal lifted: %v", err)
	}
	if !inFlight(t, db, children[0].ID) {
		c0, _ := db.GetOrder(children[0].ID)
		t.Fatalf("leg one is %s (vendor %q) after the refusal lifted — the hold was permanent, which is "+
			"a wedge, not fail-closed", c0.Status, c0.VendorOrderID)
	}
}
