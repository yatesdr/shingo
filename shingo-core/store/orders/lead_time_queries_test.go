//go:build docker

// Black-box (package orders_test) per the cycle note in orders_test.go.
package orders_test

import (
	"math"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestLeadTimeHelpers_Fidelity seeds one known window of order_history and
// asserts every Core lead-time helper computes the exact expected duration —
// the one-window fidelity check for the Edge→Core calculator port. The numbers
// are the ground truth both implementations must reproduce (mean for L1
// transit + L1 queue, median for L2 load, p95 for market→cell).
func TestLeadTimeHelpers_Fidelity(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	win := orders.LeadTimeRange{Start: base.Add(-time.Hour), End: base.Add(2 * time.Hour)}

	type ev struct {
		status string
		off    time.Duration
	}
	seed := func(uuid, orderType, payload string, evs []ev) {
		o := &domain.Order{
			EdgeUUID: uuid, StationID: "line-1", OrderType: protocol.OrderType(orderType), Status: "pending",
			Quantity: 1, PayloadCode: payload, DeliveryNode: "D", SourceNode: "S",
		}
		testutil.MustNoErr(t, orders.Create(db, o), "create "+uuid)
		var id int64
		testutil.MustNoErr(t, db.QueryRow(`SELECT id FROM orders WHERE edge_uuid=$1`, uuid).Scan(&id), "id "+uuid)
		for _, e := range evs {
			_, err := db.Exec(`INSERT INTO order_history (order_id, status, created_at) VALUES ($1,$2,$3)`,
				id, e.status, base.Add(e.off))
			testutil.MustNoErr(t, err, "hist "+uuid+" "+e.status)
		}
	}

	s := time.Second
	// retrieve_empty A: queue 10s, L1 transit 10s, L2 load 20s.
	seed("A", "retrieve_empty", "PART-A", []ev{
		{"queued", 0}, {"acknowledged", 10 * s}, {"in_transit", 15 * s}, {"delivered", 25 * s}, {"confirmed", 45 * s},
	})
	// retrieve_empty B: queue 20s, L1 transit 20s, L2 load 35s.
	seed("B", "retrieve_empty", "PART-A", []ev{
		{"queued", 100 * s}, {"acknowledged", 120 * s}, {"in_transit", 125 * s}, {"delivered", 145 * s}, {"confirmed", 180 * s},
	})
	// retrieve D: market→cell 40s.
	seed("D", "retrieve", "PART-A", []ev{{"in_transit", 300 * s}, {"delivered", 340 * s}})
	// a different payload that must NOT leak into PART-A results.
	seed("E", "retrieve_empty", "PART-Z", []ev{{"queued", 0}, {"acknowledged", 999 * s}})

	check := func(name string, got float64, err error, want float64) {
		t.Helper()
		testutil.MustNoErr(t, err, name)
		if math.Abs(got-want) > 0.01 {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}

	q, err := orders.AvgL1QueueSeconds(db, "PART-A", win)
	check("AvgL1QueueSeconds", q, err, 15) // (10+20)/2
	tr, err := orders.AvgL1TransitSeconds(db, "PART-A", win)
	check("AvgL1TransitSeconds", tr, err, 15) // (10+20)/2
	ld, err := orders.MedianL2LoadSeconds(db, "PART-A", win)
	check("MedianL2LoadSeconds", ld, err, 27.5) // median(20,35)
	mc, err := orders.P95MarketToCellSeconds(db, "PART-A", win)
	check("P95MarketToCellSeconds", mc, err, 40)

	// confidence-coverage counts.
	if n, err := orders.CountCompletedOrdersInWindow(db, "retrieve_empty", "confirmed", "PART-A", win); err != nil || n != 2 {
		t.Errorf("count retrieve_empty/confirmed = %d,%v, want 2", n, err)
	}
	if n, err := orders.CountCompletedOrdersInWindow(db, "retrieve", "delivered", "PART-A", win); err != nil || n != 1 {
		t.Errorf("count retrieve/delivered = %d,%v, want 1", n, err)
	}

	// window guard: a window that excludes everything yields 0 (no signal).
	empty := orders.LeadTimeRange{Start: base.Add(48 * time.Hour), End: base.Add(72 * time.Hour)}
	if v, err := orders.AvgL1QueueSeconds(db, "PART-A", empty); err != nil || v != 0 {
		t.Errorf("AvgL1QueueSeconds(empty window) = %v,%v, want 0", v, err)
	}
}
