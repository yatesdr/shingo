//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// c2a_mouth_granted_dig_docker_test.go — A.2's mouth clause. Red at ebb4dae0
// (Lane F); green since the plan-time verdict refuses a dig on a lane whose
// mouth is granted (plan.DigFits, DugMouthGrantedError).
//
// Source: unification-C2a-2026-10-06/STUDY-floor-and-gate.md §1(c)/(d);
// RULINGS-C1-pair-and-C2a-go-2026-10-05.md §Amendment, ruling 1.

// TestC2aPin_ADigIsNotAdmittedOnALaneWhoseMouthIsGranted — GREEN since C2a A.2.
//
// The state dig 242 was admitted into (s1-after-660fd228-20m, 23:40:50.777):
// order 233 had been released into LS_D1 at 23:40:48.020 — re-bound onto
// LSD_001 (nodes.claimed_by = 233, its slot reservation confirmed), its tail
// appended — and had set bin 10 down at LSD_001 ~23:40:50.6. Its occupancy row
// (Hold B) went with the drop (handleStoreBlockCompleted → ReleaseLaneOccupancy),
// but the bin was NOT recorded there: the re-bind had written LSD_001 into
// orders.delivery_node, so the drop read as the order's final one and was left
// to whole-order FINISHED (23:40:57.755). In the rows Core reads, the mouth was
// claimed, empty and unoccupied. proposeLaneClearDigThen asked the mouth rows
// (CanTakeFor: a complex order takes none), the occupancy rows (OccupantsOf:
// gone) and the plan (PlanLaneMouthClear → findBuriedBlockers: bins only), and
// admitted a dig whose legs lift from behind a bin about to be recorded in front
// of them — the GUARD BYPASS of 23:40:57.
//
// Fixture, the same rows: a dug lane [S1 granted to a live in-transit store,
// empty; S2 blocker; S3 target], a three-slot sibling lane for parking, no
// occupancy row. A complex demand buried at S3 asks for its dig at intake, as
// 242 did (planBuriedReshuffleAtIntake → handleComplexBurial →
// proposeLaneClearDigThen).
//
// It was red: the dig started (the demand `reshuffling`, one leg lifting S2).
//
// Now (A.2's mouth clause): the dig is refused at plan time and the demand
// parks under CauseLaneLocked — ErrLaneMouthHeld → classifyPlanError →
// laneClearLaneBusy → handleComplexBurial's laneClearLaneBusy arm, whose
// releaser row (queue_releasers.go, CauseLaneLocked) already names "the demand
// holding its mouth releasing it", DigOrderID 0 being the tell that no
// excavation is involved. The store then lands (bin recorded at S1, claim
// gone); the demand's re-ask (the scanner's entry, handleComplexBuriedOnReplay)
// starts the dig, and it now lifts S1 first.
func TestC2aPin_ADigIsNotAdmittedOnALaneWhoseMouthIsGranted(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, _, _, dugSlots, _, bp := setupDwellGroup(t, db, "C2AM", 3, false)
	mouth, wall, deep := dugSlots[0], dugSlots[1], dugSlots[2]

	createTestBinAtNode(t, db, bp.Code, wall.ID, "C2AM-BLK")
	target := createTestBinAtNode(t, db, bp.Code, deep.ID, "C2AM-TGT")

	// ── THE GRANT: a store released into the lane's mouth, not yet landed. ──
	// The two writers the gate's release uses (rebindGatedDropoff): the slot
	// claim and its confirmation. No occupancy row: its robot has placed and
	// driven on, and Hold B went with the drop.
	storeOrder := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "c2am-store"
		o.StationID = "line-1"
		o.OrderType = OrderTypeComplex
		o.Coordinated = true
		o.PayloadCode = bp.Code
		o.Status = protocol.StatusInTransit
		o.DeliveryNode = mouth.Name
	})
	testutil.MustNoErr(t, db.UpdateOrderVendor(storeOrder.ID, "C2AM-V1", "RUNNING", "AMR-1"), "vendor order")
	testutil.MustNoErr(t, claimStoreSlot(db, storeOrder, mouth), "the store's slot claim on the mouth")
	granted, err := db.GetNode(mouth.ID)
	testutil.MustNoErr(t, err, "reload the mouth")
	testutil.MustNoErr(t, d.confirmDropoffSlot(storeOrder, granted), "confirm the store's slot claim")

	// ── THE ASK: a buried complex demand wants its dig, as 242 did. ──
	demand := mkQueuedComplexParent(t, db, "c2am-demand", bp.Code)
	buried := &BuriedError{Bin: target, Slot: deep, LaneID: dug.ID}
	d.planBuriedReshuffleAtIntake(demand, bp.Code, "line-1", buried)

	after, err := db.GetOrder(demand.ID)
	testutil.MustNoErr(t, err, "read the demand back")
	if protocol.IsTerminal(after.Status) {
		t.Fatalf("the demand is %q (%q): the fixture no longer reaches the dig", after.Status, after.ErrorDetail)
	}
	legs := legsOf(t, db, demand.ID)

	switch {
	case len(legs) > 0:
		lifts := make([]string, 0, len(legs))
		for _, l := range legs {
			lifts = append(lifts, l.SourceNode)
		}
		t.Fatalf("dig for demand %d started on %s with its mouth %s granted to order %d "+
			"(claimed, not landed): %d leg(s) lifting %v — the store's bin lands in front of them",
			demand.ID, dug.Name, mouth.Name, storeOrder.ID, len(legs), lifts)
	case QueueCause(after.QueueCause) != CauseLaneLocked:
		t.Fatalf("the dig was refused but the demand parked under %q, want %q", after.QueueCause, CauseLaneLocked)
	default:
		// Refused under the named cause. The store lands...
		testutil.MustNoErr(t, db.ReleaseSlotClaim(mouth.ID, storeOrder.ID), "the landing drops the slot claim")
		createTestBinAtNode(t, db, bp.Code, mouth.ID, "C2AM-LANDED")
		if _, tErr := db.TerminalizeOrder(storeOrder.ID, protocol.StatusConfirmed, "landed"); tErr != nil {
			t.Fatalf("terminalize the store: %v", tErr)
		}
		// ...and the dig starts after, lifting the landed bin first.
		parked, gErr := db.GetOrder(demand.ID)
		testutil.MustNoErr(t, gErr, "reload the parked demand")
		d.handleComplexBuriedOnReplay(parked, buried)
		legs = legsOf(t, db, demand.ID)
		if len(legs) != 2 || legs[0].SourceNode != mouth.Name {
			t.Fatalf("after the store landed, the dig for demand %d has %d leg(s) (want 2, the first lifting "+
				"%s): the clause must release on the landing", demand.ID, len(legs), mouth.Name)
		}
	}
}

// TestC2aPin_ARequesterWalledFromItsOwnSlotGetsItsDig — the mouth clause's
// other side, found by C2a's S.2 pair on the houseserver (bc2fee27): order 147,
// a complex store standing at LS_C1's mark, its slot LSC_002 claimed and empty
// BEHIND a bin at LSC_001, summoned the dig that would open it, and the clause
// refused that dig as "mouth granted" because the claim was 147's own. A
// lane-clear dig plans as nobody in particular (proposeLaneClearDig passes
// reservations.Anyone), so the asker exemption could not spare it: 580 refusals
// under lane-locked, and the plant still from mid-run.
//
// A granted slot refuses a dig only when it lies IN FRONT of the deepest
// blocker the dig would lift (DigFitFacts' frontOf). Here it lies behind the
// only blocker, so the dig starts.
func TestC2aPin_ARequesterWalledFromItsOwnSlotGetsItsDig(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, _, _, dugSlots, _, bp := setupDwellGroup(t, db, "C2AW", 3, false)
	mouth, behind := dugSlots[0], dugSlots[1]
	createTestBinAtNode(t, db, bp.Code, mouth.ID, "C2AW-WALL")

	// The requester: a store at the lane's mark, its slot behind the wall claimed
	// and confirmed, as the gate's release left 147.
	requester := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "c2aw-store"
		o.StationID = "line-1"
		o.OrderType = OrderTypeComplex
		o.Coordinated = true
		o.PayloadCode = bp.Code
		o.Status = protocol.StatusStaged
		o.DeliveryNode = behind.Name
	})
	testutil.MustNoErr(t, db.UpdateOrderVendor(requester.ID, "C2AW-V1", "WAITING", "AMR-1"), "vendor order")
	testutil.MustNoErr(t, claimStoreSlot(db, requester, behind), "the requester's slot claim behind the wall")
	granted, err := db.GetNode(behind.ID)
	testutil.MustNoErr(t, err, "reload the slot")
	testutil.MustNoErr(t, d.confirmDropoffSlot(requester, granted), "confirm the requester's slot claim")

	res := d.proposeLaneClearDig(dug, behind, requester)
	if res.outcome != laneClearStarted {
		t.Fatalf("the requester's own dig of %s, to open its slot %s behind the wall at %s, answered %v (%v); "+
			"a slot claimed behind the blockers is the dig's purpose, not a granted mouth",
			dug.Name, behind.Name, mouth.Name, res.outcome, res.err)
	}
}
