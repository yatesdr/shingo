//go:build docker

package dispatch

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// lane_gate_pair_entry_docker_test.go — SME #18, answered: single file in the
// lane, and the wait lands on a robot at a gate point rather than on the press.
//
// A gated lane is single file, and the coordination that Tier 1 protects moves to
// where it costs nothing: the pair still DISPATCHES together, and only their
// ENTRIES serialize.
//
// Two claims, and they are different claims at different moments, which is the
// whole of the ruling:
//
//   - AT THE MOUTH the pair SERIALIZES. One tail per lane-clear, because the
//     first partner's append takes an occupancy row and the second partner's
//     verdict reads it. That is skipsForGatedStoreEntry's deleted skip.
//   - BEFORE THE FLEET CREATE neither partner is held on account of the other's
//     lane. Both robots go, both dwell at the gate. That is what preserves what
//     Tier 1 was protecting — the press never waits for the corridor.
//
// Tier 1 is live in both tests and is what makes them mean something: a
// cross-origin second store would be refused by Tier 2 (deeper-pending) and the
// occupancy question would never be reached, so the pair has to share a real
// origin from the plant-claims mirror or these prove nothing.

// pressOrigin seeds a (process, style) claim on a node, so two orders whose
// process_node is that node resolve to ONE origin key and Tier 1 co-releases
// them. Same seeding the evaluator's Tier-1 test uses.
func pressOrigin(t *testing.T, db *store.DB, press *nodes.Node, proc, style string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO process_styles (process_id, style_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
		proc, style); err != nil {
		t.Fatalf("seed process_styles: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO style_claims (process_id, style_id, core_node_name, role, swap_mode, payload_code, allowed_payload_codes, uop_capacity, reorder_point, seq)
		VALUES ($1,$2,$3,'',' ','', '[]', 0, 0, 0)`, proc, style, press.Name); err != nil {
		t.Fatalf("seed style_claims: %v", err)
	}
}

// assertSameOrigin fails unless the two orders really resolve to one non-empty
// origin key. Without it a fixture slip turns a Tier-1 test into a Tier-2 test
// that still passes for the wrong reason.
func assertSameOrigin(t *testing.T, d *Dispatcher, a, b *orders.Order) {
	t.Helper()
	oa, err := d.laneEntryOriginFor(a)
	if err != nil {
		t.Fatalf("origin for order %d: %v", a.ID, err)
	}
	ob, err := d.laneEntryOriginFor(b)
	if err != nil {
		t.Fatalf("origin for order %d: %v", b.ID, err)
	}
	if oa == "" || oa != ob {
		t.Fatalf("fixture: the pair must share ONE origin (got %q and %q) — otherwise Tier 2 "+
			"refuses the shallower partner and the occupancy question is never asked", oa, ob)
	}
}

// TestGatePair_SameOriginPairSerializesAtTheMouth is the ruling's first half: two
// same-origin stores staged at one gated lane, and when the lane clears exactly
// ONE goes in.
//
// The pair is co-released by Tier 1 — the classifier admits BOTH in the same
// evaluator pass, which is the behaviour the press needs — and the physical
// question is what holds the second one back. So the second partner's refusal
// must carry CauseLaneOccupied and NOT a tier cause: that distinction is the
// evidence that Tier 1 still fires and that single-file is being enforced
// underneath it rather than instead of it.
//
// MUTATION (verified): restore `occupancy: true` to skipsForGatedStoreEntry.
// Two appends land in one pass and two robots enter one single-file corridor.
func TestGatePair_SameOriginPairSerializesAtTheMouth(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	laneID, _, _ := gateChoreoLane(t, db, "GPSER", "GPSER-WAIT")
	// THREE slots: the blocker needs a slot deeper than both partners, or the pair
	// sails through the open valve at dispatch and never stages together.
	deepenLane(t, db, laneID, "GPSER", 3)
	slots := laneSlotsByDepth(t, db, laneID) // S0 shallow … S2 deepest
	lane, err := db.GetNode(laneID)
	if err != nil {
		t.Fatalf("get lane: %v", err)
	}

	press := lineNode(t, db, "GPSER-PRESS")
	pressOrigin(t, db, press, "GPSER-PROC", "GPSER-STYLE")

	// The blocker holds the deepest slot, dispatched and not yet placed. It takes
	// an inbound mouth row and NO occupancy row — it is the Tier-2 wall the pair
	// stages behind, not a robot inside the corridor.
	blocker := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = slots[2].Name
		o.Status = "in_transit"
	})
	if adm, _, _, aErr := d.AcquireLanesForOrder(blocker, press, slots[2], EntryFreshBin); aErr != nil || !adm {
		t.Fatalf("blocker mouth row: adm=%v err=%v", adm, aErr)
	}
	if err := db.UpdateOrderVendor(blocker.ID, "sg-gpser-blocker", "RUNNING", ""); err != nil {
		t.Fatalf("blocker vendor: %v", err)
	}

	// The pair, dispatched exactly as the scanner dispatches: admit, then create.
	// Both take their inbound mouth rows (same mode shares), both stage behind the
	// blocker, neither is inside the lane.
	pair := func(uuid string, slot *nodes.Node) *orders.Order {
		o := testdb.CreateOrder(t, db, func(ord *orders.Order) {
			ord.EdgeUUID = uuid
			ord.DeliveryNode = slot.Name
			ord.ProcessNode = press.Name
			ord.SourceNode = press.Name
			ord.Status = "sourcing"
		})
		if adm, cause, _, aErr := d.AcquireLanesForOrder(o, press, slot, EntryFreshBin); aErr != nil || !adm {
			t.Fatalf("%s refused pre-dispatch (%q, %v) — nothing is inside this lane yet", uuid, cause, aErr)
		}
		if _, dErr := d.DispatchDirect(o, press, slot); dErr != nil {
			t.Fatalf("DispatchDirect %s: %v", uuid, dErr)
		}
		reloaded, gErr := db.GetOrder(o.ID)
		if gErr != nil {
			t.Fatalf("reload %s: %v", uuid, gErr)
		}
		if !IsGateStaged(reloaded) {
			t.Fatalf("%s must be gate-staged behind the blocker (wait=%d vendor=%q)",
				uuid, reloaded.WaitIndex, reloaded.VendorOrderID)
		}
		markStaged(t, db, reloaded.ID)
		return reloaded
	}
	deepPartner := pair("gpser-deep", slots[1])
	shallowPartner := pair("gpser-shallow", slots[0])
	assertSameOrigin(t, d, deepPartner, shallowPartner)

	if n := len(backend.ReleaseCalls()); n != 0 {
		t.Fatalf("appends while the blocker still holds the lane = %d, want 0", n)
	}

	// The blocker places. One evaluator pass, and the pair is co-released by the
	// classifier — so whatever limits the entries to one is not the tiers.
	d.ReleaseInboundLaneForOrder(blocker.ID, slots[2].Name)
	d.EvaluateLaneReleases(laneID)

	if n := len(backend.ReleaseCalls()); n != 1 {
		t.Fatalf("appends in the pass that opened the lane = %d, want exactly 1. A gated lane is "+
			"single file: Tier 1 dispatches the pair together, it does not put two robots in one "+
			"corridor", n)
	}
	deepAfter, _ := db.GetOrder(deepPartner.ID)
	if IsGateStaged(deepAfter) {
		t.Error("the deepest admissible partner was not released when the lane opened")
	}
	shallowAfter, _ := db.GetOrder(shallowPartner.ID)
	if !IsGateStaged(shallowAfter) {
		t.Fatal("the second partner entered a corridor its partner is already inside")
	}

	// THE CAUSE IS THE ASSERTION. lane-occupied means the tiers admitted (Tier 1
	// co-released the pair) and the physical single-file question held it; a tier
	// cause here would mean the pair was depth-gated against itself, which is the
	// latency at the press the ruling refuses.
	v, err := d.gateEntryVerdict(lane, shallowAfter, slots[0], false)
	if err != nil {
		t.Fatalf("verdict for the held partner: %v", err)
	}
	if v.Admitted() || v.Cause() != CauseLaneOccupied {
		t.Fatalf("held partner: admitted=%v cause=%q, want refused with %q", v.Admitted(), v.Cause(), CauseLaneOccupied)
	}

	// Its releaser is named and ordinary: the partner places, its occupancy row
	// goes (wiring_block_completed.go → ReleaseLaneOccupancy), the evaluator
	// re-fires on that same event, and the second robot enters.
	d.ReleaseLaneOccupancy(deepAfter.ID)
	d.EvaluateLaneReleases(laneID)
	if n := len(backend.ReleaseCalls()); n != 2 {
		t.Fatalf("appends after the first partner placed = %d, want 2 — the gate wait has a "+
			"releaser or it is a wedge", n)
	}
	shallowFinal, _ := db.GetOrder(shallowPartner.ID)
	if IsGateStaged(shallowFinal) {
		t.Error("the second partner never entered, though the lane cleared")
	}
}

// TestGatePair_NeitherPartnerIsHeldBeforeTheFleetCreate is the ruling's second
// half, and it is the half that decides whether the deletion above is worth
// anything: *"What I didn't want was to hold up the swap at the press due to the
// lane."*
//
// A same-origin pair into a CLEAR gated lane. The first partner's tail appends
// immediately (the open valve), which takes its occupancy row — and the second
// partner must still be dispatched: its robot leaves for the press, picks, and
// dwells at the gate point OUTSIDE the corridor. The lane wait belongs on a
// robot at a gate, never on the press.
//
// The pre-dispatch admission is the thing under test. It is asked at the wrong
// moment for a gated lane if it refuses here: a gated create sends the robot to
// the GATE POINT, not into the lane — which is precisely why appendGateTail
// takes occupancy at the append rather than at the create.
func TestGatePair_NeitherPartnerIsHeldBeforeTheFleetCreate(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	laneID, s0, s1 := gateChoreoLane(t, db, "GPPRESS", "GPPRESS-WAIT")
	press := lineNode(t, db, "GPPRESS-PRESS")
	pressOrigin(t, db, press, "GPPRESS-PROC", "GPPRESS-STYLE")

	mk := func(uuid string, slot *nodes.Node) *orders.Order {
		return testdb.CreateOrder(t, db, func(ord *orders.Order) {
			ord.EdgeUUID = uuid
			ord.DeliveryNode = slot.Name
			ord.ProcessNode = press.Name
			ord.SourceNode = press.Name
			ord.Status = "sourcing"
		})
	}
	// Deepest first, which is the order the press pair is dispatched in anyway.
	first, second := mk("gppress-1", s1), mk("gppress-2", s0)
	assertSameOrigin(t, d, first, second)

	// Partner 1 goes the whole way: admitted, created, and — the lane being clear
	// — its tail appends in the same call, so it now holds the lane.
	if adm, cause, _, err := d.AcquireLanesForOrder(first, press, s1, EntryFreshBin); err != nil || !adm {
		t.Fatalf("first partner refused on an empty lane (%q, %v)", cause, err)
	}
	if _, err := d.DispatchDirect(first, press, s1); err != nil {
		t.Fatalf("DispatchDirect first: %v", err)
	}
	if occ := occupantsOf(t, d, laneID); !containsID(occ, first.ID) {
		t.Fatalf("fixture: the first partner should be INSIDE the lane after its tail appended (occupants %v)", occ)
	}

	// Partner 2 is the assertion. Its lane is busy — and that is a gate question,
	// not a dispatch question.
	admitted, cause, laneName, err := d.AcquireLanesForOrder(second, press, s0, EntryFreshBin)
	if err != nil {
		t.Fatalf("AcquireLanesForOrder second: %v", err)
	}
	if !admitted {
		t.Fatalf("the second partner of a same-origin pair was held BEFORE the fleet create "+
			"(cause %q at %q). That puts the lane's wait back on the PRESS, which is the outcome "+
			"the single-file rule exists to avoid. A gated dispatch sends the robot to the GATE "+
			"POINT, outside the corridor — "+
			"the same reason appendGateTail takes occupancy at the append and not at the create. "+
			"Refusing here puts the lane's wait back on the press", cause, laneName)
	}
	if _, err := d.DispatchDirect(second, press, s0); err != nil {
		t.Fatalf("DispatchDirect second: %v", err)
	}

	// Both robots are en route: two creates, two vendor ids.
	if n := len(backend.CreateRequests()); n != 2 {
		t.Fatalf("fleet creates = %d, want 2 — the pair dispatches together", n)
	}
	reloadedSecond, _ := db.GetOrder(second.ID)
	if reloadedSecond.VendorOrderID == "" {
		t.Fatal("the second partner has no fleet job — no robot went to the press for it")
	}

	// And the corridor is still single file: only the first partner's tail landed.
	if n := len(backend.ReleaseCalls()); n != 1 {
		t.Fatalf("appends = %d, want 1 — the second partner dwells at the gate, it does not enter", n)
	}
	if !IsGateStaged(reloadedSecond) {
		t.Fatal("the second partner entered a lane its partner is inside")
	}
}
