//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// lane_gate_mark_cleared_docker_test.go — the ROLLBACK rule.
//
// The mark is the enablement (lane_gate.go), and the enablement ruling comes
// with a rollback attached: "Each lane goes gated the day a human places its
// mark, and rollback is clearing it (robots already dwelling complete under the
// old rules)." There is a UI for exactly this — apiNodePropertySet /
// apiNodePropertyDelete on `lane_gate_point`, with a confirmation that counts
// the dwellers first (GateStagedCount, www/handlers_nodes.go apiLaneWaiting) —
// so the state these tests drive is one an admin can produce with two clicks.
//
// Every other lane-gate test SETS the mark in its fixture and leaves it set.
// That makes the mark look like a static fact, and the two behaviours below are
// the two ways it is not:
//
//   - a robot already dwelling at the mark must still be released, because it
//     is physically committed at a wait point and only Core can append its
//     tail. The mark decides where an order WAITS; it does not decide whether a
//     robot that is already waiting is ever let go.
//   - the next order into the now-unmarked lane must go back to deciding
//     BEFORE dispatch — parked in sourcing with a cause, no robot committed —
//     rather than being sent out to a mark that is no longer there.
//
// Both are driven through the same fixture and the same contention (a dig), so
// the only difference between the two dispositions is the property.

// The two tests use DIFFERENT contention, and the difference is not arbitrary.
// The dweller test uses a DIG, which is what holds a gate-staged store at the
// mark. The pre-dispatch test uses OCCUPANCY — a robot inside the lane — which
// is the disposition the catalog names for the unmarked case (`lane-occupied`)
// and the only one the mark actually moves. A dig writes a mouth row, and the
// mouth acquire refuses a dig-held lane marked or not, so a dig would answer the
// same on both sides of the property and prove nothing about it.

// TestGateMark_ClearedMidDwellStillReleasesTheDweller is the rollback rule's
// load-bearing half.
//
// A store is dispatched into a marked lane a dig holds. It ships unsealed, the
// robot drives to the mark and dwells there, and its tail — the dropoff that
// puts the bin down — exists only in steps_json. Then an admin clears the mark.
//
// THE ROBOT IS ALREADY OUT THERE. Clearing the mark cannot un-dispatch it: the
// fleet holds a live, unsealed waybill ending at a Wait block, and the ONLY
// thing that can finish that order is Core appending the tail. So the release
// evaluator has to keep working for this order after the property is gone.
//
// The evaluator's own contract is what makes this the right level to assert it:
// its candidate set is derived from the ORDER's durable state (gate-staged, wait
// kind lane, wait_lane = this lane), never from configuration, and
// GateStagedCount answers the admin's "how many are waiting" from that same
// derivation. A dweller the UI counts and the evaluator will not release is a
// robot the floor has to recover by hand.
//
// Vacuity: the dig is released before the pass, so the ONLY thing that could
// hold this order back is the cleared mark. If the evaluator declines here it is
// declining on the property, not on the lane.
//
// MUTATION (verified): restore the `if !d.laneIsGated(lane.ID) { return }`
// early-return at the top of EvaluateLaneReleases. The pass returns without
// looking at its candidates and this test's "append calls = 1" assertion fires
// with 0 — a robot dwelling at a mark that no longer exists, with no releaser.
func TestGateMark_ClearedMidDwellStillReleasesTheDweller(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	laneID, s0, _ := gateChoreoLane(t, db, "GCCLR", "GCCLR-WAIT")
	line := lineNode(t, db, "GCCLR-LINE")

	// A dig holds the lane, so the store ships unsealed and dwells at the mark.
	digger := testdb.CreateOrder(t, db)
	if !d.laneLock.TryLock(laneID, digger.ID) {
		t.Fatal("TryLock on a free lane must succeed")
	}

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = s0.Name
		o.Status = "sourcing"
	})
	if _, err := d.DispatchDirect(order, line, s0); err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}
	if n := len(backend.ReleaseCalls()); n != 0 {
		t.Fatalf("append calls after dispatch = %d, want 0 — a dig-held store holds its tail", n)
	}

	// PRECONDITION: the robot is genuinely committed at the mark. Without this
	// the release below would be releasing nothing and the mutation could not
	// fire.
	dwelling, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload the dweller: %v", err)
	}
	if !IsGateStaged(dwelling) {
		t.Fatalf("order must be gate-staged before the mark is cleared (steps=%q wait=%d vendor=%q)",
			dwelling.StepsJSON, dwelling.WaitIndex, dwelling.VendorOrderID)
	}
	if n, cErr := d.GateStagedCount(laneID); cErr != nil || n != 1 {
		t.Fatalf("GateStagedCount = %d (err %v), want 1 — this is the number the clear-the-mark "+
			"confirmation shows the admin, and the whole point of showing it is that these "+
			"orders survive the clear", n, cErr)
	}

	// ── The admin clears the mark ─────────────────────────────────────────
	if err := db.DeleteNodeProperty(laneID, PropLaneGatePoint); err != nil {
		t.Fatalf("clear the mark: %v", err)
	}
	if d.laneIsGated(laneID) {
		t.Fatal("fixture: the lane still reads as gated after its mark was deleted — the rest of " +
			"this test would be exercising the ordinary gated path")
	}
	// The dweller is unchanged by the clear: nothing rewrites a live plan, so its
	// wait still names this lane and it is still the evaluator's to release.
	if still, sErr := db.GetOrder(order.ID); sErr != nil || !IsGateStaged(still) {
		t.Fatalf("clearing the mark must not disturb the dwelling order's own state (err %v)", sErr)
	}

	// ── The lane becomes safe ─────────────────────────────────────────────
	d.laneLock.Unlock(laneID, digger.ID)
	d.EvaluateLaneReleases(laneID)

	if n := len(backend.ReleaseCalls()); n != 1 {
		t.Fatalf("append calls after the dig cleared = %d, want 1 — the mark was removed while a "+
			"robot was dwelling at it, and nothing else in the system can append that tail. The "+
			"order is unsealed at the fleet, the robot is parked at a point Core no longer "+
			"configures, and clearing a mark is documented as a rollback rather than an "+
			"abandonment (lane_gate.go)", n)
	}
	sealed, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload after release: %v", err)
	}
	if sealed.WaitIndex != 1 {
		t.Errorf("wait_index = %d, want 1 — the tail must advance the durable witness", sealed.WaitIndex)
	}
	if IsGateStaged(sealed) {
		t.Error("a released order must stop reading as gate-staged, or the next pass appends again")
	}
}

// TestGateMark_ClearedLaneParksTheNextOrderPreDispatch is the other half, and it
// is what stops the first from being read as "the mark does not matter".
//
// The SAME lane, the SAME occupant, the SAME order shape — and the mark is the
// only thing that changes. It flips the pre-dispatch answer from DEFER to
// REFUSE:
//
//   - marked: admission's arm 0 (entryDeferredToGate, admission.go) returns
//     Admitted because this caller's dispatch stops at the wait point OUTSIDE
//     the corridor. Who is inside is not this moment's question; the gate asks
//     it later, when the robot actually goes in.
//   - unmarked: there is no gate to defer to, so the occupancy arm runs and the
//     order is refused with lane-occupied. The scanner parks it in sourcing
//     holding its soft reservations and NO robot is committed.
//
// That is the whole of "parks pre-dispatch instead of taking the gate", and it
// matters because the two waiting rooms have different costs: a parked order
// costs a queue row, a dwelling one costs a robot.
//
// Both branches are asserted in one test on purpose. Either alone is
// satisfiable by a broken predicate — "always defer" passes the first, "always
// refuse" passes the second — and it is the PAIR that pins the derivation.
//
// MUTATION (verified): make entryDeferredToGate return `skip.entryWhenGated`
// without consulting laneIsGated. The unmarked branch then defers as well and
// this test's "want refused" assertion fires — the order would be shipped to a
// wait point that no longer exists.
func TestGateMark_ClearedLaneParksTheNextOrderPreDispatch(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	laneID, s0, _ := gateChoreoLane(t, db, "GCCLR2", "GCCLR2-WAIT")
	line := lineNode(t, db, "GCCLR2-LINE")

	// Somebody is INSIDE the lane — Hold B, the row a dispatch takes when Core
	// sends a robot into a corridor.
	occupant := testdb.CreateOrder(t, db)
	if _, err := reservations.AcquireOccupancy(db.DB, occupant.ID, laneID); err != nil {
		t.Fatalf("acquire occupancy: %v", err)
	}

	// MARKED: the occupancy question is deferred to the gate, so the order is
	// admitted and will be shipped out to dwell.
	marked := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = s0.Name
		o.Status = "sourcing"
	})
	admitted, cause, _, err := d.AcquireLanesForOrder(marked, line, s0, EntryFreshBin)
	if err != nil {
		t.Fatalf("acquire on the marked lane: %v", err)
	}
	if !admitted {
		t.Fatalf("a marked lane must ADMIT at dispatch and let the gate decide entry, got refused "+
			"with %q — refusing here is the pre-gate disposition the mark exists to replace, and "+
			"it strands the order's pre-lane work as well", cause)
	}

	// ── The admin clears the mark ─────────────────────────────────────────
	if err := db.DeleteNodeProperty(laneID, PropLaneGatePoint); err != nil {
		t.Fatalf("clear the mark: %v", err)
	}

	// UNMARKED: nothing to defer to, so the occupancy arm refuses here and now.
	next := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = s0.Name
		o.Status = "sourcing"
	})
	admitted, cause, laneName, err := d.AcquireLanesForOrder(next, line, s0, EntryFreshBin)
	if err != nil {
		t.Fatalf("acquire on the unmarked lane: %v", err)
	}
	if admitted {
		t.Fatal("an order into an unmarked lane another robot is inside must be refused BEFORE " +
			"dispatch — there is no mark left to dwell at, so admitting it sends a second robot " +
			"into a single-file corridor")
	}
	if cause != CauseLaneOccupied {
		t.Errorf("cause = %q, want %q — the park has to say why on the row, or an operator looking "+
			"at a stalled order sees a blank", cause, CauseLaneOccupied)
	}
	if laneName == "" {
		t.Error("expected the contended lane's name for the \"Waiting for a slot at ‹lane›\" sentence")
	}
}

// TestGateDwell_CarriesItsCauseAndClearsItOnEntry closes the operator gap F-11
// exposed: a dwelling robot with nothing on its row to say why.
//
// Three orders sat `staged` for 77 minutes on the lane-stress rig with a blank
// queue_cause. The board showed three robots doing nothing and no sentence
// anywhere explaining it — and the answer was in the evaluator's own verdict on
// every pass, thrown away into a debug log.
//
// DESIGN §16 rule 7: occupancy is the first refusal a dweller can take here. The
// lane is marked, the order is staged with its wait at index 0, and another
// order holds the lane — so the verdict is lane-occupied and that is what has to
// reach the row.
//
// MUTATION (verified 2026-08-10): delete the setQueueReason on the refusal arm
// of EvaluateLaneReleases. The cause assertion fires with an empty string, which
// is the state the rig sat in.
func TestGateDwell_CarriesItsCauseAndClearsItOnEntry(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	srcNode, _, bp := setupTestData(t, db)
	_, laneID, mouth := gatedLane(t, db, "DWELLCAUSE", "DWELLCAUSE-WAIT")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	blocker := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dwellcause-blocker"
		o.Status = StatusDispatched
	})
	testutil.MustNoErr(t, d.TakeLaneOccupancy(blocker.ID, mouth), "somebody is inside the lane")

	bin := testdb.CreateBinAtNode(t, db, bp.Code, srcNode.ID, "DWELLCAUSE-BIN")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dwellcause"
		o.OrderType = OrderTypeMove
		o.PayloadCode = bp.Code
		o.SourceNode = srcNode.Name
		o.DeliveryNode = mouth.Name
		o.Status = StatusSourcing
	})
	testutil.MustNoErr(t, db.UpdateOrderBinID(order.ID, bin.ID), "stamp the bin")
	order, _ = db.GetOrder(order.ID)
	_, dErr := d.DispatchDirect(order, srcNode, mouth)
	testutil.MustNoErr(t, dErr, "the gated dispatch must create, unsealed")

	// The evaluator passes over a dweller it cannot admit.
	d.EvaluateLaneReleases(laneID)

	held, _ := db.GetOrder(order.ID)
	if held.QueueCause == "" {
		t.Fatalf("order %d is dwelling at the mark with NO cause on its row. An operator sees a "+
			"robot parked and nothing that says why — which is how three of these went 77 minutes "+
			"unexplained on the rig.", order.ID)
	}
	if QueueCause(held.QueueCause) != CauseLaneOccupied {
		t.Errorf("cause = %q, want %q — the sentence has to name what it is actually waiting for",
			held.QueueCause, CauseLaneOccupied)
	}

	// The lane clears; the dweller goes in; the cause must not survive it.
	d.ReleaseLaneOccupancy(blocker.ID) // the blocker leaves the lane
	d.EvaluateLaneReleases(laneID)

	entered, _ := db.GetOrder(order.ID)
	if entered.QueueCause != "" {
		t.Errorf("order %d entered the lane still carrying cause %q — a stale wait on a robot that "+
			"is now driving is the same lie as the blank was, told the other way round",
			order.ID, entered.QueueCause)
	}
}
