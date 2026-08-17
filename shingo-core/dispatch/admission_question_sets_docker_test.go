//go:build docker

package dispatch

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// admission_question_sets_docker_test.go — the convergence's acceptance
// criterion, made executable.
//
// The criterion is plan §9.5's corrected one: EACH CALLER ASKS THE SAME
// QUESTIONS AFTER THE REFACTOR AS BEFORE. The convergence moved three
// hand-written copies of the physical questions onto one function, and the only
// way that goes wrong quietly is a caller silently GAINING a question — which is
// not a refactor, it is the unification (occupancy on the plain path) or the
// dispatchHeldBin item (reachability on the plain path) landing by accident, on
// the highest-traffic path in the system, with every gated-lane test still green.
//
// So these tests are mostly NEGATIVE: they assert that a caller does NOT refuse
// on a question it did not ask before. A positive-only suite cannot see the
// failure this file exists to catch.
//
// Every lane here is left at the DEFAULT enforcement mode — `none`, what both
// plants run — so resolveOrderLaneHolds yields no holds and the mouth path
// admits on len(holds)==0. Admission is then the only thing that can refuse, and
// a verdict is unambiguously attributable to it.

// noneLaneWithTwoSlots builds an ungated lane and returns its slots,
// shallowest first.
func noneLaneWithTwoSlots(t *testing.T, db *store.DB, prefix string) (laneID int64, shallow, deep *nodes.Node) {
	t.Helper()
	_, laneID, _ = gatedLane(t, db, prefix, "") // enforcement unset → none
	slots := laneSlots(t, db, laneID)
	return laneID, slots[0], slots[1]
}

// TestQuestionSet_PlainEntryDoesNotAskOccupancy WAS HERE AND IS DELETED.
//
// It asserted the opposite of what the code now does, and it was RIGHT to, for
// as long as it stood: it said the plain path must not gain occupancy "as a
// refactor's side effect", and it failed the moment the unification landed —
// which is exactly the job it was written for.
//
// The owner then ruled that the unification IS the work, not a side effect:
// "Anything going into lanes needs to follow the gate and paths. I don't even
// want a switch. It's the authority." So the assertion inverts rather than
// weakens, and it lives with the rest of the unification's coverage in
// lane_unification_docker_test.go, where the take half is asserted beside it —
// the two are worth nothing apart.

// TestQuestionSet_PlainEntryDoesNotAskReachability — skipsForPlainEntry, arm 2.
//
// The order's source bin sits behind another. The plain path admits anyway.
//
// This is the empty cell in admission.go's audit table, and it is an OWNER
// decision with a floor consequence (dispatchHeldBin): a held-bin order never
// calls the finder, so nothing looks. Recording it as a skip makes the gap
// greppable; closing it is a separate, named change.
//
// MUTATION: delete `reachability: true` from skipsForPlainEntry. This fires with
// lane-target-buried.
func TestQuestionSet_PlainEntryDoesNotAskReachability(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	_, shallow, deep := noneLaneWithTwoSlots(t, db, "QS-PLAIN-REACH")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	line, err := db.GetNodeByDotName(sd.LineNode.Name)
	if err != nil {
		t.Fatalf("resolve line node: %v", err)
	}

	// A bin in the mouth slot buries the one the order wants.
	testdb.CreateBinAtNode(t, db, "DEFAULT", shallow.ID, "QS-PLAIN-BLOCKER")
	wanted := testdb.CreateBinAtNode(t, db, "DEFAULT", deep.ID, "QS-PLAIN-WANTED")

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "qs-plain-reach"
		o.StationID = "line-1"
		o.SourceNode = deep.Name
		o.BinID = &wanted.ID
	})

	admitted, cause, _, err := d.AcquireLanesForOrder(order, deep, line, EntryFreshBin)
	if err != nil {
		t.Fatalf("AcquireLanesForOrder: %v", err)
	}
	if !admitted {
		t.Fatalf("the plain entry path refused on %q for a buried source. It has never asked that "+
			"question — the audit table in admission.go records the cell as empty and names it an "+
			"owner decision. A refactor must not close it", cause)
	}
}

// TestQuestionSet_PlainEntryStillAsksTheDig is the positive half, and it is the
// question the convergence had to preserve: the floor defect where a plain
// retrieve walked into a corridor another reshuffle owned.
//
// Fully covered by lane_dig_read_test.go; restated here so the three arms of
// skipsForPlainEntry can be read as one set — two skipped, one asked.
func TestQuestionSet_PlainEntryStillAsksTheDig(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	laneID, shallow, _ := noneLaneWithTwoSlots(t, db, "QS-PLAIN-DIG")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	line, err := db.GetNodeByDotName(sd.LineNode.Name)
	if err != nil {
		t.Fatalf("resolve line node: %v", err)
	}

	foreign := digHolder(t, db, "qs-plain-dig-foreign")
	if !d.laneLock.TryLock(laneID, foreign.ID) {
		t.Fatal("foreign dig could not take the lane")
	}

	order := admitOrder(t, db, "qs-plain-dig", shallow)
	admitted, cause, laneName, err := d.AcquireLanesForOrder(order, shallow, line, EntryFreshBin)
	if err != nil {
		t.Fatalf("AcquireLanesForOrder: %v", err)
	}
	if admitted || cause != CauseLaneDigActive {
		t.Fatalf("admitted=%v cause=%q, want refused with %q — the dig question is the one this path "+
			"does ask, and it is mode-independent", admitted, cause, CauseLaneDigActive)
	}
	if laneName == "" {
		t.Error("no lane name on the refusal; the operator sentence names the contended lane, and " +
			"carrying it is why GateVerdict gained a lane field when digRefusalFor was folded in")
	}
}

// TestQuestionSet_PlainEntryStillAsksOccupancyOnAnUnmarkedLane is the NARROWNESS
// assertion for entryWhenGated, and it is the mistake worth pinning.
//
// The flag defers the in-lane questions to the gate for a lane that HAS a mark,
// because only there does the create end outside the corridor and the tail append
// record presence instead. A lane with no mark has no gate and no wait point — its
// dispatch drives the robot straight in — so deferring there would mean nobody
// asks at all, on every lane at both plants.
//
// The plausible wrong implementation is deferring for any lane in a group that
// has SOME gated lane in it, or for any lane at all. Either would silently delete
// the unification's refusal on the unmarked majority while every gate test stayed
// green.
//
// MUTATION (verified): make entryDeferredToGate return true whenever the caller
// declares the flag, without consulting the lane. This fires — the occupied
// unmarked lane admits.
func TestQuestionSet_PlainEntryStillAsksOccupancyOnAnUnmarkedLane(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	_, laneID, shallow := gatedLane(t, db, "QS-UNMARKED-OCC", "") // no mark: not gated
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	line, err := db.GetNodeByDotName(sd.LineNode.Name)
	if err != nil {
		t.Fatalf("resolve line node: %v", err)
	}
	inside := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "qs-mouth-occ-inside" })
	if _, err := reservations.AcquireOccupancy(db.DB, inside.ID, laneID); err != nil {
		t.Fatalf("acquire occupancy: %v", err)
	}

	store := admitOrder(t, db, "qs-mouth-occ", shallow)
	admitted, cause, _, err := d.AcquireLanesForOrder(store, line, shallow, EntryFreshBin)
	if err != nil {
		t.Fatalf("AcquireLanesForOrder: %v", err)
	}
	if admitted || cause != CauseLaneOccupied {
		t.Fatalf("a plain store was admitted into an occupied UNMARKED lane (admitted=%v cause=%q). "+
			"There is no gate on a lane with no mark: the robot goes straight in at dispatch, so "+
			"this is the only moment the question can be asked", admitted, cause)
	}
}

// TestQuestionSet_GateStagedRetrieveAsksOccupancy — INVERTED by the unification,
// and the inversion is a consequence rather than a choice.
//
// The skip's justification was that occupancy stood in for nothing missing: the
// only orders that HELD occupancy rows were compound legs, whose parent holds
// the dig that this caller already refuses against. That was true when written
// and the unification made it false — plain orders now take occupancy rows, and
// a plain store inside a lane holds no dig and, on `none`, no mouth row either.
//
// So a gate-staged retrieve that skipped occupancy would be RELEASED into a lane
// another robot is placing in: the collision Hold B exists to prevent, reached
// through the release path instead of the dispatch path.
//
// Safe to refuse here in the way the reachability skip is not: this refusal has
// a named releaser by construction. The order keeps dwelling at its gate point
// and the evaluator re-runs on every lane-clearing event
// (engine/wiring_lane_gate.go). Dwelling until the lane is free is what a gate
// wait is FOR.
//
// MUTATION (verified): re-add an unconditional `occupancy bool` to
// admissionSkips, have admitLane honour it, and set it on
// laneGateRetrieveCause. This fires. (The field itself is gone — C4 found it had
// no setter left — so the mutation now has to put it back before it can be set.)
func TestQuestionSet_GateStagedRetrieveAsksOccupancy(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	laneID, shallow, _ := noneLaneWithTwoSlots(t, db, "QS-RETR-OCC")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// The retrieve's own bin, in the mouth slot: reachable, nothing in front, so
	// the only thing that can refuse is the occupancy read under test.
	wanted := testdb.CreateBinAtNode(t, db, "DEFAULT", shallow.ID, "QS-RETR-OCC-BIN")

	inside := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "qs-retr-occ-inside" })
	if _, err := reservations.AcquireOccupancy(db.DB, inside.ID, laneID); err != nil {
		t.Fatalf("acquire occupancy: %v", err)
	}

	retrieve := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "qs-retr-occ"
		o.StationID = "line-1"
		o.SourceNode = shallow.Name
		o.BinID = &wanted.ID
	})

	v, err := d.laneGateRetrieveCause(shallow, retrieve)
	if err != nil {
		t.Fatalf("laneGateRetrieveCause: %v", err)
	}
	if v.Admitted() || v.Cause() != CauseLaneOccupied {
		t.Fatalf("a gate-staged retrieve was released into a lane another order is inside "+
			"(admitted=%v cause=%q). The mouth gate cannot see that order — on `none` it holds no "+
			"mouth row — and it holds no dig either, so nothing else on this path refuses it",
			v.Admitted(), v.Cause())
	}
}

// TestQuestionSet_GateStagedRetrieveAsksDigAndReachability is the positive half
// of the same set: both questions the hand-written classifier asked, still
// asked, with the same two causes.
func TestQuestionSet_GateStagedRetrieveAsksDigAndReachability(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	laneID, shallow, deep := noneLaneWithTwoSlots(t, db, "QS-RETR-POS")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// Reachability: a bin in the mouth slot buries the one the retrieve wants.
	testdb.CreateBinAtNode(t, db, "DEFAULT", shallow.ID, "QS-RETR-BLOCKER")
	wanted := testdb.CreateBinAtNode(t, db, "DEFAULT", deep.ID, "QS-RETR-WANTED")

	buried := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "qs-retr-buried"
		o.StationID = "line-1"
		o.SourceNode = deep.Name
		o.BinID = &wanted.ID
	})

	v, err := d.laneGateRetrieveCause(deep, buried)
	if err != nil {
		t.Fatalf("laneGateRetrieveCause (buried): %v", err)
	}
	if v.Admitted() || v.Cause() != CauseLaneTargetBuried {
		t.Errorf("buried retrieve: admitted=%v cause=%q, want refused with %q",
			v.Admitted(), v.Cause(), CauseLaneTargetBuried)
	}

	// The dig arm. Planted second so the burial arm above was read without it —
	// a dig excludes everything and would mask the reachability verdict.
	foreign := digHolder(t, db, "qs-retr-foreign-dig")
	if !d.laneLock.TryLock(laneID, foreign.ID) {
		t.Fatal("foreign dig could not take the lane")
	}
	v, err = d.laneGateRetrieveCause(deep, buried)
	if err != nil {
		t.Fatalf("laneGateRetrieveCause (dig): %v", err)
	}
	if v.Admitted() || v.Cause() != CauseLaneDigActive {
		t.Errorf("dig-held retrieve: admitted=%v cause=%q, want refused with %q — a dig excludes "+
			"everything and is asked before the burial",
			v.Admitted(), v.Cause(), CauseLaneDigActive)
	}
}

// TestQuestionSet_CompoundLegSkipsNothing pins the third caller by pinning the
// ZERO VALUE, which is what AdvanceCompoundOrder passes.
//
// The arms themselves are covered by admission_docker_test.go (dig, occupancy,
// reachability) and compound_admission_docker_test.go. What is NOT covered
// elsewhere, and is the thing that would break silently, is the direction of the
// default: an admissionSkips whose zero value SKIPPED would make every caller
// that forgets the field fail open — the shape GateVerdict's zero value exists
// to make unreachable, arriving through the field beside it.
//
// MUTATION: invert the struct to an "asks" set (zero value asks nothing) and
// this fires — the occupied lane admits.
func TestQuestionSet_CompoundLegSkipsNothing(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	laneID, shallow, _ := noneLaneWithTwoSlots(t, db, "QS-COMPOUND")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	inside := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "qs-compound-inside" })
	if _, err := reservations.AcquireOccupancy(db.DB, inside.ID, laneID); err != nil {
		t.Fatalf("acquire occupancy: %v", err)
	}

	leg := admitOrder(t, db, "qs-compound-leg", shallow)
	// The zero skip set, spelled the way AdvanceCompoundOrder spells it: omitted.
	v, err := d.admit(admissionSituation{order: leg, sourceNode: shallow})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if v.Admitted() || v.Cause() != CauseLaneOccupied {
		t.Fatalf("a caller that omitted the skip set was ADMITTED into an occupied lane "+
			"(admitted=%v cause=%q). The zero value must ask everything: forgetting has to be the "+
			"safe direction, or a new lane-entry path fails open",
			v.Admitted(), v.Cause())
	}
}
