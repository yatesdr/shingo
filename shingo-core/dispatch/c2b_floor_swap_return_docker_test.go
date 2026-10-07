//go:build docker

package dispatch

import (
	"encoding/json"
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// c2b_floor_swap_return_docker_test.go — C2b's first pin. It was committed red
// and is green since F1 (laneWaitAfterAppend): the station's append is the lane
// wait's own event.
//
// Source: unification-C2a-2026-10-06/STUDY-floor-and-gate.md §1(b) and §2;
// RULINGS-C1-pair-and-C2a-go-2026-10-05.md §Amendment, ruling 2.

// c2bSwap is the 233 shape, parked at its station wait: lane D (the return,
// free), lane M (the pickup, walled), the plan as the splice leaves it.
type c2bSwap struct {
	db      *store.DB
	d       *Dispatcher
	backend *testdb.MockBackend
	swap    *orders.Order
	laneD   int64
	dSlots  []*nodes.Node
	press2  *nodes.Node
	dDrop   *nodes.Node // the slot the plan's lane drop names
}

func newC2bSwap(t *testing.T) c2bSwap {
	t.Helper()
	return newC2bSwapDroppingAt(t, nil)
}

// newC2bSwapDroppingAt is newC2bSwap with the slot the plan's lane drop names
// chosen by drop (nil: the first slot the lane lists).
func newC2bSwapDroppingAt(t *testing.T, drop func(db *store.DB, laneD int64, slots []*nodes.Node) *nodes.Node) c2bSwap {
	t.Helper()
	// Its own database: the floor sweeps every waiter it can find, and a shared
	// one would count other tests' robots.
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	// Lane D: the return lane, free. Lane M: the pickup lane, walled.
	_, laneD, _ := gatedLane(t, db, "C2BF-D", "C2BF-D-WAIT")
	_, laneM, mMouth := gatedLane(t, db, "C2BF-M", "C2BF-M-WAIT")
	mSlots := laneSlots(t, db, laneM)
	var mDeep = mSlots[0]
	for _, s := range mSlots {
		if s.ID != mMouth.ID {
			mDeep = s
		}
	}
	dSlots := laneSlots(t, db, laneD)
	press := lineNode(t, db, "C2BF-PRESS1")
	press2 := lineNode(t, db, "C2BF-PRESS2")
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, mMouth.ID, "C2BF-WALL")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, mDeep.ID, "C2BF-EMPTY")
	dDrop := dSlots[0]
	if drop != nil {
		dDrop = drop(db, laneD, dSlots)
	}

	plan := []resolvedStep{
		{Action: protocol.ActionWait, Node: press.Name, WaitKind: WaitKindStation},
		{Action: protocol.ActionPickup, Node: press.Name},
		{Action: protocol.ActionWait, Node: "C2BF-D-WAIT", WaitKind: WaitKindLane, WaitLane: laneD},
		{Action: protocol.ActionDropoff, Node: dDrop.Name},
		{Action: protocol.ActionWait, Node: "C2BF-M-WAIT", WaitKind: WaitKindLane, WaitLane: laneM},
		{Action: protocol.ActionPickup, Node: mDeep.Name, Empty: true},
		{Action: protocol.ActionDropoff, Node: press2.Name},
	}
	planJSON, err := json.Marshal(plan)
	testutil.MustNoErr(t, err, "marshal the swap's plan")

	swap := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "c2bf-swap"
		o.StationID = "line-1"
		o.OrderType = OrderTypeComplex
		o.Coordinated = true
		o.PayloadCode = sd.Payload.Code
		o.SourceNode = press.Name
		o.DeliveryNode = press2.Name
		o.Status = protocol.StatusStaged
	})
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(swap.ID, string(planJSON)), "persist the plan")
	testutil.MustNoErr(t, db.UpdateOrderVendor(swap.ID, "C2BF-V1", "WAITING", "AMR-1"), "set the vendor order")

	return c2bSwap{db: db, d: d, backend: backend, swap: swap, laneD: laneD, dSlots: dSlots, press2: press2, dDrop: dDrop}
}

// TestC2bPin_ASwapsLaneReturnIsReleasedByItsOwnEventNotTheFloor.
//
// The reproduction of order 233 (s1-after-660fd228-20m, 2026-10-05 23:40:47-48):
// a PANEL swap whose plan, as the splice leaves it, is
//
//	wait(station) · pickup@press · wait(lane D) · dropoff@D · wait(lane M) · pickup@M (empty, walled) · dropoff@press2
//
// The station releases its wait (HandleOrderRelease → dispatchFleetRelease →
// appendSegmentAndAdvance, "complex release"). That append advances wait_index
// onto lane D's wait and RETURNS: nothing evaluates lane D. The event was on a
// station node; the waiter is on lane D, whose evaluator nobody calls. The row
// carries no cause, because nothing refused it — nothing asked. It is released
// by whichever comes first: an unrelated event on D, the robot reaching D's mark
// (EvaluateWaitLaneForStagedOrder), or the periodic floor (SweepLaneWaiters),
// which then writes a lane_floor_release row under "(none)".
//
// Measured: 141-166 of the 131-170 floor releases per 20-minute packed run, in
// all five runs on record (p1/p2/p3, s1-before, s1-after), are exactly this.
// Base code: dispatchFleetRelease and the floor predate C1 (647b65af..8fc5562e
// does not touch complex_release.go or lane_gate_release.go).
//
// Lane M is walled (a bin at its mouth in front of the empty the swap fetches)
// to keep the shape whole; the pin asserts nothing about M.
//
// GREEN (C2b): the station release leaves the order released into lane D on its
// own event — its wait_index is past lane D's wait and D's tail is appended —
// and a floor pass frees nobody and records nothing. Then assert:
//
//	after the station release: wait_index > 1 and one tail append on D
//	SweepLaneWaiters() == 0, and no lane_floor_release row for the order
//
// A NAMED TEST PINS TODAY'S BEHAVIOUR AND WILL MOVE WITH THE FIX:
// TestWaitKind_FenceHoldsBothWaysOnOneCoordinatedPlan asserts exactly ONE append
// after a station release onto a FREE lane wait (lane_gate_wait_kind_docker_test.go,
// "append calls after the station released its OWN wait"). It is the same
// missing evaluation, asserted as correct.
func TestC2bPin_ASwapsLaneReturnIsReleasedByItsOwnEventNotTheFloor(t *testing.T) {
	t.Parallel()
	f := newC2bSwap(t)
	db, d, backend, swap, laneD := f.db, f.d, f.backend, f.swap, f.laneD
	atStation, err := db.GetOrder(swap.ID)
	testutil.MustNoErr(t, err, "reload the swap")
	if IsGateStaged(atStation) {
		t.Fatal("fixture: the swap is parked at its STATION wait and must not read as gate-staged")
	}

	// ── THE EVENT: the station releases its own wait. ─────────────────────
	d.HandleOrderRelease(d.syntheticEnvelope(atStation.StationID),
		&protocol.OrderRelease{OrderUUID: atStation.EdgeUUID})

	afterEvent, err := db.GetOrder(swap.ID)
	testutil.MustNoErr(t, err, "reload after the station release")
	if afterEvent.WaitIndex < 1 {
		t.Fatalf("fixture: the station release did not consume its own wait (wait_index %d) — the "+
			"pin no longer reaches the lane wait", afterEvent.WaitIndex)
	}
	var red []string
	if afterEvent.WaitIndex == 1 && IsGateStaged(afterEvent) {
		red = append(red, fmt.Sprintf("the station release advanced order %d onto lane %d's wait and "+
			"nothing evaluated lane %d: wait_index 1, cause %q, %d append(s) — a free lane, no refusal, "+
			"no release", swap.ID, laneD, laneD, afterEvent.QueueCause, appendsTo(backend, "C2BF-V1")))
	}

	// ── THE FLOOR: one pass of what the 60 s loop does. ───────────────────
	freed := d.SweepLaneWaiters()
	recs, rErr := db.ListRecoveryActions(50)
	testutil.MustNoErr(t, rErr, "list recovery actions")
	for _, r := range recs {
		if r.Action == floorReleaseAction && r.TargetID == swap.ID {
			red = append(red, fmt.Sprintf("the floor freed %d order(s); order %d's record: %s",
				freed, swap.ID, r.Detail))
		}
	}
	if freed > 0 && len(red) == 0 {
		t.Fatalf("the floor freed %d order(s) but none is the swap — the fixture has drifted", freed)
	}

	// THE SHOULD-BE-ZERO, both halves: the floor released order 233 in the run
	// under "(none)"; the station's release is now the lane wait's event.
	if len(red) != 0 {
		t.Fatalf("%v", red)
	}
	if after, gErr := db.GetOrder(swap.ID); gErr != nil || after.WaitIndex < 2 {
		t.Fatalf("the swap is not released into lane %d on its own event (wait_index %v, err %v)",
			laneD, after.WaitIndex, gErr)
	}
}
