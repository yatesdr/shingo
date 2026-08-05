package dispatch

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// buriedEmptyFinderFixture is TestFindSourceEmptyBuriedReshuffles' scene: one
// global empty sitting in a lane slot, with the reachability answer under the
// test's control.
func buriedEmptyFinderFixture(t *testing.T) (*fakeFinderDB, *orders.Order, int64, int64) {
	t.Helper()
	db := newFakeFinderDB()
	db.addNode(&nodes.Node{ID: 99, Name: "DEST"})
	laneID, slotID := int64(400), int64(401)
	db.addNode(&nodes.Node{ID: laneID, Name: "LANE-FC", NodeTypeCode: protocol.NodeClassLANE})
	db.addNode(&nodes.Node{ID: slotID, Name: "LANE-FC-SLOT-2", ParentID: &laneID})
	db.globalEmpty = &bins.Bin{ID: 77, PayloadCode: "", NodeID: &slotID}
	order := &orders.Order{ID: 8, OrderType: OrderTypeRetrieveEmpty, DeliveryNode: "DEST"}
	return db, order, laneID, slotID
}

// TestFindSource_UnreadableLaneIsBlockedNotClear pins D2 at the site that got it
// backwards.
//
// One question — "is anything occupied in front of this slot" — had three
// different error dispositions across its three callers. findShuffleSlots read a
// failed read as NOT REACHABLE (skip the slot), findBuriedBlockers read it as
// NOT A BLOCKER, and this site read it as REACHABLE: the guard was
//
//	if accessible, err := f.db.IsSlotAccessible(...); err == nil && !accessible {
//
// so a failed read fell straight through to OutcomeFound and a robot was
// dispatched to a slot nothing had successfully checked.
//
// The ruling is FAIL CLOSED, and the asymmetry is the whole argument: refusing
// to move is always recoverable, and driving into a lane whose state you could
// not read is not. The order must wait for the next scan.
func TestFindSource_UnreadableLaneIsBlockedNotClear(t *testing.T) {
	db, order, _, _ := buriedEmptyFinderFixture(t)
	db.accessibilityErr = errors.New("connection reset by peer")

	res := NewSourceFinder(db, nil, nil).FindSource(order, IntentEmpty)

	if res.Outcome == OutcomeFound {
		t.Fatalf("an unreadable lane produced OutcomeFound — the finder handed back a slot whose reachability " +
			"it never learned, and the caller will dispatch a robot into it")
	}
	if res.Outcome != OutcomeWait {
		t.Fatalf("outcome = %v, want OutcomeWait — an unanswered reachability question is congestion, not a "+
			"terminal fault; the order retries on the next scan", res.Outcome)
	}
	if res.QueueCode != protocol.QueueStorageRearranging {
		t.Errorf("queue code = %q, want %q — the order is waiting on storage to become reachable",
			res.QueueCode, protocol.QueueStorageRearranging)
	}
}

// TestFindSource_UnreadableLaneNodeIsBlockedNotClear covers the same defect one
// read deeper. Having established the slot IS buried, the tier resolved its lane
// under `lerr == nil` — so a failed lane read also fell through to OutcomeFound,
// dispatching to a slot the finder had just proven unreachable. Worse than the
// first case, and it was the same silent shape.
func TestFindSource_UnreadableLaneNodeIsBlockedNotClear(t *testing.T) {
	db, order, laneID, slotID := buriedEmptyFinderFixture(t)
	db.accessible = map[int64]bool{slotID: false} // definitively buried
	db.nodeErr = map[int64]error{laneID: errors.New("lane read failed")}

	res := NewSourceFinder(db, nil, nil).FindSource(order, IntentEmpty)

	if res.Outcome == OutcomeFound {
		t.Fatalf("OutcomeFound for a bin the finder had already established is BURIED — the lane read failed and " +
			"the guard fell through instead of parking")
	}
	if res.Outcome != OutcomeWait {
		t.Fatalf("outcome = %v, want OutcomeWait", res.Outcome)
	}
}

// TestFindSource_ReadableBuriedEmptyStillReshuffles is the control. Fail-closed
// must not become fail-always: when every read succeeds and the slot really is
// buried, the tier still routes to a dig.
func TestFindSource_ReadableBuriedEmptyStillReshuffles(t *testing.T) {
	db, order, laneID, slotID := buriedEmptyFinderFixture(t)
	db.accessible = map[int64]bool{slotID: false}

	res := NewSourceFinder(db, nil, nil).FindSource(order, IntentEmpty)

	if res.Outcome != OutcomeReshuffle {
		t.Fatalf("outcome = %v, want OutcomeReshuffle", res.Outcome)
	}
	if res.Buried == nil || res.Buried.Bin.ID != 77 || res.Buried.LaneID != laneID {
		t.Errorf("buried payload: got %+v, want bin 77 in lane %d", res.Buried, laneID)
	}
}
