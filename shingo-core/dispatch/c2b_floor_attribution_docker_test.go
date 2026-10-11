//go:build docker

package dispatch

import (
	"encoding/json"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// floorWaiterAtHeldLane is an order gate-staged at a lane wait whose lane a dig
// holds, so every evaluation refuses it until the dig lets go.
func floorWaiterAtHeldLane(t *testing.T, prefix string) (*Dispatcher, *store.DB, *orders.Order, int64, *orders.Order) {
	t.Helper()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, laneID, _ := gatedLane(t, db, prefix, prefix+"-WAIT")
	slots := laneSlots(t, db, laneID)
	line := lineNode(t, db, prefix+"-LINE")
	plan := []resolvedStep{
		{Action: protocol.ActionPickup, Node: line.Name},
		{Action: protocol.ActionWait, Node: prefix + "-GATE", WaitKind: WaitKindLane, WaitLane: laneID},
		{Action: protocol.ActionDropoff, Node: slots[1].Name},
	}
	planJSON, err := json.Marshal(plan)
	testutil.MustNoErr(t, err, "marshal the plan")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = prefix + "-waiter"
		o.StationID = "line-1"
		o.Coordinated = true
		o.SourceNode = line.Name
		o.DeliveryNode = slots[1].Name
		o.Status = protocol.StatusStaged
	})
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(order.ID, string(planJSON)), "persist the plan")
	testutil.MustNoErr(t, db.UpdateOrderVendor(order.ID, prefix+"-V1", "WAITING", "AMR-1"), "send it")
	digger := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = prefix + "-digger" })
	if !d.laneLock.TryLock(laneID, digger.ID) {
		t.Fatal("fixture: the dig could not take the lane")
	}
	fresh, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload the waiter")
	if !IsGateStaged(fresh) {
		t.Fatal("fixture: the waiter must read gate-staged at its lane wait")
	}
	return d, db, fresh, laneID, digger
}

func floorRecords(t *testing.T, db *store.DB, orderID int64) int {
	t.Helper()
	recs, err := db.ListRecoveryActions(50)
	testutil.MustNoErr(t, err, "list recovery actions")
	n := 0
	for _, r := range recs {
		if r.Action == floorReleaseAction && r.TargetID == orderID {
			n++
		}
	}
	return n
}

// TestC2bPin_TheFloorRecordsOnlyWhatItsOwnCallReleased is F6. The lane's dig
// lets go and an EVENT evaluates the lane while the floor's pass runs (the hook
// stands in for it, between the floor reading the waiting set and its own
// evaluation). The waiter is released by that event. The floor's own call
// releases nothing, so it records nothing: its "freed" used to be the
// before/after diff over the whole pass, which counted the event's release as
// its own. The second half pins the other side: a release the floor's own call
// makes is still recorded.
//
// MUTATION: count a waiter as the floor's whenever its state changed (drop the
// owned test in SweepLaneWaiters); the first half records the event's release.
func TestC2bPin_TheFloorRecordsOnlyWhatItsOwnCallReleased(t *testing.T) {
	t.Parallel()

	t.Run("an event's release mid-pass is not the floor's", func(t *testing.T) {
		t.Parallel()
		d, db, waiter, laneID, digger := floorWaiterAtHeldLane(t, "C2BF6E")
		d.floorPassHook = func() {
			d.laneLock.Unlock(laneID, digger.ID)
			d.EvaluateLaneReleases(laneID)
		}
		if freed := d.SweepLaneWaiters(); freed != 0 {
			t.Fatalf("the floor reported %d release(s); the event released the waiter, not the floor", freed)
		}
		if after, err := db.GetOrder(waiter.ID); err != nil || after.WaitIndex < 1 {
			t.Fatalf("fixture: the event did not release the waiter (wait_index %v, err %v)", after.WaitIndex, err)
		}
		if n := floorRecords(t, db, waiter.ID); n != 0 {
			t.Fatalf("the floor recorded %d lane_floor_release row(s) for a release an event made", n)
		}
	})

	t.Run("the floor's own release is still recorded", func(t *testing.T) {
		t.Parallel()
		d, db, waiter, laneID, digger := floorWaiterAtHeldLane(t, "C2BF6F")
		// CONVERTED (DECISIONS D27 R1): one pass while the dig holds the lane, so
		// the waiter is in the previous pass's snapshot; the floor records only a
		// waiter a pass already left waiting.
		if freed := d.SweepLaneWaiters(); freed != 0 {
			t.Fatalf("the held pass reported %d release(s); the dig still holds the lane", freed)
		}
		d.laneLock.Unlock(laneID, digger.ID) // freed with no evaluation: only the floor will ask
		if freed := d.SweepLaneWaiters(); freed != 1 {
			t.Fatalf("the floor reported %d release(s), want its own 1", freed)
		}
		if n := floorRecords(t, db, waiter.ID); n != 1 {
			t.Fatalf("the floor recorded %d row(s) for its own release, want 1", n)
		}
	})
}

// heldOneLegCompound is a compound whose one pending leg sources from a slot of
// a lane, held by a refusing fleet and swept once by the floor while held, so
// the leg was in a floor pass's snapshot before the race the pin stages (an
// earlier pass saw it waiting; R1 then cannot be what silences the record).
func heldOneLegCompound(t *testing.T, prefix string) (*Dispatcher, *store.DB, *testdb.MockBackend, *orders.Order, *orders.Order) {
	t.Helper()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	lane := mirrorLane(t, db, prefix+"-LANE", 3)
	slots, err := db.ListLaneSlots(lane)
	testutil.MustNoErr(t, err, "list the lane's slots")
	parent := &orders.Order{EdgeUUID: prefix + "-parent", StationID: "line-1",
		OrderType: OrderTypeRetrieve, Status: protocol.StatusReshuffling, Quantity: 1}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create the parent")
	leg := &orders.Order{EdgeUUID: prefix + "-leg", StationID: "line-1",
		OrderType: OrderTypeMove, Status: StatusPending, Quantity: 1,
		ParentOrderID: &parent.ID, Sequence: 1, SourceNode: slots[0].Name, DeliveryNode: sd.LineNode.Name}
	testutil.MustNoErr(t, db.CreateOrder(leg), "create the leg")

	backend := testdb.NewFailingBackend()
	d, _ := newTestDispatcher(t, db, backend)
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "the first advance, refused by the fleet")
	if freed := d.SweepLaneWaiters(); freed != 0 {
		t.Fatalf("fixture: the held pass recorded %d release(s); the fleet still refuses", freed)
	}
	if held, gErr := db.GetOrder(leg.ID); gErr != nil || held.VendorOrderID != "" {
		t.Fatalf("fixture: the leg must be held, unsent (err %v)", gErr)
	}
	backend.SetFail(false)
	return d, db, backend, parent, leg
}

// TestF2Pin_TheFloorRecordsACompoundItAdvancedNotOneItTried — DECISIONS D27 F2
// (TRACE-floor-releases.md (b), order 263). redriveHeldCompoundLegs returned
// every parent it ITERATED, and the floor counted a held leg as its own release
// whenever its parent was in that list and the leg's state had changed. Order
// 263: the creator dispatched the leg, the floor's AdvanceCompoundOrder lost
// the hand's status CAS ("fleet handover lost the CAS", which returns nil),
// and the floor still wrote a lane_floor_release row.
//
// MUTATION: return the iterated parents from redriveHeldCompoundLegs again; the
// lost-CAS subtest records the creator's dispatch as the floor's.
func TestF2Pin_TheFloorRecordsACompoundItAdvancedNotOneItTried(t *testing.T) {
	t.Parallel()

	t.Run("the floor loses the leg's CAS to another caller", func(t *testing.T) {
		t.Parallel()
		d, db, _, parent, leg := heldOneLegCompound(t, "F2CAS")
		// The other caller (263's intake dispatching its own first leg) wins
		// inside the floor's own advance: after the floor listed the parent and
		// before its hand's CAS.
		won := false
		d.SetPreHandHook(func(orderID int64) {
			if orderID == leg.ID && !won {
				won = true
				testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "the other caller's advance")
			}
		})
		freed := d.SweepLaneWaiters()
		if after, err := db.GetOrder(leg.ID); err != nil || after.VendorOrderID == "" || !won {
			t.Fatalf("fixture: the other caller did not dispatch the leg inside the floor's advance (won %v, err %v)", won, err)
		}
		if n := floorRecords(t, db, leg.ID); freed != 0 || n != 0 {
			t.Fatalf("the floor reported %d release(s) and wrote %d row(s) for a leg another caller "+
				"dispatched; its own advance lost the CAS and advanced nothing", freed, n)
		}
	})

	t.Run("the event dispatches the leg before the floor's re-drive", func(t *testing.T) {
		t.Parallel()
		d, db, _, parent, leg := heldOneLegCompound(t, "F2HOOK")
		d.floorPassHook = func() {
			testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "the event's advance")
		}
		freed := d.SweepLaneWaiters()
		if after, err := db.GetOrder(leg.ID); err != nil || after.VendorOrderID == "" {
			t.Fatalf("fixture: the event did not dispatch the leg (err %v)", err)
		}
		if n := floorRecords(t, db, leg.ID); freed != 0 || n != 0 {
			t.Fatalf("the floor reported %d release(s) and wrote %d row(s) for a leg the event dispatched", freed, n)
		}
	})

	t.Run("the floor's own advance is still recorded", func(t *testing.T) {
		t.Parallel()
		d, db, _, _, leg := heldOneLegCompound(t, "F2OWN")
		if freed := d.SweepLaneWaiters(); freed != 1 {
			t.Fatalf("the floor reported %d release(s), want its own 1", freed)
		}
		if n := floorRecords(t, db, leg.ID); n != 1 {
			t.Fatalf("the floor wrote %d row(s) for its own dispatch, want 1", n)
		}
	})
}

// TestR1Pin_AWaiterBornSinceThePreviousPassIsNotAMissedEvent — DECISIONS D27
// R1 (TRACE-floor-releases.md, "Ruling needed"). All seven traced floor
// releases were the floor's tick landing 5-60 ms after an order became a
// waiter, ahead of the event that made it one and was about to evaluate it.
// The floor still frees every waiter it finds; it writes the defect row only
// for one whose same waiting state (order, state tuple, cause) was already in
// the previous pass's snapshot: it waited a whole interval and nothing freed it.
//
// MUTATION: drop the previous-snapshot test in SweepLaneWaiters; the first
// subtest records a waiter no pass had seen.
func TestR1Pin_AWaiterBornSinceThePreviousPassIsNotAMissedEvent(t *testing.T) {
	t.Parallel()

	t.Run("a waiter the previous pass never saw is freed and not recorded", func(t *testing.T) {
		t.Parallel()
		d, db, waiter, laneID, digger := floorWaiterAtHeldLane(t, "R1BORN")
		d.laneLock.Unlock(laneID, digger.ID) // free, and no event has evaluated it
		freed := d.SweepLaneWaiters()
		if after, err := db.GetOrder(waiter.ID); err != nil || after.WaitIndex < 1 {
			t.Fatalf("the floor did not free the waiter (err %v): liveness is unchanged by R1", err)
		}
		if n := floorRecords(t, db, waiter.ID); freed != 0 || n != 0 {
			t.Fatalf("the floor reported %d and recorded %d row(s) for a waiter no previous pass had "+
				"seen: born this interval, its event in flight, not a missed one", freed, n)
		}
	})

	t.Run("a waiter held across a pass and then freed by nothing is recorded", func(t *testing.T) {
		t.Parallel()
		d, db, waiter, laneID, digger := floorWaiterAtHeldLane(t, "R1HELD")
		d.laneLock.Unlock(laneID, digger.ID)
		blocker := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = "R1HELD-blocker"
			o.Status = StatusDispatched
		})
		_, err := reservations.AcquireOccupancy(db.DB, blocker.ID, laneID)
		testutil.MustNoErr(t, err, "somebody is inside the lane")

		if freed := d.SweepLaneWaiters(); freed != 0 {
			t.Fatalf("the held pass reported %d release(s); the lane is occupied", freed)
		}
		held, err := db.GetOrder(waiter.ID)
		testutil.MustNoErr(t, err, "reload the waiter")
		if held.WaitIndex != waiter.WaitIndex || QueueCause(held.QueueCause) != CauseLaneOccupied {
			t.Fatalf("fixture: the waiter must be refused under %q (wait %d, cause %q)",
				CauseLaneOccupied, held.WaitIndex, held.QueueCause)
		}

		// The blocker leaves with no event: only the floor will ask.
		testutil.MustNoErr(t, reservations.ReleaseAllOccupancy(db.DB, blocker.ID), "the blocker leaves")
		if freed := d.SweepLaneWaiters(); freed != 1 {
			t.Fatalf("the floor reported %d release(s), want 1", freed)
		}
		recs, err := db.ListRecoveryActions(50)
		testutil.MustNoErr(t, err, "list recovery actions")
		var got []string
		for _, r := range recs {
			if r.Action == floorReleaseAction && r.TargetID == waiter.ID {
				got = append(got, r.Detail)
			}
		}
		if len(got) != 1 || !strings.Contains(got[0], string(CauseLaneOccupied)) {
			t.Fatalf("records = %q, want exactly one, under %q", got, CauseLaneOccupied)
		}
	})
}
