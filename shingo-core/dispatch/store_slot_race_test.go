//go:build docker

package dispatch

import (
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// mkStoreOrder creates a queued order pointed at destName, carrying the literal
// type "store". The type is incidental: the slot race under test is driven by
// the destination being a STOR node. It stays a literal rather than becoming a
// constant because "store" is no longer a kind of order — it was the bin
// resolver's direction argument, and it now has its own type.
func mkStoreOrder(t *testing.T, db *store.DB, uuid, payload, destName string) *orders.Order {
	t.Helper()
	o := &orders.Order{
		EdgeUUID: uuid, StationID: "ST", OrderType: protocol.OrderType("store"),
		Status: StatusQueued, Quantity: 1, PayloadCode: payload, DeliveryNode: destName,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create store order")
	return o
}

// TestStoreDestinationSlot_ExactlyOneWins pins the #115/#117 fix: two store
// orders that resolve the SAME destination slot can no longer both claim it.
// The first wins the atomic reserve→confirm; the second gets an error and its
// caller requeues (polite wait), keeping its bin. Once the winner's slot frees,
// the loser can secure it on a later attempt — it never terminal-failed.
func TestStoreDestinationSlot_ExactlyOneWins(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	_, _, bp := setupTestData(t, db)
	storType, _ := db.GetNodeTypeByCode("STOR")

	dest := &nodes.Node{Name: "STORE-RACE-DEST", Zone: "A", Enabled: true, NodeTypeID: &storType.ID}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest node")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	order1 := mkStoreOrder(t, db, "store-race-1", bp.Code, dest.Name)
	order2 := mkStoreOrder(t, db, "store-race-2", bp.Code, dest.Name)

	// First store wins the slot (acquires the exclusive pending reservation).
	if dest := d.ReserveStorageDropoff(order1); dest.Refused() {
		t.Fatalf("first store must win the slot, got: %v", dest.Err)
	}
	// Second store loses and must be told to requeue (non-nil error) — it must
	// NOT also secure the same single-bin slot.
	// THE CAUSE IS PART OF THE CONTRACT NOW, not just the refusal: a lost slot
	// race is CONTENTION, and it has to arrive tagged as such or the caller parks
	// it under the undetermined cause and tells an operator nothing was readable.
	if lost := d.ReserveStorageDropoff(order2); !lost.Refused() {
		t.Fatal("second store must lose the slot race (caller requeues), got no refusal")
	} else if lost.Cause != CauseStoreSlotContended {
		t.Fatalf("a lost slot race parked under %q, want %q — the slot is genuinely taken, "+
			"which is the one shape at this door that is honest backpressure", lost.Cause,
			CauseStoreSlotContended)
	}

	// Replay idempotency: the winner re-securing its own slot succeeds without a
	// spurious self-conflict (the scanner re-runs this every dispatch tick); the
	// loser keeps losing while the winner holds the slot.
	if again := d.ReserveStorageDropoff(order1); again.Refused() {
		t.Fatalf("winner re-securing its own slot must succeed, got: %v", again.Err)
	}
	if lost := d.ReserveStorageDropoff(order2); !lost.Refused() {
		t.Fatal("loser must keep losing while the winner holds the slot, got no refusal")
	}

	// Polite wait: once the winner's slot frees (its order completed / released
	// its reservation), the loser secures it on a later attempt — it was queued,
	// never terminal-failed.
	testutil.MustNoErr(t, db.ReleaseSlotReservation(dest.ID, order1.ID), "release winner reservation")
	if freed := d.ReserveStorageDropoff(order2); freed.Refused() {
		t.Fatalf("loser must secure the freed slot on a later attempt, got: %v", freed.Err)
	}
}

// TestStoreDestinationSlot_ConcurrentExactlyOneWins stresses the same guarantee
// under real concurrency: two goroutines race to secure the same empty slot;
// exactly one must succeed.
func TestStoreDestinationSlot_ConcurrentExactlyOneWins(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	_, _, bp := setupTestData(t, db)
	storType, _ := db.GetNodeTypeByCode("STOR")

	dest := &nodes.Node{Name: "STORE-CRACE-DEST", Zone: "A", Enabled: true, NodeTypeID: &storType.ID}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest node")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	os := []*orders.Order{
		mkStoreOrder(t, db, "store-crace-1", bp.Code, dest.Name),
		mkStoreOrder(t, db, "store-crace-2", bp.Code, dest.Name),
	}

	var wg sync.WaitGroup
	errs := make([]error, len(os))
	wg.Add(len(os))
	for i, o := range os {
		go func(idx int, ord *orders.Order) {
			defer wg.Done()
			errs[idx] = d.ReserveStorageDropoff(ord).Err
		}(i, o)
	}
	wg.Wait()

	wins := 0
	for _, e := range errs {
		if e == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("exactly one store must win the slot, got %d winners (errs=%v)", wins, errs)
	}
}
