//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// TestLaneEntryTiers_ParksDeeperPending is the end-to-end ON test: a shallow store parks behind a deeper active
// cross-origin store in the same lane — against REAL DB rows, not a fake. It runs
// twice: once with a BARE delivery_node and once with a DOT-qualified one
// ("LANE.SLOT"), because a dotted row invisible to the active-set query would make
// the gate silently admit (the F1 fail-open). Both must park.
func TestLaneEntryTiers_ParksDeeperPending(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	check := func(prefix string, deliveryFor func(lane, slot string) string) {
		_, laneID, s0 := gatedLane(t, db, prefix, prefix+"-WAIT") // S0 depth0, S1 depth1, gated by its mark
		laneNode, err := db.GetNode(laneID)
		if err != nil {
			t.Fatalf("[%s] get lane: %v", prefix, err)
		}
		slots, err := db.ListLaneSlots(laneID)
		if err != nil {
			t.Fatalf("[%s] list slots: %v", prefix, err)
		}
		var s1 *nodes.Node
		for _, s := range slots {
			if dpt, _ := db.GetSlotDepth(s.ID); dpt == 1 {
				s1 = s
			}
		}
		if s1 == nil {
			t.Fatalf("[%s] fixture should have a depth-1 slot", prefix)
		}

		// A shallow store queued for S0, and a deeper store active in S1 (its
		// delivery_node written in the format under test). Different origins: both
		// resolve to no style claim → cross-origin (Tier 2).
		shallow := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.DeliveryNode = s0.Name
			o.Status = "queued"
		})
		_ = testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.DeliveryNode = deliveryFor(laneNode.Name, s1.Name)
			o.Status = "in_transit"
		})

		v, err := d.laneEntryCause(laneNode, shallow, s0)
		if err != nil || v.Admitted() || v.Cause() != CauseLaneDeeperPending {
			t.Fatalf("[%s] deep active: admitted=%v cause=%q err=%v, want refused with %q", prefix, v.Admitted(), v.Cause(), err, CauseLaneDeeperPending)
		}
	}

	check("TEBARE", func(lane, slot string) string { return slot })             // bare delivery_node
	check("TEDOT", func(lane, slot string) string { return lane + "." + slot }) // dot-qualified
}

// TestLaneEntryTiers_ReleasesOnPlacement is the A′ scenario: a shallow store parks
// behind a deeper one only until the deeper store PLACES its bin — not until the
// deeper ORDER completes.
//
// Placement is observed exactly as production observes it ON AN UNMARKED LANE:
// the deeper store holds an inbound mouth row from its fleet commit
// (AcquireLanesForOrder) until its dropoff block reports FINISHED, at which point
// ReleaseInboundLaneForOrder deletes the row (the §4 early handoff, fired from
// handleStoreBlockCompleted BEFORE the delivery-node early-return). The deeper
// order stays `in_transit` throughout, so the only thing that changes between the
// two AdmitLaneEntry calls is the mouth row — which is precisely the claim under
// test.
//
// UNMARKED, and it used to be marked. A gated lane defers its destination hold to
// the mark (resolveOrderLaneHolds), so on one of those there is no row for this
// test to watch appear and disappear — placement is DERIVED there instead
// (stillComingToLane), and the F-11 fixtures are what exercise that arm. The
// tiers this test is about are identical on both, so it moved to the lane whose
// mechanism it actually describes rather than having its assertions rewritten
// into a mechanism it does not.
//
// The third leg pins the half A′ deliberately does NOT change: a deeper store that
// has not been dispatched yet (no vendor_order_id, hence no mouth row it could have
// released) still blocks. Dropping those would not be placement-release; it would
// silently stop a queued deeper store from holding its place.
func TestLaneEntryTiers_ReleasesOnPlacement(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, laneID, s0 := gatedLane(t, db, "TEPLACE", "")
	laneNode, err := db.GetNode(laneID)
	if err != nil {
		t.Fatalf("get lane: %v", err)
	}
	line := lineNode(t, db, "TEPLACE-LINE")
	slots, err := db.ListLaneSlots(laneID)
	if err != nil {
		t.Fatalf("list slots: %v", err)
	}
	var s1 *nodes.Node
	for _, s := range slots {
		if dpt, _ := db.GetSlotDepth(s.ID); dpt == 1 {
			s1 = s
		}
	}
	if s1 == nil {
		t.Fatal("fixture should have a depth-1 slot")
	}

	shallow := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = s0.Name
		o.Status = "queued"
	})
	deep := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = s1.Name
		o.Status = "in_transit"
	})

	// The deeper store commits to the fleet: it takes its inbound mouth row and
	// gets a vendor order id, exactly as the scanner does before DispatchDirect.
	if adm, _, _, aErr := d.AcquireLanesForOrder(deep, line, s1, EntryFreshBin); aErr != nil || !adm {
		t.Fatalf("deep store must be admitted on a free lane: adm=%v err=%v", adm, aErr)
	}
	if err := db.UpdateOrderVendor(deep.ID, "sg-teplace-deep", "RUNNING", ""); err != nil {
		t.Fatalf("update deep vendor: %v", err)
	}
	if n := gateMouthRows(t, db, laneID); n != 1 {
		t.Fatalf("deep store inbound row = %d, want 1", n)
	}

	// In flight, not yet placed → the shallow store parks (unchanged behavior).
	v, err := d.laneEntryCause(laneNode, shallow, s0)
	if err != nil || v.Admitted() || v.Cause() != CauseLaneDeeperPending {
		t.Fatalf("before placement: admitted=%v cause=%q err=%v, want refused with %q", v.Admitted(), v.Cause(), err, CauseLaneDeeperPending)
	}

	// The deeper store's dropoff block completes — the bin is DOWN. Its order is
	// still in_transit (not completed, not terminal).
	placeDeeperBlocker(t, db, d, deep.ID, s1.Name)
	if n := gateMouthRows(t, db, laneID); n != 0 {
		t.Fatalf("inbound row after placement = %d, want 0", n)
	}
	still, err := db.GetOrder(deep.ID)
	if err != nil {
		t.Fatalf("reload deep order: %v", err)
	}
	if protocol.IsTerminal(still.Status) {
		t.Fatalf("deep order went terminal (%s) — the test would prove nothing", still.Status)
	}

	// A′: the shallow store admits NOW, on placement, without waiting for the
	// deeper order to complete.
	v, err = d.laneEntryCause(laneNode, shallow, s0)
	if err != nil || !v.Admitted() {
		t.Fatalf("after placement: admitted=%v cause=%q err=%v, want admit", v.Admitted(), v.Cause(), err)
	}

	// A deeper store that never reached the fleet still blocks: it holds no mouth
	// row because it has not dispatched, not because it has placed.
	_ = testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = s1.Name
		o.Status = "queued"
	})
	v, err = d.laneEntryCause(laneNode, shallow, s0)
	if err != nil || v.Admitted() || v.Cause() != CauseLaneDeeperPending {
		t.Fatalf("undispatched deeper store: admitted=%v cause=%q err=%v, want refused with %q", v.Admitted(), v.Cause(), err, CauseLaneDeeperPending)
	}
}

// TestLaneEntryTiers_OffIsAdmit confirms an UNMARKED lane is not sequenced at all.
//
// The assertion moved with the ruling. It used to say "a non-mouth group admits
// regardless of a deeper active store", checked through a wrapper that consulted
// the enforcement mode. There is no mode: a lane without a mark is not gated, the
// evaluator never runs for it, and the classifier is therefore never consulted —
// so what is worth pinning is the DERIVATION, that an unmarked lane reports
// ungated and a marked one does not.
func TestLaneEntryTiers_OffIsAdmit(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, unmarked, _ := gatedLane(t, db, "TEOFF", "")       // no mark
	_, marked, _ := gatedLane(t, db, "TEON", "TEON-WAIT") // a mark

	if d.laneIsGated(unmarked) {
		t.Error("a lane with no mark reports gated — every lane at every plant is unmarked today, " +
			"so this would turn the gate on everywhere at deploy")
	}
	if !d.laneIsGated(marked) {
		t.Error("a lane with a mark reports ungated — placing the mark IS the enablement, and " +
			"nothing else turns it on")
	}
	if got := d.laneWaitPoint(marked); got != "TEON-WAIT" {
		t.Errorf("wait point = %q, want the mark that was set — the value goes to the fleet verbatim "+
			"as the block location a robot dwells at", got)
	}
	if got := d.laneWaitPoint(unmarked); got != "" {
		t.Errorf("unmarked lane reports wait point %q", got)
	}
}

// TestLaneEntry_DwellingCandidateStillBlocks is the latent defect the release
// pass's own header named, made visible and then closed.
//
// ── THE DEFECT ────────────────────────────────────────────────────────────
//
// A gate-staged order is STANDING AT A MARK with its bin still on the robot's
// deck. It has not placed. It is as much of a blocker as a robot driving down
// the aisle — more, in one sense, because it has not even started.
//
// The old witness could not see that. It asked "dispatched, and holding no
// inbound mouth row?" and called the answer "it has placed". A dwelling COMPLEX
// candidate satisfied both halves — resolvePlanLaneHolds has always deferred a
// gated lane's hold — so it was filtered OUT of the blocker set while it was
// still very much coming, and a shallower entrant walked past it. The release
// pass header recorded this in as many words and told whoever removed the plain
// arm to move the signal first.
//
// It stayed latent because nothing dwelled: no plant sets a mark, so the gated
// arm never ran anywhere. Group wait points make dwelling the ordinary case,
// which is what turns a latent defect into a live one.
//
// ── WHY IT IS THIS SHAPE AND NOT A UNIT TEST ──────────────────────────────
//
// The claim is about what the CLASSIFIER sees, so the dweller has to be a real
// one: dispatched through the valve, refused entry, parked at its mark with a
// lane wait in its own plan. A hand-built row would let the fixture assert
// something the plant cannot produce.
//
// RED WITHOUT THE GATE-STAGED ARM (verified by deleting the
// `IsGateStaged(o) && gateWaitLane(o) == laneID` case from stillComingToLane):
// the shallow store is ADMITTED with no cause, i.e. sent into a lane a robot is
// about to enter ahead of it, at a depth that seals the dweller's slot in.
func TestLaneEntry_DwellingCandidateStillBlocks(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	wall, _, w, _, _ := clearLaneFixture(t, db, "DWBLK")
	line := lineNode(t, db, "DWBLK-LINE")

	// A REAL DWELLER at the deep slot: parked behind a deeper-still blocker, then
	// left standing at the mark when that blocker places. Its bin is on the deck
	// and its lane wait is un-appended.
	deeper := stageDeeperBlocker(t, db, d, line, w[2], "dwblk-deeper")
	dweller := stageGatedStore(t, db, d, line, w[1], nil)
	if !IsGateStaged(dweller) {
		t.Fatalf("fixture: the dweller must be gate-staged (wait_index=%d vendor=%q)",
			dweller.WaitIndex, dweller.VendorOrderID)
	}
	markStaged(t, db, dweller.ID)
	placeDeeperBlocker(t, db, d, deeper.ID, w[2].Name)

	// It is dispatched, it holds no inbound mouth row, and it is not in the
	// corridor — the exact three facts the OLD witness read as "it has placed".
	reloaded, err := db.GetOrder(dweller.ID)
	testutil.MustNoErr(t, err, "reload dweller")
	if reloaded.VendorOrderID == "" {
		t.Fatal("fixture: the dweller must be dispatched, or the not-dispatched arm carries this " +
			"test and it proves nothing about dwelling")
	}
	rows, err := reservations.ActiveMouthRows(db.DB, wall.ID)
	testutil.MustNoErr(t, err, "read mouth rows")
	for _, r := range rows {
		if r.OrderID == dweller.ID && r.Mode == reservations.ModeInbound {
			t.Fatal("fixture: the dweller must hold NO inbound mouth row — if it does, the row arm " +
				"carries this test and the gate-staged arm is untested")
		}
	}
	occupants, err := reservations.OccupantsOf(db.DB, wall.ID)
	testutil.MustNoErr(t, err, "read occupancy")
	for _, id := range occupants {
		if id == dweller.ID {
			t.Fatal("fixture: the dweller must NOT hold lane occupancy — it is outside the corridor, " +
				"which is what dwelling means")
		}
	}

	// THE CLAIM: a SHALLOWER store is refused, deepest-first, because the dweller
	// is still coming.
	shallow := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = w[0].Name
		o.Status = "queued"
	})
	laneNode, err := db.GetNode(wall.ID)
	testutil.MustNoErr(t, err, "reload lane")

	v, err := d.laneEntryCause(laneNode, shallow, w[0])
	testutil.MustNoErr(t, err, "classify the shallow entrant")
	if v.Admitted() {
		t.Fatalf("the shallow store was ADMITTED while a robot dwells at the mark holding a bin for "+
			"%s — it would drive in ahead of the dweller and seal its slot in. That is the latent "+
			"defect the release pass header named", w[1].Name)
	}
	if v.Cause() != CauseLaneDeeperPending {
		t.Errorf("cause = %q, want %q — a dweller is a deeper store that has not placed, and the "+
			"row an engineer reads has to say so", v.Cause(), CauseLaneDeeperPending)
	}
}
