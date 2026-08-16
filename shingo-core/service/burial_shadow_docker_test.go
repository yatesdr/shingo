//go:build docker

package service

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// burial_shadow_docker_test.go — the instrument counts, and it refuses nothing.
//
// Since the burial guard landed, the instrument reports two different things: the
// SOFT count is the dataset (placements that buried a plan, which the guard
// deliberately allows and the held-bin path turns into digs), and the BYPASS
// count is a tripwire (a hard claim buried without going through the guarded
// selector, expected zero). Both are asserted here, along with the property the
// whole thing rests on: a placement over a maximally-encumbered lane behaves
// byte-identically to a placement over an empty one.

// burialLane builds a LANE with `depth` depth-ordered slots and returns them
// shallow → deep. A lane rather than a bare parent because LaneForNode keys on
// the parent's node-type code, which is the instrument's cheap exit.
func burialLane(t *testing.T, db *store.DB, name string, depth int) []*nodes.Node {
	t.Helper()
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	testutil.MustNoErr(t, err, "get LANE node type")
	lane := &nodes.Node{Name: name + "-LANE", IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID}
	testutil.MustNoErr(t, db.CreateNode(lane), "create lane")

	out := make([]*nodes.Node, 0, depth)
	for i := 0; i < depth; i++ {
		d := i
		s := &nodes.Node{Name: name + "-S" + string(rune('0'+i)), Enabled: true, ParentID: &lane.ID, Depth: &d}
		testutil.MustNoErr(t, db.CreateNode(s), "create slot")
		out = append(out, s)
	}
	return out
}

// binAt drops a bin into a slot.
func binAt(t *testing.T, db *store.DB, label string, slot *nodes.Node) *bins.Bin {
	t.Helper()
	bt := ensureDefaultBinType(t, db)
	b := &bins.Bin{BinTypeID: bt.ID, Label: label, NodeID: &slot.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(b), "create bin")
	return b
}

// softHold puts a PENDING bin reservation on b for a fresh live order and
// backdates it — the window-2 shape: an order parked pre-dispatch holding the
// bin it will come back for, with no hard claim, because ConfirmForDispatch runs
// after the admission that parked it.
func softHold(t *testing.T, db *store.DB, b *bins.Bin, age time.Duration) *orders.Order {
	t.Helper()
	holder := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "sourcing" })
	testutil.MustNoErr(t, reservations.Acquire(db.DB, holder.ID, b.ID, "test"), "acquire soft hold")
	_, err := db.Exec(`UPDATE reservations SET created_at = NOW() - $1::interval WHERE order_id=$2 AND bin_id=$3`,
		age.String(), holder.ID, b.ID)
	testutil.MustNoErr(t, err, "backdate hold")
	return holder
}

// TestBurialShadow_CountsAPendingHoldBurial is the core case, and the hold class
// is the point: `pending-hold` is the window-2 population — the very bin the
// SAFE guard predicate cannot see, which is why the instrument exists rather
// than the guard.
func TestBurialShadow_CountsAPendingHoldBurial(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := newBinSvc(db)
	slots := burialLane(t, db, "BSPEND", 3)

	held := binAt(t, db, "BSPEND-HELD", slots[2]) // depth 2, deepest
	softHold(t, db, held, 3*time.Hour)

	// The placement: a bin arrives at depth 1, in front of the held one.
	arriving := binAt(t, db, "BSPEND-ARRIVE", slots[0])
	_, err := svc.ApplyArrival(arriving.ID, slots[1].ID, false, nil)
	testutil.MustNoErr(t, err, "ApplyArrival")

	got := svc.BurialShadowTally()
	if got.Soft != 1 {
		t.Fatalf("tally = %+v, want exactly 1 soft burial. A placement at depth 1 walls a bin an "+
			"order is holding a plan on, which the guard deliberately allows", got)
	}
	if got.Bypass != 0 || got.DigUncovered != 0 {
		t.Errorf("tally = %+v, want the hard-claim counters empty — a soft hold is not a tripwire "+
			"event, and confusing the two makes the should-be-zero useless", got)
	}
	// The age is the cost side: it lower-bounds how long protecting this class
	// would have kept the lane closed, so a wrong age makes the trade unreadable.
	if got.SoftLongestHeld < 2*time.Hour+50*time.Minute || got.SoftLongestHeld > 3*time.Hour+10*time.Minute {
		t.Errorf("held-at-burial = %s, want about 3h (the hold's backdated age)", got.SoftLongestHeld)
	}
}

// TestBurialShadow_UnencumberedLaneCountsNothing is the narrowness assertion: an
// ordinary placement behind nothing, or in front of a bin nobody is waiting for,
// is not a burial. Without this the instrument could count every arrival and
// still look like it worked.
func TestBurialShadow_UnencumberedLaneCountsNothing(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := newBinSvc(db)
	slots := burialLane(t, db, "BSCLEAR", 3)

	// A bin at depth 2 that NOBODY holds, and a terminal order's stale interest
	// in another one — neither is a live hold.
	_ = binAt(t, db, "BSCLEAR-FREE", slots[2])

	arriving := binAt(t, db, "BSCLEAR-ARRIVE", slots[0])
	_, err := svc.ApplyArrival(arriving.ID, slots[1].ID, false, nil)
	testutil.MustNoErr(t, err, "ApplyArrival")

	if got := svc.BurialShadowTally(); got.Soft != 0 || got.Bypass != 0 {
		t.Fatalf("tally = %+v, want 0 — nothing behind this placement is spoken for", got)
	}

	// And the other direction: a placement DEEPER than an occupied slot buries
	// nothing, because burial is about what is behind you, not in front.
	deeper := binAt(t, db, "BSCLEAR-DEEPER", slots[0])
	_, err = svc.ApplyArrival(deeper.ID, slots[2].ID, false, nil)
	if err == nil {
		if got := svc.BurialShadowTally(); got.Soft != 0 || got.Bypass != 0 {
			t.Fatalf("tally = %+v after a deep placement, want 0", got)
		}
	}
}

// TestBurialShadow_HardClaimBuriedIsATripwire is the counter that must stay at
// zero.
//
// The burial guard refuses a placement in front of a hard-claimed bin at the
// store-slot selector, so reaching this state means the placement did not go
// through it. The test drives it directly (ApplyArrival takes a destination; it
// does not consult the selector) precisely because production cannot, which is
// the whole point of the tripwire.
//
// Both hard flavours land in the same counter — an ordinary dispatched pickup and
// a dig leg's claim — because the guard's clause reads bins.claimed_by and does
// not care which. They are still NAMED apart in the log line, so a bypass report
// says which kind of holder got walled.
func TestBurialShadow_HardClaimBuriedIsATripwire(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	slots := burialLane(t, db, "BSTRIP", 4)

	// Depth 3: a dig leg's claim, held from plan creation.
	legBin := binAt(t, db, "BSTRIP-LEG", slots[3])
	parent := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "reshuffling" })
	leg := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = "pending"
		o.ParentOrderID = &parent.ID
	})
	testdb.ClaimBinForTest(t, db, legBin.ID, leg.ID)

	// Depth 2: an ordinary dispatched pickup — a robot en route.
	hardBin := binAt(t, db, "BSTRIP-HARD", slots[2])
	dispatched := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "in_transit" })
	testdb.ClaimBinForTest(t, db, hardBin.ID, dispatched.ID)

	// One placement at depth 1, by a PLAIN order, buries both.
	svc := newBinSvc(db)
	arriving := binAt(t, db, "BSTRIP-ARRIVE", slots[0])
	placer := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "in_transit" })
	testdb.ClaimBinForTest(t, db, arriving.ID, placer.ID)
	_, err := svc.ApplyArrival(arriving.ID, slots[1].ID, false, nil)
	testutil.MustNoErr(t, err, "ApplyArrival")

	got := svc.BurialShadowTally()
	if got.Bypass != 2 {
		t.Fatalf("tally = %+v, want Bypass=2 — a placement that walls two bins robots are coming "+
			"for, from a path that did not consult the guarded selector", got)
	}
	if got.Soft != 0 || got.DigUncovered != 0 {
		t.Errorf("tally = %+v, want the other counters empty — the tripwire is only worth having "+
			"if nothing else can raise it", got)
	}
}

// TestBurialShadow_DigPlacementIsCountedApart keeps the tripwire clean.
//
// A reshuffle picks its shuffle slots through findShuffleSlots, which has its own
// candidate predicate and never calls the store-slot selector — deliberately, so
// that a guard can never refuse the moves that exist to unbury things. A dig leg
// burying a claimed bin is therefore a KNOWN gap, not a bypass, and counting it
// as one would make the should-be-zero permanently dirty and useless.
func TestBurialShadow_DigPlacementIsCountedApart(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	slots := burialLane(t, db, "BSDIG", 4)

	hardBin := binAt(t, db, "BSDIG-HARD", slots[2])
	owner := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "in_transit" })
	testdb.ClaimBinForTest(t, db, hardBin.ID, owner.ID)

	// The placement is a DIG LEG's: a compound child carrying the bin it is
	// parking in a shuffle slot.
	svc := newBinSvc(db)
	arriving := binAt(t, db, "BSDIG-ARRIVE", slots[0])
	digParent := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "reshuffling" })
	digLeg := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = "in_transit"
		o.ParentOrderID = &digParent.ID
	})
	testdb.ClaimBinForTest(t, db, arriving.ID, digLeg.ID)
	_, err := svc.ApplyArrival(arriving.ID, slots[1].ID, false, nil)
	testutil.MustNoErr(t, err, "ApplyArrival")

	got := svc.BurialShadowTally()
	if got.DigUncovered != 1 {
		t.Fatalf("tally = %+v, want DigUncovered=1 — a dig's placement never consults the selector, "+
			"so it is a gap to size and not a defect to chase", got)
	}
	if got.Bypass != 0 {
		t.Fatalf("tally = %+v, want Bypass=0 — filing the known gap as a bypass would keep the "+
			"should-be-zero permanently non-zero, which retires it as a signal", got)
	}
}

// TestBurialShadow_TerminalHolderIsNotAHold — a hold whose order is terminal is
// not a hold. The terminal chokepoint releases it in the same transaction as the
// status write, so a row that outlives its order is a leak, not a wait — and
// counting one would inflate the benefit side with burials nobody was waiting on.
func TestBurialShadow_TerminalHolderIsNotAHold(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := newBinSvc(db)
	slots := burialLane(t, db, "BSTERM", 3)

	held := binAt(t, db, "BSTERM-HELD", slots[2])
	holder := softHold(t, db, held, time.Hour)
	// Take the holder terminal WITHOUT releasing the row, which is the only state
	// this filter can be tested against: the production chokepoint would have
	// released it, and the janitors reap it within a sweep.
	_, err := db.Exec(`UPDATE orders SET status='cancelled' WHERE id=$1`, holder.ID)
	testutil.MustNoErr(t, err, "terminalize holder")

	arriving := binAt(t, db, "BSTERM-ARRIVE", slots[0])
	_, err = svc.ApplyArrival(arriving.ID, slots[1].ID, false, nil)
	testutil.MustNoErr(t, err, "ApplyArrival")

	if got := svc.BurialShadowTally(); got.Soft != 0 || got.Bypass != 0 {
		t.Fatalf("tally = %+v, want 0 — a terminal order's leftover row is a leak for the reaper, "+
			"not somebody waiting for a bin", got)
	}
}

// TestBurialShadow_CannotRefuseThePlacement IS THE MUTATION THAT MATTERS.
//
// The instrument's whole licence is that it changes nothing. So: make the
// predicate maximally true — every slot behind the placement holds a spoken-for
// bin, one of each class — and assert the arrival is byte-identical to an
// arrival into an empty lane. Bin moved, claim released, status set, no error.
//
// The tally assertion is not decoration: without it the test would also pass if
// the predicate never fired at all, which is the one way this could look safe
// and prove nothing.
func TestBurialShadow_CannotRefuseThePlacement(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := newBinSvc(db)
	slots := burialLane(t, db, "BSNOREF", 4)

	soft := binAt(t, db, "BSNOREF-SOFT", slots[2])
	softHold(t, db, soft, 90*time.Minute)
	hard := binAt(t, db, "BSNOREF-HARD", slots[3])
	owner := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "in_transit" })
	testdb.ClaimBinForTest(t, db, hard.ID, owner.ID)

	arriving := binAt(t, db, "BSNOREF-ARRIVE", slots[0])
	mover := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "in_transit" })
	testdb.ClaimBinForTest(t, db, arriving.ID, mover.ID)

	evicted, err := svc.ApplyArrival(arriving.ID, slots[1].ID, false, nil)
	testutil.MustNoErr(t, err, "ApplyArrival over a fully encumbered lane must still succeed")
	if evicted {
		t.Error("evicted = true, want false — the destination was empty")
	}

	landed, gErr := db.GetBin(arriving.ID)
	testutil.MustNoErr(t, gErr, "reload arriving bin")
	if landed.NodeID == nil || *landed.NodeID != slots[1].ID {
		t.Fatalf("bin NodeID = %v, want %d — the instrument refused a placement, which it must "+
			"never be able to do: it runs after the arrival commits and returns nothing",
			landed.NodeID, slots[1].ID)
	}
	if landed.ClaimedBy != nil {
		t.Errorf("ClaimedBy = %v, want nil — arrival releases the claim whether or not it buried anything", landed.ClaimedBy)
	}
	if landed.Status != "available" {
		t.Errorf("Status = %q, want available", landed.Status)
	}

	if got := svc.BurialShadowTally(); got.Soft+got.Bypass != 2 {
		t.Fatalf("tally = %+v, want 2 events — if the predicate did not fire, this test proves "+
			"nothing about the instrument being unable to refuse", got)
	}
}
