//go:build docker

package reservations_test

import (
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/shared/clock"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// TestReservations_AcquireConflict verifies ErrReservationConflict on a second
// Acquire for the same bin — the partial unique index is the gate.
func TestReservations_AcquireConflict(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-RES-CONFLICT")

	expires := clock.Now().Add(60 * time.Second)
	o1 := testdb.CreateOrder(t, db)
	o2 := testdb.CreateOrder(t, db)
	if err := reservations.Acquire(db, o1.ID, bin.ID, "test", "conflict", expires); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	// A different order acquiring the same bin must conflict.
	if err := reservations.Acquire(db, o2.ID, bin.ID, "test", "conflict", expires); err != reservations.ErrReservationConflict {
		t.Fatalf("second Acquire: wanted ErrReservationConflict, got %v", err)
	}
	_ = reservations.Release(db, o1.ID, bin.ID)
}

// TestReservations_AcquireConfirmRelease exercises the full happy-path sequence:
// Acquire (pending) → Confirm (confirmed) → Release (deleted).
func TestReservations_AcquireConfirmRelease(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-RES-ACR")

	expires := clock.Now().Add(60 * time.Second)
	o1 := testdb.CreateOrder(t, db)
	o2 := testdb.CreateOrder(t, db)
	if err := reservations.Acquire(db, o1.ID, bin.ID, "test", "acr", expires); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := reservations.Confirm(db, o1.ID, bin.ID); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	// Confirmed row still blocks a new order.
	if err := reservations.Acquire(db, o2.ID, bin.ID, "test", "acr", expires); err != reservations.ErrReservationConflict {
		t.Fatalf("Acquire after Confirm: wanted ErrReservationConflict, got %v", err)
	}
	if err := reservations.Release(db, o1.ID, bin.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// After release the bin is acquirable.
	if err := reservations.Acquire(db, o2.ID, bin.ID, "test", "acr", expires); err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	_ = reservations.Release(db, o2.ID, bin.ID)
}

// TestReservations_ConcurrentAcquire verifies that when N goroutines race Acquire
// on the same bin, exactly one wins. This is the DB-level race gate — the partial
// unique index resolves the race before any CAS attempt.
func TestReservations_ConcurrentAcquire(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-RACE-RESV")

	const N = 10
	orderIDs := make([]int64, N)
	for i := 0; i < N; i++ {
		orderIDs[i] = testdb.CreateOrder(t, db).ID
	}
	errs := make([]error, N)
	ready := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(N)
	expires := clock.Now().Add(60 * time.Second)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-ready
			errs[i] = reservations.Acquire(db, orderIDs[i], bin.ID, "test", "race", expires)
		}()
	}
	close(ready)
	wg.Wait()

	winners := 0
	winnerOrder := int64(-1)
	for i, err := range errs {
		if err == nil {
			winners++
			winnerOrder = orderIDs[i]
		} else if err != reservations.ErrReservationConflict {
			t.Errorf("goroutine %d: unexpected error %v (want nil or ErrReservationConflict)", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", winners)
	}
	if winnerOrder > 0 {
		_ = reservations.Release(db, winnerOrder, bin.ID)
	}
}

// TestReservations_Expire is the crash-leak backstop test: a pending reservation
// that was never Confirmed (simulating a crash between Acquire and Confirm) is
// reaped by Expire, and the bin becomes acquirable again.
func TestReservations_Expire(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-EXPIRE-1")

	// Acquire with a TTL already in the past — simulates a crash-leaked hold.
	pastExpiry := clock.Now().Add(-1 * time.Second)
	o1 := testdb.CreateOrder(t, db)
	o2 := testdb.CreateOrder(t, db)
	if err := reservations.Acquire(db, o1.ID, bin.ID, "test", "expire", pastExpiry); err != nil {
		t.Fatalf("Acquire with past TTL: %v", err)
	}

	// Another order cannot acquire yet (row exists).
	if err := reservations.Acquire(db, o2.ID, bin.ID, "test", "expire", clock.Now().Add(60*time.Second)); err != reservations.ErrReservationConflict {
		t.Fatalf("before Expire: wanted ErrReservationConflict, got %v", err)
	}

	n, err := reservations.Expire(db)
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if n == 0 {
		t.Fatal("Expire deleted 0 rows — expected at least 1 past-TTL reservation")
	}

	// After expiry the bin is acquirable.
	if err := reservations.Acquire(db, o2.ID, bin.ID, "test", "expire", clock.Now().Add(60*time.Second)); err != nil {
		t.Fatalf("Acquire after Expire: %v", err)
	}
	_ = reservations.Release(db, o2.ID, bin.ID)

	// HasPendingReservation should now be false.
	got, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("GetBin: %v", err)
	}
	if got.HasPendingReservation {
		t.Error("HasPendingReservation = true after Expire — expected false")
	}
}

// TestReservations_HasPendingReservation verifies the 1b domain field is correctly
// populated by BinJoinQuery: true while a pending row exists, false once released.
func TestReservations_HasPendingReservation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-HPR-1")

	expires := clock.Now().Add(60 * time.Second)
	o := testdb.CreateOrder(t, db)

	got, _ := db.GetBin(bin.ID)
	if got.HasPendingReservation {
		t.Fatal("HasPendingReservation should be false before any Acquire")
	}

	if err := reservations.Acquire(db, o.ID, bin.ID, "test", "hpr", expires); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	got, _ = db.GetBin(bin.ID)
	if !got.HasPendingReservation {
		t.Fatal("HasPendingReservation should be true after Acquire (state=pending)")
	}

	// After Confirm the field is checked against state='pending' only — confirmed
	// rows do not set it. The bin is now physically claimed, not reservation-pending.
	if err := reservations.Confirm(db, o.ID, bin.ID); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	got, _ = db.GetBin(bin.ID)
	if got.HasPendingReservation {
		t.Fatal("HasPendingReservation should be false after Confirm (only checks state=pending)")
	}

	if err := reservations.Release(db, o.ID, bin.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	got, _ = db.GetBin(bin.ID)
	if got.HasPendingReservation {
		t.Fatal("HasPendingReservation should be false after Release")
	}
}

// TestReservations_ReleaseByOrder verifies teardown paths: ReleaseByOrder deletes
// all of an order's reservations in one call, leaving each bin acquirable again.
func TestReservations_ReleaseByOrder(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)

	expires := clock.Now().Add(60 * time.Second)
	orderID := testdb.CreateOrder(t, db).ID

	bin1 := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-ROB-1")
	bin2 := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-ROB-2")

	for _, b := range []*bins.Bin{bin1, bin2} {
		if err := reservations.Acquire(db, orderID, b.ID, "test", "rob", expires); err != nil {
			t.Fatalf("Acquire bin %d: %v", b.ID, err)
		}
	}

	for _, b := range []*bins.Bin{bin1, bin2} {
		got, _ := db.GetBin(b.ID)
		if !got.HasPendingReservation {
			t.Fatalf("bin %d: HasPendingReservation should be true before ReleaseByOrder", b.ID)
		}
	}

	if err := reservations.ReleaseByOrder(db, orderID); err != nil {
		t.Fatalf("ReleaseByOrder: %v", err)
	}

	for _, b := range []*bins.Bin{bin1, bin2} {
		got, _ := db.GetBin(b.ID)
		if got.HasPendingReservation {
			t.Errorf("bin %d: HasPendingReservation still true after ReleaseByOrder", b.ID)
		}
	}
}

// TestReservations_SwapSiblingCancel asserts that when a two-robot swap pair is
// abandoned (the engine calls CancelOrderAtomic on each leg), both orders'
// reservations are released. This test exercises the cascade at the teardown
// boundary — the dispatch-level sibling lookup and LifecycleService.CancelOrder
// routing are tested elsewhere; this pins the reservation-release leg.
//
// The swap pair is explicitly linked via LinkOrderSiblingsByEdgeUUID to
// document the intent even though CancelOrderAtomic only needs the order ID.
func TestReservations_SwapSiblingCancel(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)

	// Create two sibling orders — supply and evac legs of a two-robot swap.
	supply := &orders.Order{
		EdgeUUID: "swap-sib-supply", StationID: "test",
		OrderType: "move", Status: "pending", Quantity: 1,
		DeliveryNode: sd.LineNode.Name,
	}
	evac := &orders.Order{
		EdgeUUID: "swap-sib-evac", StationID: "test",
		OrderType: "move", Status: "pending", Quantity: 1,
		DeliveryNode: sd.LineNode.Name,
	}
	if err := db.CreateOrder(supply); err != nil {
		t.Fatalf("create supply order: %v", err)
	}
	if err := db.CreateOrder(evac); err != nil {
		t.Fatalf("create evac order: %v", err)
	}
	if _, err := db.LinkOrderSiblingsByEdgeUUID(supply.EdgeUUID, evac.EdgeUUID); err != nil {
		t.Fatalf("link siblings: %v", err)
	}

	binSupply := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-SWP-SUPPLY")
	binEvac := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-SWP-EVAC")

	expires := clock.Now().Add(60 * time.Second)
	if err := reservations.Acquire(db, supply.ID, binSupply.ID, "test", "swap-supply", expires); err != nil {
		t.Fatalf("Acquire supply: %v", err)
	}
	if err := reservations.Acquire(db, evac.ID, binEvac.ID, "test", "swap-evac", expires); err != nil {
		t.Fatalf("Acquire evac: %v", err)
	}

	// Sanity: both bins show reserved before cancel.
	for _, b := range []*bins.Bin{binSupply, binEvac} {
		got, _ := db.GetBin(b.ID)
		if !got.HasPendingReservation {
			t.Fatalf("bin %d: expected HasPendingReservation=true before cancel", b.ID)
		}
	}

	// Simulate abandonOrder: terminate each leg through the chokepoint that
	// LifecycleService.CancelOrder routes to (transition → TerminalizeOrder for
	// StatusCancelled), which releases every reservation the order holds.
	if err := db.TerminalizeOrder(supply.ID, protocol.StatusCancelled, "test abandon"); err != nil {
		t.Fatalf("terminalize supply: %v", err)
	}
	if err := db.TerminalizeOrder(evac.ID, protocol.StatusCancelled, "test abandon"); err != nil {
		t.Fatalf("terminalize evac: %v", err)
	}

	// Both reservations must be gone.
	for _, b := range []*bins.Bin{binSupply, binEvac} {
		got, _ := db.GetBin(b.ID)
		if got.HasPendingReservation {
			t.Errorf("bin %d: HasPendingReservation=true after swap pair cancelled", b.ID)
		}
	}

	// No residual rows in the reservations table for either order.
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM reservations WHERE order_id IN ($1,$2)`,
		supply.ID, evac.ID).Scan(&count)
	if err != nil {
		t.Fatalf("count residual reservations: %v", err)
	}
	if count != 0 {
		t.Errorf("residual reservation rows = %d, want 0 after swap pair cancel", count)
	}
}
