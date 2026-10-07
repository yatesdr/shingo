//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
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
		d.laneLock.Unlock(laneID, digger.ID) // freed with no evaluation: only the floor will ask
		if freed := d.SweepLaneWaiters(); freed != 1 {
			t.Fatalf("the floor reported %d release(s), want its own 1", freed)
		}
		if n := floorRecords(t, db, waiter.ID); n != 1 {
			t.Fatalf("the floor recorded %d row(s) for its own release, want 1", n)
		}
	})
}
