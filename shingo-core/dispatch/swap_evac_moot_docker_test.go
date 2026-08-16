//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// mootPayload creates a payload for these fixtures. Inline rather than shared:
// healLaneFixture builds a whole gated lane group, which this shape does not
// need — the deadlock is about a press pair, not a lane.
func mootPayload(t *testing.T, db *store.DB, code string) *payloads.Payload {
	t.Helper()
	bp := &payloads.Payload{Code: code, Description: code, UOPCapacity: 10}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload "+code)
	return bp
}

// TestReserve_EvacHoldingItsReplacementIsMootWhenTheLineBinIsGone is the
// PRESS-1 deadlock from lane-stress 2026-08-10, in the smallest form that has it.
//
// ── THE TWO LEGS ──────────────────────────────────────────────────────────
//
//	evac  (64): wait PLN_001 → pickup PLN_001 → dropoff STORE
//	            → pickup SPARE(empty) → dropoff PLN_002
//	index (65): wait PLN_002 → pickup PLN_002(empty) → dropoff PLN_001
//
// The evac clears the press front and stages a fresh carrier at the back; the
// index then walks that carrier from back to front. The index is correctly held
// by swapLegHeld until the evac commits — "only the FILLER waits".
//
// ── THE DEADLOCK ──────────────────────────────────────────────────────────
//
// PLN_001 is empty. The evac's first pickup can never be satisfied, so it never
// commits, so the index stays held — and the index's dropoff is PLN_001, the
// only thing that would put a bin back where the evac is looking. Each leg waits
// for the other, and neither is behaving incorrectly.
//
// reserveMoot exists for exactly this ("a swap evac whose line bin was removed")
// but tested it as "reserved nothing". An evac that fetches its own replacement
// has already reserved that carrier by the time its line pickup comes up empty,
// so it always holds something and always fell to reserveHolding. On the rig it
// held for 33 minutes with no way out.
//
// MUTATION: dropping the lineBinGone arm returns reserveHolding and the assert
// below fails with the live symptom — an evac holding partials against an empty
// line node.
func TestReserve_EvacHoldingItsReplacementIsMootWhenTheLineBinIsGone(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	front := lineNode(t, db, "MOOT-PLN-001") // the line position — DELIBERATELY EMPTY
	back := lineNode(t, db, "MOOT-PLN-002")  // paired on-deck position
	store := lineNode(t, db, "MOOT-STORE")
	spare := lineNode(t, db, "MOOT-SPARE")
	bp := mootPayload(t, db, "MOOTP")

	// The replacement carrier the evac fetches for itself — present and free, so
	// the evac WILL hold a reservation by the time its line pickup misses.
	createTestBinAtNode(t, db, bp.Code, spare.ID, "BIN-MOOT-SPARE")

	evac := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = OrderTypeComplex
		o.Coordinated = true
		o.Status = "sourcing"
		o.ProcessNode = front.Name
		o.SourceNode = front.Name
	})
	steps := []resolvedStep{
		{Action: protocol.ActionWait, Node: front.Name},
		{Action: protocol.ActionPickup, Node: front.Name}, // the line bin — GONE
		{Action: protocol.ActionDropoff, Node: store.Name},
		{Action: protocol.ActionPickup, Node: spare.Name}, // its own replacement
		{Action: protocol.ActionDropoff, Node: back.Name},
	}

	// Sanity: this is a pure evac that secures its own replacement — the exact
	// shape the old moot test could not see.
	if !legTakesLineBin(steps, front.Name) {
		t.Fatal("fixture is not a pure evac — the narrowing under test would not apply")
	}
	if !legSecuresOwnReplacement(steps) {
		t.Fatal("fixture does not fetch its own replacement — that property is the whole point")
	}

	plan := &ComplexPlan{ResolvedSteps: steps}
	assigned, outcome, err := d.allocator.reserveComplexPlan(evac, plan)
	testutil.MustNoErr(t, err, "reserve")

	if outcome != reserveMoot {
		t.Errorf("outcome = %v, want reserveMoot. The bin this leg came to remove is gone and "+
			"nothing can put one back except its own sibling, which is held until this leg "+
			"commits. Holding here is a wait with nothing to wait for — it parked two robots "+
			"for 33 minutes on the rig. (held %d partial(s), which is exactly why the "+
			"len(assigned)==0 test could not see it)", outcome, len(assigned))
	}
}

// TestReserve_EvacWithBinsPresentStillHolds — the narrowing must not turn a
// momentary shortage into a skip.
//
// When the line node HAS a bin that is merely taken by someone else, the work is
// not void: whoever holds it will finish. That is reserveHolding, and skipping it
// would delete real work.
func TestReserve_EvacWithBinsPresentStillHolds(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	front := lineNode(t, db, "HOLD-PLN-001")
	back := lineNode(t, db, "HOLD-PLN-002")
	store := lineNode(t, db, "HOLD-STORE")
	spare := lineNode(t, db, "HOLD-SPARE")
	bp := mootPayload(t, db, "HOLDP")

	createTestBinAtNode(t, db, bp.Code, spare.ID, "BIN-HOLD-SPARE")
	// A bin IS at the line — reserved by another order, so present-but-taken.
	resident := createTestBinAtNode(t, db, bp.Code, front.ID, "BIN-HOLD-RESIDENT")
	other := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "sourcing" })
	testutil.MustNoErr(t, d.binManifest.ReserveForDispatch(resident.ID, other.ID), "other order reserves the line bin")

	evac := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = OrderTypeComplex
		o.Coordinated = true
		o.Status = "sourcing"
		o.ProcessNode = front.Name
		o.SourceNode = front.Name
	})
	plan := &ComplexPlan{ResolvedSteps: []resolvedStep{
		{Action: protocol.ActionWait, Node: front.Name},
		{Action: protocol.ActionPickup, Node: front.Name},
		{Action: protocol.ActionDropoff, Node: store.Name},
		{Action: protocol.ActionPickup, Node: spare.Name},
		{Action: protocol.ActionDropoff, Node: back.Name},
	}}

	_, outcome, err := d.allocator.reserveComplexPlan(evac, plan)
	testutil.MustNoErr(t, err, "reserve")

	if outcome == reserveMoot {
		t.Error("an evac whose line bin is merely TAKEN was skipped as moot. The bin is there " +
			"and whoever holds it will finish — deleting this order deletes real work, and the " +
			"press would sit unswapped with no order on the board explaining why")
	}
}

// TestReserve_FillerWithEmptySourceStillHolds pins the pre-existing exception
// this change must not disturb.
//
// A leg that PLACES a bin on the line and finds its source empty means "the
// replacement is not staged yet" — demand, which is operator-driven and never
// evaporates. Skipping it is the Hopkinsville 2026-07-22 failure, where every
// index leg died ~5ms after creation and took its evac sibling with it.
func TestReserve_FillerWithEmptySourceStillHolds(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	front := lineNode(t, db, "FILL-PLN-001")
	back := lineNode(t, db, "FILL-PLN-002") // empty source
	mootPayload(t, db, "FILLP")

	filler := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = OrderTypeComplex
		o.Coordinated = true
		o.Status = "sourcing"
		o.ProcessNode = front.Name
		o.SourceNode = back.Name
	})
	plan := &ComplexPlan{ResolvedSteps: []resolvedStep{
		{Action: protocol.ActionWait, Node: back.Name},
		{Action: protocol.ActionPickup, Node: back.Name},
		{Action: protocol.ActionDropoff, Node: front.Name},
	}}

	_, outcome, err := d.allocator.reserveComplexPlan(filler, plan)
	testutil.MustNoErr(t, err, "reserve")

	if outcome == reserveMoot {
		t.Error("a filler with an unstaged source was skipped as moot — that is demand, not " +
			"void work, and skipping it kills the index leg and its evac sibling with it")
	}
}
