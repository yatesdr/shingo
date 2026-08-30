// operator_release_hop_guard_test.go — regression tests for the Hopkinsville
// press-index index-leg hang (2026-07-23). The consolidated two-robot RELEASE
// must gate each leg on Core's own release precondition (hop A4-i) and re-fire
// a deferred leg when it later reaches staged, its sibling having already gone
// (hop A4-ii).
package engine

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
)

// TestReleaseStagedOrders_HeldSupplyLegNotReleasedNorDesynced is the hop A4-i
// regression: the operator's consolidated two-robot RELEASE gates each leg on
// orders.ReleasableAtCore. The staged evac releases; a supply leg still at
// sourcing is SKIPPED, not force-flipped to in_transit. Before the fix the
// fan-out used releaseUnlessTerminal (IsTerminal only), which moved the Edge
// row to in_transit and then rolled it back on Core's invalid_state — the
// persistent divergence that hid the RELEASE button on the press-index hang.
func TestReleaseStagedOrders_HeldSupplyLegNotReleasedNorDesynced(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "HOP-A4I", PayloadCode: "PART-A4I", UOPCapacity: 1200, InitialUOP: 800,
	})
	// seedTwoRobotPair: orderA -> ActiveOrderID (supply), orderB -> StagedOrderID
	// (evac). ResolveSwapPair maps those slots to supply/evac respectively.
	supplyID, evacID := seedTwoRobotPair(t, db, nodeID, "uuid-a4i", "two_robot")

	// Supply leg is NOT yet releasable — held at sourcing, exactly the state the
	// pre-hop fan-out optimistically force-flipped to in_transit.
	testutil.MustNoErr(t, db.UpdateOrderStatus(supplyID, string(protocol.StatusSourcing)), "hold supply at sourcing")

	// Drain seed/setup envelopes so the release-envelope count is exact.
	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	eng := testEngine(t, db)
	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: "test-op"}), "ReleaseStagedOrders with a held supply leg")

	// Exactly one release envelope: the staged evac. The held supply is skipped,
	// so Core is never handed a release it would refuse.
	if releases := findOutboxByType(t, db, protocol.TypeOrderRelease); len(releases) != 1 {
		t.Errorf("OrderRelease envelopes = %d, want 1 (evac only; a held supply must not queue one)", len(releases))
	}

	// The desync assertion: the supply leg must stay exactly where it was.
	supply, err := db.GetOrder(supplyID)
	if err != nil {
		t.Fatalf("re-read supply: %v", err)
	}
	if supply.Status != protocol.StatusSourcing {
		t.Errorf("supply status = %q, want %q — a skipped release must not move the Edge row",
			supply.Status, protocol.StatusSourcing)
	}

	// The evac actually released (advanced past staged).
	evac, err := db.GetOrder(evacID)
	if err != nil {
		t.Fatalf("re-read evac: %v", err)
	}
	if evac.Status == protocol.StatusStaged {
		t.Errorf("evac status = %q, want past staged — the staged evac must still release", evac.Status)
	}
}

// TestReleaseStagedOrders_RefiresDeferredSiblingOnStaged is the hop A4-ii
// regression: a leg deferred by the consolidated RELEASE (Core would have
// refused it) fires the release the operator already intended the moment it
// later reaches staged — its sibling having already gone.
func TestReleaseStagedOrders_RefiresDeferredSiblingOnStaged(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "HOP-A4II", PayloadCode: "PART-A4II", UOPCapacity: 1200, InitialUOP: 800,
	})
	supplyID, _ := seedTwoRobotPair(t, db, nodeID, "uuid-a4ii", "two_robot")

	// Supply held pre-staged; evac at staged. The consolidated release fans out:
	// the evac releases, the supply is deferred (Core would refuse it now).
	testutil.MustNoErr(t, db.UpdateOrderStatus(supplyID, string(protocol.StatusSourcing)), "hold supply at sourcing")

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: "test-op"}), "ReleaseStagedOrders")

	// Precondition: the supply really was deferred, not released.
	supply, _ := db.GetOrder(supplyID)
	if supply.Status != protocol.StatusSourcing {
		t.Fatalf("precondition: supply status = %q, want sourcing (must have been deferred)", supply.Status)
	}

	// Drain the evac's release envelope so the re-fire's is the only one we count.
	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	// Supply now reaches staged. In production Core's order.staged push drives
	// this transition and the engine bridges it onto the event bus; here we set
	// the row and fire the same EventOrderStatusChanged the lifecycle emits.
	testutil.MustNoErr(t, db.UpdateOrderStatus(supplyID, string(protocol.StatusStaged)), "supply reaches staged")
	eng.Events.Emit(Event{Type: EventOrderStatusChanged, Payload: OrderStatusChangedEvent{
		OrderID: supplyID, NewStatus: string(protocol.StatusStaged),
	}})

	// The deferred release fired: supply advanced past staged, one OrderRelease
	// envelope queued.
	supply, _ = db.GetOrder(supplyID)
	if supply.Status == protocol.StatusStaged {
		t.Errorf("supply status = %q, want past staged — the deferred release must re-fire when the leg reaches staged", supply.Status)
	}
	if releases := findOutboxByType(t, db, protocol.TypeOrderRelease); len(releases) != 1 {
		t.Errorf("OrderRelease envelopes after re-fire = %d, want 1 (the deferred supply)", len(releases))
	}
}

// TestSiblingReleaseRefire_NoRefireWithoutPriorRelease is the safety property:
// a leg reaching staged must NOT auto-release unless its sibling already
// released on an operator click. Without a recorded deferral there is no
// operator intent, so the handler must do nothing — never auto-release, never
// cancel, never re-plan.
func TestSiblingReleaseRefire_NoRefireWithoutPriorRelease(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "HOP-A4II-NEG", PayloadCode: "PART-A4IIN", UOPCapacity: 1200, InitialUOP: 800,
	})
	supplyID, _ := seedTwoRobotPair(t, db, nodeID, "uuid-a4ii-neg", "two_robot")

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// No consolidated RELEASE was ever clicked, so nothing is recorded as a
	// deferred sibling. Drain setup envelopes.
	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	// The supply leg reaches staged on its own.
	testutil.MustNoErr(t, db.UpdateOrderStatus(supplyID, string(protocol.StatusStaged)), "supply reaches staged")
	eng.Events.Emit(Event{Type: EventOrderStatusChanged, Payload: OrderStatusChangedEvent{
		OrderID: supplyID, NewStatus: string(protocol.StatusStaged),
	}})

	// Nothing must have been released — the operator never clicked.
	if releases := findOutboxByType(t, db, protocol.TypeOrderRelease); len(releases) != 0 {
		t.Errorf("OrderRelease envelopes = %d, want 0 — a leg with no recorded operator release must not auto-fire", len(releases))
	}
	supply, _ := db.GetOrder(supplyID)
	if supply.Status != protocol.StatusStaged {
		t.Errorf("supply status = %q, want staged unchanged (no auto-release)", supply.Status)
	}
}

// ── THE SWAP SURVIVOR: a partner that FINISHED, not one that died ─────────
//
// The five tests below are the swap-orphan round (run 12d, orders 84/85). The
// population is a leg standing `staged` whose swap partner already ran its half
// and CONFIRMED. Nothing anywhere reacted to that: every peer-terminal arm keys
// on a death, and the deferral map's entry — if there ever was one — was
// consumed at an earlier wait or lost to a restart.
//
// stageSurvivorWithPartner builds that shape: a linked pair, the partner in the
// given terminal (or live) status, the survivor arriving at staged with NOTHING
// in the deferral map. It deliberately never calls ReleaseStagedOrders, because
// the whole point is the arm that fires when no click is on record.
func stageSurvivorWithPartner(t *testing.T, prefix string, partnerStatus protocol.Status) (*Engine, *store.DB, int64) {
	t.Helper()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: prefix, PayloadCode: "PART-" + prefix, UOPCapacity: 1200, InitialUOP: 800,
	})
	survivorID, partnerID := seedTwoRobotPair(t, db, nodeID, "uuid-"+prefix, "two_robot")
	testutil.MustNoErr(t, db.UpdateOrderStatus(partnerID, string(partnerStatus)), "partner status")

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Drain seed/setup envelopes so a release envelope is unambiguous.
	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}
	return eng, db, survivorID
}

// arriveAtStaged puts the survivor at staged and fires the same
// EventOrderStatusChanged the lifecycle emits when Core's order.staged push
// lands.
func arriveAtStaged(t *testing.T, eng *Engine, db *store.DB, orderID int64) {
	t.Helper()
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(protocol.StatusStaged)), "survivor reaches staged")
	eng.Events.Emit(Event{Type: EventOrderStatusChanged, Payload: OrderStatusChangedEvent{
		OrderID: orderID, NewStatus: string(protocol.StatusStaged),
	}})
}

// TestSwapSurvivor_PartnerConfirmedReleasesWithNoClick is case (a), and it is
// RED at 7883ad94: the survivor stages, no map entry exists, and before
// releaseSurvivorOfFinishedPartner nothing looked at the partner at all.
//
// Order 84 held AMR-15 for the rest of a run in exactly this state while its
// partner 85 had confirmed a minute earlier.
func TestSwapSurvivor_PartnerConfirmedReleasesWithNoClick(t *testing.T) {
	t.Parallel()
	eng, db, survivorID := stageSurvivorWithPartner(t, "SURVOK", protocol.StatusConfirmed)

	arriveAtStaged(t, eng, db, survivorID)

	survivor, _ := db.GetOrder(survivorID)
	if survivor.Status == protocol.StatusStaged {
		t.Errorf("survivor status = %q, want past staged — its partner already confirmed, so nothing "+
			"is coming and nobody is going to click", survivor.Status)
	}
	if releases := findOutboxByType(t, db, protocol.TypeOrderRelease); len(releases) != 1 {
		t.Errorf("OrderRelease envelopes = %d, want 1 (the survivor's)", len(releases))
	}
}

// TestSwapSurvivor_DeadPartnerIsLeftToTheUnwind is case (b). A partner that
// FAILED is HandleSwapPeerTerminal's business on the Core side — it cancels the
// survivor, correctly. Releasing it here would send a robot on a leg that is
// about to be unwound, and would race the cancel to do it.
//
// The distinction is the whole reason IsTerminalSuccess exists. Every status in
// this test is terminal; only one of them means the partner did its half.
func TestSwapSurvivor_DeadPartnerIsLeftToTheUnwind(t *testing.T) {
	t.Parallel()
	for i, partnerStatus := range []protocol.Status{
		protocol.StatusFailed, protocol.StatusCancelled, protocol.StatusSkipped,
	} {
		prefix := fmt.Sprintf("SURVDEAD%d", i)
		t.Run(string(partnerStatus), func(t *testing.T) {
			t.Parallel()
			eng, db, survivorID := stageSurvivorWithPartner(t, prefix, partnerStatus)

			arriveAtStaged(t, eng, db, survivorID)

			survivor, _ := db.GetOrder(survivorID)
			if survivor.Status != protocol.StatusStaged {
				t.Errorf("survivor status = %q, want staged unchanged — a partner that %s is unwound "+
					"by Core, not released by the Edge", survivor.Status, partnerStatus)
			}
			if releases := findOutboxByType(t, db, protocol.TypeOrderRelease); len(releases) != 0 {
				t.Errorf("OrderRelease envelopes = %d, want 0 — a dead partner must not trigger a release",
					len(releases))
			}
		})
	}
}

// TestSwapSurvivor_LivePartnerIsUntouched is case (d), and it is the safety
// property that keeps this arm from being an auto-release.
//
// A partner still in flight is the ORDINARY swap: the other robot is coming, the
// line has not cleared, and the wait is the system working. Releasing the
// survivor now is the two-bins-on-one-node collision
// refusePlacingLegWhileSiblingPending exists to prevent.
func TestSwapSurvivor_LivePartnerIsUntouched(t *testing.T) {
	t.Parallel()
	eng, db, survivorID := stageSurvivorWithPartner(t, "SURVLIVE", protocol.StatusInTransit)

	arriveAtStaged(t, eng, db, survivorID)

	survivor, _ := db.GetOrder(survivorID)
	if survivor.Status != protocol.StatusStaged {
		t.Errorf("survivor status = %q, want staged unchanged — its partner is still in flight",
			survivor.Status)
	}
	if releases := findOutboxByType(t, db, protocol.TypeOrderRelease); len(releases) != 0 {
		t.Errorf("OrderRelease envelopes = %d, want 0 — a live partner means the swap is mid-flight",
			len(releases))
	}
}

// TestSwapSurvivor_SurvivesEdgeRestart is case (c). pendingSiblingRelease is an
// in-memory map, so an Edge restart between the operator's click and the leg
// reaching staged loses the deferral entirely — the fourth drop point, and the
// one no amount of care at the click site can fix.
//
// The restart is modelled the way it actually presents: a SECOND Engine over the
// same database, with an empty map, receiving the staged transition. Nothing but
// the durable read can answer there.
func TestSwapSurvivor_SurvivesEdgeRestart(t *testing.T) {
	t.Parallel()
	_, db, survivorID := stageSurvivorWithPartner(t, "SURVBOOT", protocol.StatusConfirmed)

	// The Edge restarts: a fresh Engine over the same store, nothing remembered.
	rebooted := testEngine(t, db)
	rebooted.wireEventHandlers()
	rebooted.pendingSiblingReleaseMu.Lock()
	mapEmpty := len(rebooted.pendingSiblingRelease) == 0
	rebooted.pendingSiblingReleaseMu.Unlock()
	if !mapEmpty {
		t.Fatalf("precondition: a rebooted Engine must start with no deferrals remembered")
	}

	arriveAtStaged(t, rebooted, db, survivorID)

	survivor, _ := db.GetOrder(survivorID)
	if survivor.Status == protocol.StatusStaged {
		t.Errorf("survivor status = %q, want past staged — the durable re-derivation is what makes "+
			"this survive a restart", survivor.Status)
	}
}

// TestSwapSurvivor_FiresAtMostOnce is the bound, and it is not tidiness.
//
// "my partner confirmed and I am staged" is LEVEL-triggered: it stays true. Core
// refuses a release for a wait its own lane evaluator owns and the Edge rolls the
// leg back to staged, which is itself a staged transition — so an unbounded arm
// re-fires forever. That is the 1.25s refusal flap measured on the lane-stress
// rig (240 refusals in five minutes, 1796 outbox rows for 46 completed orders),
// and run 12d's order 232 is a live example of the population that would trip it:
// a staged leg whose partner confirmed, parked at a LANE wait.
//
// The Edge cannot tell the two waits apart — protocol.OrderStaged carries a uuid
// and a detail string, no wait index and no wait kind — so the bound is what
// stands in for the discriminator it is not sent.
func TestSwapSurvivor_FiresAtMostOnce(t *testing.T) {
	t.Parallel()
	eng, db, survivorID := stageSurvivorWithPartner(t, "SURVONCE", protocol.StatusConfirmed)

	arriveAtStaged(t, eng, db, survivorID)
	if releases := findOutboxByType(t, db, protocol.TypeOrderRelease); len(releases) != 1 {
		t.Fatalf("precondition: OrderRelease envelopes after first staging = %d, want 1", len(releases))
	}

	// Core refuses (a lane wait) and the Edge rolls the leg back to staged. The
	// arm must not fire again.
	arriveAtStaged(t, eng, db, survivorID)

	if releases := findOutboxByType(t, db, protocol.TypeOrderRelease); len(releases) != 1 {
		t.Errorf("OrderRelease envelopes after a second staging = %d, want 1 — an unbounded arm is "+
			"the refusal flap, not a fix", len(releases))
	}
}

// TestReleaseStagedOrders_DefersWhenSiblingAlreadyFinished is the deferral
// predicate's own RED case, and it is about the CLICK rather than the staging.
//
// Two things made releaseIfReleasable return false, and the arms could not tell
// them apart: "the sibling is not releasable yet" and "the sibling already ran
// and confirmed". Only the first justifies dropping the deferral. So an operator
// who clicked RELEASE on a pair whose other half had already finished got
// nothing recorded at all — the click expressed "go", and the leg that could not
// go yet was forgotten.
//
// Asserted on the MAP, not on the release, deliberately: the durable
// re-derivation would also carry this leg when it stages, so an outbox-only
// assertion would pass with this arm removed and prove nothing about it.
//
// MUTATION (verified): restore `evacReleased && !supplyReleased` at the call
// site and this fires — nothing is remembered.
func TestReleaseStagedOrders_DefersWhenSiblingAlreadyFinished(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "SURVDEFER", PayloadCode: "PART-SURVDEFER", UOPCapacity: 1200, InitialUOP: 800,
	})
	supplyID, evacID := seedTwoRobotPair(t, db, nodeID, "uuid-survdefer", "two_robot")

	// The shape run 12d produced: the partner already ran its half and confirmed
	// BEFORE the click, and this leg is not yet releasable at Core.
	testutil.MustNoErr(t, db.UpdateOrderStatus(evacID, string(protocol.StatusConfirmed)), "evac confirmed")
	testutil.MustNoErr(t, db.UpdateOrderStatus(supplyID, string(protocol.StatusSourcing)), "hold supply at sourcing")

	eng := testEngine(t, db)
	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID,
		ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: "test-op"}), "ReleaseStagedOrders")

	eng.pendingSiblingReleaseMu.Lock()
	_, remembered := eng.pendingSiblingRelease[supplyID]
	eng.pendingSiblingReleaseMu.Unlock()
	if !remembered {
		t.Errorf("supply leg %d was not recorded as a deferred sibling release — its partner had "+
			"already confirmed, which is the operator's click having been honoured on one half and "+
			"dropped on the other", supplyID)
	}

	// The click must not have MOVED the un-releasable leg: deferring is the point.
	supply, _ := db.GetOrder(supplyID)
	if supply.Status != protocol.StatusSourcing {
		t.Errorf("supply status = %q, want sourcing — a deferral records, it does not release",
			supply.Status)
	}
}

// TestSwapSurvivor_PartnerFinishesAfterTheSurvivorIsAlreadyParked is order 112,
// found on the board rather than in a snapshot, and it is the OTHER order of
// events.
//
// ── WHY THE STAGED ARM ALONE IS NOT ENOUGH ────────────────────────────────
//
// The arm above fires when the SURVIVOR stages and finds its partner already
// finished. That covers order 84, where the partner confirmed first. It does not
// cover the survivor that is ALREADY parked when its partner finishes: there is
// no staged transition left to fire on, so nothing looks.
//
// MEASURED, run 2026-08-30. Order 112 was released past its first wait at
// 14:19:24 and drove to its second, where the fleet reported it WAITING — but
// Core never wrote a second `staged`, so its row sat at `in_transit` with the
// robot parked. Its partner 111 confirmed at 14:20:23. 112 then held AMR-19 for
// 28 minutes under a board reading "Waiting for partner robot", about a partner
// that had finished.
//
// `in_transit` IS releasable at Core — orders.ReleasableAtCore accepts it "for
// multi-wait re-release", which is exactly this — so nothing about the release
// needed changing. What was missing was anything to ask for it.
//
// RED without the terminal arm: the survivor stays in_transit and no envelope is
// queued.
func TestSwapSurvivor_PartnerFinishesAfterTheSurvivorIsAlreadyParked(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "SURVLATE", PayloadCode: "PART-SURVLATE", UOPCapacity: 1200, InitialUOP: 800,
	})
	survivorID, partnerID := seedTwoRobotPair(t, db, nodeID, "uuid-survlate", "two_robot")

	// The survivor is PARKED PAST ITS FIRST WAIT: released once, driving, and the
	// fleet has it standing at the next one. Core's row says in_transit — the
	// state order 112 sat in for 28 minutes.
	testutil.MustNoErr(t, db.UpdateOrderStatus(survivorID, string(protocol.StatusInTransit)), "survivor in transit")

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	// NOW the partner finishes. This is the only event left.
	testutil.MustNoErr(t, db.UpdateOrderStatus(partnerID, string(protocol.StatusConfirmed)), "partner confirmed")
	eng.Events.Emit(Event{Type: EventOrderStatusChanged, Payload: OrderStatusChangedEvent{
		OrderID: partnerID, NewStatus: string(protocol.StatusConfirmed),
	}})

	if releases := findOutboxByType(t, db, protocol.TypeOrderRelease); len(releases) != 1 {
		t.Errorf("OrderRelease envelopes = %d, want 1 — the survivor is parked at a wait its partner "+
			"has already made moot, and the staged transition it would have been caught on is behind "+
			"it. Nothing else is coming to ask", len(releases))
	}
	// THE ENVELOPE IS THE EVIDENCE, NOT THE LOCAL STATUS, and the difference is
	// worth stating: a staged→in_transit release moves the Edge row, and an
	// in_transit RE-release has nowhere local to move it to — the row is already
	// where the release would put it. Core does the work from here (it re-reads
	// the wait index and appends the next segment). Asserting the status would
	// have failed for a release that fired correctly.
	if _, err := db.GetOrder(survivorID); err != nil {
		t.Fatalf("reload survivor: %v", err)
	}
}

// TestSwapSurvivor_PartnerDyingLateDoesNotRelease is the terminal arm's safety
// property, and the mirror of the dead-partner case above: the arm fires on a
// partner that FINISHED, never on one that died. A partner that failed or was
// cancelled is HandleSwapPeerTerminal's business, and it unwinds the survivor —
// releasing it here would send a robot on a leg about to be cancelled, and race
// the cancel to do it.
func TestSwapSurvivor_PartnerDyingLateDoesNotRelease(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "SURVLATEDEAD", PayloadCode: "PART-SURVLD", UOPCapacity: 1200, InitialUOP: 800,
	})
	survivorID, partnerID := seedTwoRobotPair(t, db, nodeID, "uuid-survlatedead", "two_robot")
	testutil.MustNoErr(t, db.UpdateOrderStatus(survivorID, string(protocol.StatusInTransit)), "survivor in transit")

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	testutil.MustNoErr(t, db.UpdateOrderStatus(partnerID, string(protocol.StatusFailed)), "partner failed")
	eng.Events.Emit(Event{Type: EventOrderStatusChanged, Payload: OrderStatusChangedEvent{
		OrderID: partnerID, NewStatus: string(protocol.StatusFailed),
	}})

	if releases := findOutboxByType(t, db, protocol.TypeOrderRelease); len(releases) != 0 {
		t.Errorf("OrderRelease envelopes = %d, want 0 — a partner that FAILED is unwound by Core, "+
			"not answered by releasing the survivor into the cancel", len(releases))
	}
}

// TestSwapSurvivor_AnAttemptThatSendsNothingDoesNotSpendTheShot is order 123,
// and it is a defect in the bound rather than in the arm.
//
// ── WHAT WENT WRONG ───────────────────────────────────────────────────────
//
// The one-shot bound marked the order BEFORE attempting. Run 2026-08-30, order
// 123 (partner 122 confirmed): the arm fired while 123 was mid-flap and
// releaseIfReleasable answered "not releasable at Core", so no envelope was ever
// sent — and the shot was gone. 123 then settled into `in_transit`, which IS
// releasable, and nothing could ask again. It stood 26 minutes holding AMR-13.
// Forty-four of that run's eighty-nine survivor log lines were the same wasted
// shot.
//
// ── WHY COUNTING ENVELOPES STILL BOUNDS THE FLAP ──────────────────────────
//
// The bound exists to stop a refusal flap: Core refuses a release for a lane
// wait, the Edge rolls the leg back to `staged`, and that rollback is itself a
// staged transition. In THAT path the release genuinely fires — the order is
// staged, so an envelope goes out — so counting envelopes bounds it at exactly
// one, which is what TestSwapSurvivor_FiresAtMostOnce still asserts. What stops
// being counted is an attempt that never reached Core and therefore cannot have
// flapped anything.
//
// RED before the fix: the second arrival sends nothing, because the first
// no-op attempt consumed the order's only shot.
func TestSwapSurvivor_AnAttemptThatSendsNothingDoesNotSpendTheShot(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "SURVSHOT", PayloadCode: "PART-SURVSHOT", UOPCapacity: 1200, InitialUOP: 800,
	})
	survivorID, partnerID := seedTwoRobotPair(t, db, nodeID, "uuid-survshot", "two_robot")
	testutil.MustNoErr(t, db.UpdateOrderStatus(partnerID, string(protocol.StatusConfirmed)), "partner confirmed")

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	// FIRST ARRIVAL, AND IT IS A STALE EVENT — which is how order 123 reached
	// this state. The bus delivers a `staged` transition, but by the time
	// releaseIfReleasable re-reads the row the order has already moved on (the
	// refusal flap runs about once a second). The arm asks, learns it cannot
	// send, and must send nothing — and must not treat that as its one shot.
	testutil.MustNoErr(t, db.UpdateOrderStatus(survivorID, string(protocol.StatusSourcing)), "the row moves on")
	eng.Events.Emit(Event{Type: EventOrderStatusChanged, Payload: OrderStatusChangedEvent{
		OrderID: survivorID, NewStatus: string(protocol.StatusStaged),
	}})
	if releases := findOutboxByType(t, db, protocol.TypeOrderRelease); len(releases) != 0 {
		t.Fatalf("precondition: OrderRelease envelopes after the un-releasable arrival = %d, want 0",
			len(releases))
	}

	// IT SETTLES SOMEWHERE RELEASABLE. This is order 123's actual end state:
	// parked at a later wait, in_transit, with the fleet reporting WAITING.
	testutil.MustNoErr(t, db.UpdateOrderStatus(survivorID, string(protocol.StatusStaged)), "survivor stages")
	eng.Events.Emit(Event{Type: EventOrderStatusChanged, Payload: OrderStatusChangedEvent{
		OrderID: survivorID, NewStatus: string(protocol.StatusStaged),
	}})

	if releases := findOutboxByType(t, db, protocol.TypeOrderRelease); len(releases) != 1 {
		t.Errorf("OrderRelease envelopes = %d, want 1 — the first attempt sent nothing, so it cannot "+
			"have been the one shot. Spending it there is what left order 123 holding AMR-13 for 26 "+
			"minutes with its partner long finished", len(releases))
	}
}
