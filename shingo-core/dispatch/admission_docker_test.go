//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// admission_docker_test.go — the one decision, exercised arm by arm.
//
// admit has no production callers yet; step 5 moves the existing sites onto it
// one at a time. These tests are what those moves will be compared against, so
// each arm here states an answer a delegating site must keep giving.

// laneSlots returns a gated lane's slots, shallowest first.
func laneSlots(t *testing.T, db *store.DB, laneID int64) []*nodes.Node {
	t.Helper()
	slots, err := db.ListLaneSlots(laneID)
	if err != nil || len(slots) < 2 {
		t.Fatalf("list lane slots: %v (got %d, want >= 2)", err, len(slots))
	}
	return slots
}

// admitOrder makes a plain order sourcing from slot.
func admitOrder(t *testing.T, db *store.DB, uuid string, slot *nodes.Node) *orders.Order {
	t.Helper()
	return testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = uuid
		o.StationID = "line-1"
		o.SourceNode = slot.Name
	})
}

// TestAdmit_PhysicalChecksIgnoreTheEnforcementMode is a CORRECTION to the
// version of this file that shipped with admission, and the assertion is the
// reverse of what it said.
//
// It used to assert that an ungated lane always admits — dig planted, verdict
// admitted, "the gate must be a no-op where it is switched off". That was wrong,
// and wrong in the direction that matters: configuration selects who arbitrates
// mouth MODE-SHARING, not whether a corridor is single-file. The facts admission
// reads are written unconditionally — TakeLaneOccupancy resolves lanes with no
// mode check and both reshuffle planners take the dig lock without one — so
// gating the reads meant occupancy rows written and never consulted.
//
// The blast radius is what makes it worth a named test: `none` is the default
// and is what both plants run, so the first delegating caller would have stopped
// serialising compound legs on every lane in production while every gated-lane
// test stayed green.
//
// MUTATION (verified): put the mode back in front of the physical arms — add
// `if !d.laneIsGated(lane.ID) { return Admitted() }` ahead of admitLane's dig
// arm. This fires on both arms; every other admission test stays green, because
// they all build mouth-enforced lanes — which is exactly how the original
// slipped through.
func TestAdmit_PhysicalChecksIgnoreTheEnforcementMode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	_, laneID, s0 := gatedLane(t, db, "ADM-OFF", "") // enforcement unset → none
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	order := admitOrder(t, db, "adm-off", s0)

	// A foreign dig still excludes, on a lane Core does not arbitrate.
	stranger := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "adm-off-dig" })
	if err := reservations.AcquireLanes(db.DB, stranger.ID, reservations.ModeDig, "test", laneID); err != nil {
		t.Fatalf("plant a dig: %v", err)
	}
	v, err := d.admit(admissionSituation{order: order, sourceNode: s0})
	if err != nil {
		t.Fatalf("admit with a dig on an ungated lane: %v", err)
	}
	if v.Admitted() {
		t.Errorf("admitted into a lane another reshuffle is digging, because the group is not " +
			"mouth-enforced. Configuration chooses who arbitrates mouth mode-sharing; it does not " +
			"make a single-file corridor shareable")
	}
	if _, err := reservations.ReleaseLanesByOwner(db.DB, stranger.ID); err != nil {
		t.Fatalf("release the dig: %v", err)
	}

	// And so does an occupant.
	inside := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "adm-off-inside" })
	if _, err := reservations.AcquireOccupancy(db.DB, inside.ID, laneID); err != nil {
		t.Fatalf("acquire occupancy: %v", err)
	}
	v, err = d.admit(admissionSituation{order: order, sourceNode: s0})
	if err != nil {
		t.Fatalf("admit with an occupant on an ungated lane: %v", err)
	}
	if v.Admitted() || v.Cause() != CauseLaneOccupied {
		t.Errorf("admitted=%v cause=%q into an occupied ungated lane, want refused with %q — "+
			"TakeLaneOccupancy writes that row without consulting the mode, so reading it behind a "+
			"mode check means writing a fact nothing ever reads",
			v.Admitted(), v.Cause(), CauseLaneOccupied)
	}
}

// TestAdmit_ForeignDigRefusesAndOwnDigDoesNot is the exclusion arm and its
// exemption together, because either alone is misleading.
//
// A dig excludes every other order from the lane for the whole reshuffle. It
// must NOT exclude the reshuffle's own legs — the lock is not a reason to keep
// out the work it exists to perform, and parking a leg behind its parent's dig
// deadlocks: that dig only clears when the leg it is parking runs. That was
// brief 3 defect 1, and this arm is what keeps it fixed through the move.
//
// MUTATION (verified): delete the !d.ownsDig term from admitLane's dig arm. The
// own-leg arm fires with lane-dig-active — the deadlock, reproduced.
func TestAdmit_ForeignDigRefusesAndOwnDigDoesNot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	_, laneID, s0 := gatedLane(t, db, "ADM-DIG", "")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	digger := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "adm-dig-parent"
		o.Status = protocol.StatusReshuffling
	})
	// TAGGED AS AN EXCAVATION, because that is what this fixture has always been:
	// a reshuffle parent in `reshuffling`, holding the lane for the length of a
	// dig. The tag used to be `"test"` and the mode alone carried the meaning.
	// §R.101 gave every demand's SOURCE hold mode='dig' as well, so the kind is
	// read off reserved_by now (reservations.IsExcavation) and an untagged row
	// reads as a source lock — which is what the cause assertion below started
	// seeing. The assertion is unchanged.
	if err := reservations.AcquireLanes(db.DB, digger.ID, reservations.ModeDig,
		reservations.ByExcavation, laneID); err != nil {
		t.Fatalf("acquire dig: %v", err)
	}

	// A stranger is kept out.
	stranger := admitOrder(t, db, "adm-dig-stranger", s0)
	v, err := d.admit(admissionSituation{order: stranger, sourceNode: s0})
	if err != nil {
		t.Fatalf("admit stranger: %v", err)
	}
	if v.Admitted() || v.Cause() != CauseLaneDigActive {
		t.Errorf("stranger: admitted=%v cause=%q, want refused with %q — a dig excludes everyone else "+
			"for the whole reshuffle", v.Admitted(), v.Cause(), CauseLaneDigActive)
	}

	// The digger's OWN leg is not.
	leg := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "adm-dig-leg"
		o.StationID = "line-1"
		o.SourceNode = s0.Name
		o.OrderType = protocol.OrderTypeMove
		o.ParentOrderID = &digger.ID
	})
	v, err = d.admit(admissionSituation{order: leg, sourceNode: s0})
	if err != nil {
		t.Fatalf("admit own leg: %v", err)
	}
	if !v.Admitted() {
		t.Errorf("the dig's OWN leg was refused with %q. The lock exists to let this leg run; parking "+
			"it behind the lock deadlocks, because the lock only clears when the leg completes",
			v.Cause())
	}
}

// TestAdmit_OccupantRefusesAndSelfDoesNot — Hold B, the per-leg presence row.
//
// The self arm is not padding. admit is re-asked on every event for an order
// already inside its lane (the compound re-drive does exactly that), so an order
// that blocked on its own occupancy would refuse itself forever from the moment
// it was dispatched.
//
// MUTATION (verified): drop the `occ != s.order.ID` term. The self arm fires
// with lane-occupied.
func TestAdmit_OccupantRefusesAndSelfDoesNot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	_, laneID, s0 := gatedLane(t, db, "ADM-OCC", "")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	inside := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "adm-occ-inside" })
	if _, err := reservations.AcquireOccupancy(db.DB, inside.ID, laneID); err != nil {
		t.Fatalf("acquire occupancy: %v", err)
	}

	newcomer := admitOrder(t, db, "adm-occ-newcomer", s0)
	v, err := d.admit(admissionSituation{order: newcomer, sourceNode: s0})
	if err != nil {
		t.Fatalf("admit newcomer: %v", err)
	}
	if v.Admitted() || v.Cause() != CauseLaneOccupied {
		t.Errorf("newcomer: admitted=%v cause=%q, want refused with %q — a robot is inside a "+
			"single-file lane", v.Admitted(), v.Cause(), CauseLaneOccupied)
	}

	// The order already inside is not blocked by itself. Its own row is the only
	// one left, so nothing else can be supplying the verdict.
	insideAgain := admitOrder(t, db, "adm-occ-inside-2", s0)
	if _, err := reservations.AcquireOccupancy(db.DB, insideAgain.ID, laneID); err != nil {
		t.Fatalf("acquire second occupancy: %v", err)
	}
	if err := reservations.ReleaseAllOccupancy(db.DB, inside.ID); err != nil {
		t.Fatalf("release first occupant: %v", err)
	}
	v, err = d.admit(admissionSituation{order: insideAgain, sourceNode: s0})
	if err != nil {
		t.Fatalf("admit self: %v", err)
	}
	if !v.Admitted() {
		t.Errorf("an order already inside refused ITSELF with %q — admit is re-asked on every event "+
			"for an in-flight order, so this would refuse forever from its own dispatch onward", v.Cause())
	}
}

// TestAdmit_BuriedRefusesAtThePickupEndOnly is the reachability arm AND the
// boundary that keeps it out of the placer's business.
//
// A buried SOURCE is admission: the bin cannot be reached, so the move cannot
// happen. A buried DESTINATION is not admission's question — where a store lands
// and whether that slot sits behind an occupied one belongs to the placer and to
// the tiered-entry ordering rules. Asking it here would make admission REJECT
// stores that depth-ordering exists to SEQUENCE.
//
// MUTATION (verified): drop the `if !isSource` early return so reachability runs
// at both ends. The destination arm fires with lane-target-buried and the source
// arm stays green — the split showing the boundary is what is under test rather
// than the check itself.
func TestAdmit_BuriedRefusesAtThePickupEndOnly(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	_, laneID, _ := gatedLane(t, db, "ADM-BURIED", "")
	slots := laneSlots(t, db, laneID)
	shallow, deep := slots[0], slots[1]
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// A bin in the mouth slot buries the one behind it.
	testdb.CreateBinAtNode(t, db, "DEFAULT", shallow.ID, "ADM-BLOCKER")
	wanted := testdb.CreateBinAtNode(t, db, "DEFAULT", deep.ID, "ADM-WANTED")

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "adm-buried"
		o.StationID = "line-1"
		o.SourceNode = deep.Name
		o.BinID = &wanted.ID
	})

	v, err := d.admit(admissionSituation{order: order, sourceNode: deep})
	if err != nil {
		t.Fatalf("admit buried source: %v", err)
	}
	if v.Admitted() || v.Cause() != CauseLaneTargetBuried {
		t.Errorf("buried SOURCE: admitted=%v cause=%q, want refused with %q — the bin sits behind "+
			"another and a robot sent for it cannot reach it",
			v.Admitted(), v.Cause(), CauseLaneTargetBuried)
	}

	// THE SAME ORDER, the same lane, the same buried bin — moved from the source
	// field to the destination field. That is the only difference between these
	// two calls, so the verdict changing IS the isSource flag and nothing else.
	//
	// The first version of this arm used a DIFFERENT order whose pickup resolved
	// to a storage node outside the lane. Removing the isSource guard then
	// changed nothing, because the reachability check had nothing in the lane to
	// find, and the mutation passed. A boundary test whose two sides differ in
	// more than the boundary is not testing the boundary.
	v, err = d.admit(admissionSituation{order: order, destNode: deep})
	if err != nil {
		t.Fatalf("admit buried destination: %v", err)
	}
	if !v.Admitted() {
		t.Errorf("buried DESTINATION refused with %q. Where a store lands behind an occupied slot is "+
			"the placer's question and the ordering gate's, not admission's — refusing here would "+
			"reject stores that depth-ordering exists to sequence", v.Cause())
	}
}

// TestAdmit_UndeterminedIsNeverAdmitted is ruling 1, and it is the reason
// admission returns a struct rather than a bool.
//
// Every existing site spells its undetermined answer `park=false, err`, which
// reads as ADMIT at a glance and is correct today only because both scanner call
// sites happen to check err before park. The safety lives in each caller's
// discipline and a new caller inherits none of it. Here the dangerous reading is
// unreachable: the zero verdict is a refusal, so a caller that drops the error
// still refuses.
//
// Both halves are asserted, because either alone is satisfiable by the wrong
// implementation:
//
//   - the TYPE — a bare GateVerdict is not admitted. A `type verdict bool`
//     would fail this and nothing else would notice.
//   - a REAL undetermined path — an order whose pickup slot cannot be resolved,
//     reached through production code rather than injected, with everything
//     upstream satisfied so this arm is the first that can fail.
//
// MUTATION (re-driven 2026-08-16): return Admitted() alongside the error in
// admitLane's pickupSlotNow arm. The second half fires; the type half stays
// green, which is the split showing they are different claims. The note used to
// name `admittedVerdict()`, which is not a symbol in this tree — the recipe was
// unperformable as written.
func TestAdmit_UndeterminedIsNeverAdmitted(t *testing.T) {
	t.Parallel()

	// The type's own property — no database needed, and that is the point.
	var zero GateVerdict
	if zero.Admitted() {
		t.Fatal("the zero GateVerdict reports ADMITTED. Every undetermined arm returns this " +
			"value, so a caller that ignored the error would be told to proceed — the exact shape " +
			"this type exists to make unreachable")
	}

	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	_, _, s0 := gatedLane(t, db, "ADM-UNDET", "")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// No bin, and a source name that resolves to nothing: pickupSlotNow cannot
	// say which slot this order wants. Dig and occupancy are both clear, so this
	// is the first arm that can fail.
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "adm-undetermined"
		o.StationID = "line-1"
		o.SourceNode = "NO-SUCH-NODE-ADM"
	})

	v, err := d.admit(admissionSituation{order: order, sourceNode: s0})
	if err == nil {
		t.Fatal("an unresolvable pickup slot returned no error — the fixture no longer reaches the " +
			"arm under test, so this proves nothing about it")
	}
	if v.Admitted() {
		t.Errorf("admitted=%v alongside err=%v. An undetermined answer must never be an admission: "+
			"refusing costs a retry on the next event, and sending a robot into a lane whose state "+
			"could not be read costs a collision", v.Admitted(), err)
	}
}

// TestAdmit_NoOrderIsAnError: a situation with no order is a caller bug rather
// than a refusal with a cause — there is no order to park and no row to write a
// cause onto. Asserted so the arm cannot quietly become a silent admit.
func TestAdmit_NoOrderIsAnError(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	v, err := d.admit(admissionSituation{})
	if err == nil {
		t.Fatal("admit with no order returned a nil error")
	}
	if v.Admitted() {
		t.Error("admit with no order ADMITTED the move")
	}
}
