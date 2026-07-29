//go:build docker

package dispatch

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingo/shared/clock"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// futility_test.go covers the detector's arithmetic against real plant rates.
// These cover the seam it hangs off: transition() is the one chokepoint every
// status write goes through, and the classification "did this order ever
// reach in_transit" has to be answered from history rather than from the
// status the order happens to be terminating FROM.

func armDetector(t *testing.T, d *Dispatcher, threshold int, clk *clock.Manual) *fakeAuditor {
	t.Helper()
	aud := &fakeAuditor{}
	det := NewFutilityDetector(FutilityConfig{
		Enabled:       true,
		Threshold:     threshold,
		Window:        time.Hour,
		AlertThrottle: 15 * time.Minute,
	}, func(string, ...any) {}, aud)
	det.SetClock(clk.Now)
	d.lifecycle.futility = det
	return aud
}

func seedFutilityOrder(t *testing.T, db *store.DB, uuid, node, payload string) *orders.Order {
	t.Helper()
	o := &orders.Order{
		EdgeUUID:     uuid,
		StationID:    "plant-a.line-1",
		OrderType:    protocol.OrderTypeRetrieve,
		Status:       protocol.StatusQueued,
		Quantity:     1,
		ProcessNode:  node,
		PayloadCode:  payload,
		SourceNode:   "S",
		DeliveryNode: "D",
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create "+uuid)
	return o
}

// An order cancelled straight out of queued never had a robot: futile.
func TestFutilityWiring_QueuedCancelCountsAsFutile(t *testing.T) {
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	clk := clock.NewManual(time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC))
	aud := armDetector(t, d, 3, clk)

	key := FutilityKey{StationID: "plant-a.line-1", ProcessNode: "ALN_003", PayloadCode: "74577-6SA0A.06"}
	for i := range 3 {
		o := seedFutilityOrder(t, db, "fut-q-"+string(rune('a'+i)), key.ProcessNode, key.PayloadCode)
		d.Lifecycle().CancelOrder(o, o.StationID, "no_source_bin")
		clk.Advance(time.Minute)
	}

	if got := len(aud.all()); got != 1 {
		t.Fatalf("3 queued→cancelled on one tuple at threshold 3 should record once, got %d", got)
	}
}

// An order that reached in_transit and was cancelled later is NOT futile —
// a robot moved for it. `from` alone cannot tell these apart, which is why
// the classification reads order_history.
func TestFutilityWiring_OrderThatMovedIsNotFutile(t *testing.T) {
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	clk := clock.NewManual(time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC))
	aud := armDetector(t, d, 2, clk)

	key := FutilityKey{StationID: "plant-a.line-1", ProcessNode: "ALN_007", PayloadCode: "MOVED-PART"}
	for i := range 3 {
		o := seedFutilityOrder(t, db, "fut-m-"+string(rune('a'+i)), key.ProcessNode, key.PayloadCode)
		// queued → in_transit is a legal edge; the history row it writes is
		// what the classifier reads back.
		testutil.MustNoErr(t, d.Lifecycle().MarkInTransit(o, "AMR-01", "test"), "mark in transit")
		d.Lifecycle().CancelOrder(o, o.StationID, "operator cancelled mid-flight")
		clk.Advance(time.Minute)
	}

	if got := len(aud.all()); got != 0 {
		t.Fatalf("orders that reached in_transit are not futile, got %d records", got)
	}
}

// One robot departing for the tuple clears the count, so an intermittent
// problem never accumulates into a false record.
func TestFutilityWiring_InTransitResetsTheTuple(t *testing.T) {
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	clk := clock.NewManual(time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC))
	aud := armDetector(t, d, 3, clk)

	key := FutilityKey{StationID: "plant-a.line-1", ProcessNode: "ALN_009", PayloadCode: "INTERMITTENT"}

	// Two futile terminals.
	for i := range 2 {
		o := seedFutilityOrder(t, db, "fut-r-"+string(rune('a'+i)), key.ProcessNode, key.PayloadCode)
		d.Lifecycle().CancelOrder(o, o.StationID, "no_source_bin")
	}
	// One order genuinely departs — the reset.
	moved := seedFutilityOrder(t, db, "fut-r-moved", key.ProcessNode, key.PayloadCode)
	testutil.MustNoErr(t, d.Lifecycle().MarkInTransit(moved, "AMR-02", "test"), "mark in transit")

	// Two more futile terminals: 4 total, but only 2 since the reset.
	for i := range 2 {
		o := seedFutilityOrder(t, db, "fut-r-post-"+string(rune('a'+i)), key.ProcessNode, key.PayloadCode)
		d.Lifecycle().CancelOrder(o, o.StationID, "no_source_bin")
	}

	if got := len(aud.all()); got != 0 {
		t.Fatalf("the reset should have kept the count below threshold, got %d records", got)
	}
	if got := d.lifecycle.futility.Count(key); got != 2 {
		t.Fatalf("count after reset = %d, want 2", got)
	}
}

// With no detector installed — the default until a plant opts in — the hook
// is inert and nothing changes about the transition.
func TestFutilityWiring_NilDetectorIsInert(t *testing.T) {
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	o := seedFutilityOrder(t, db, "fut-nil", "ALN_003", "PART-X")
	d.Lifecycle().CancelOrder(o, o.StationID, "no_source_bin")

	got, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload order")
	if got.Status != protocol.StatusCancelled {
		t.Fatalf("status = %s, want cancelled — the futility hook must not affect the transition", got.Status)
	}
}
