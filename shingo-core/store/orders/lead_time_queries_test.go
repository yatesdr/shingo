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
			_, err := db.Exec(`INSERT INTO order_history (order_id, status, created_at, code)
				VALUES ($1,$2,$3, CASE WHEN $2 = 'queued' THEN 'waiting_for_material' END)`,
				id, e.status, base.Add(e.off))
			testutil.MustNoErr(t, err, "hist "+uuid+" "+e.status)
		}
	}

	s := time.Second
	// THE QUEUE SEGMENT ENDS AT `dispatched`, not at `acknowledged`: Core writes
	// no acknowledged row on any plant (see AvgL1QueueSeconds), so a fixture that
	// seeded one was measuring a shape the system does not produce.
	//
	// retrieve_empty A: queue 10s, L1 transit 10s, L2 load 20s.
	seed("A", "retrieve_empty", "PART-A", []ev{
		{"queued", 0}, {"dispatched", 10 * s}, {"in_transit", 15 * s}, {"delivered", 25 * s}, {"confirmed", 45 * s},
	})
	// retrieve_empty B: queue 20s, L1 transit 20s, L2 load 35s.
	seed("B", "retrieve_empty", "PART-A", []ev{
		{"queued", 100 * s}, {"dispatched", 120 * s}, {"in_transit", 125 * s}, {"delivered", 145 * s}, {"confirmed", 180 * s},
	})
	// retrieve D: market→cell 40s.
	seed("D", "retrieve", "PART-A", []ev{{"in_transit", 300 * s}, {"delivered", 340 * s}})
	// a different payload that must NOT leak into PART-A results.
	seed("E", "retrieve_empty", "PART-Z", []ev{{"queued", 0}, {"dispatched", 999 * s}})

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

// TestLeadTimesByPayload_RateIsTheRunningRate pins the rate /inventory turns
// into a cycle. A tte_samples rate is a sliding window's count, so after a stop
// it decays to one unit per window before it reads 0. The latest positive
// sample of an idle part is that tail; the rate is the median of the window's
// positive samples, the line's running rate. A sample before the window does
// not count.
func TestLeadTimesByPayload_RateIsTheRunningRate(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB
	_, err := db.Exec(`INSERT INTO demand_registry (station_id, core_node_name, role, payload_code, replenish_uop_threshold)
		VALUES ('line-1', 'SMN_R1', 'consume', 'PART-R', 100)`)
	testutil.MustNoErr(t, err, "seed registry")
	sample := func(ago time.Duration, rate float64) {
		_, err := db.Exec(`INSERT INTO tte_samples (computed_at, process_id, style_id, core_node_name, payload_code,
			uop_remaining, rate_per_sec, style_status, kind) VALUES ($1, 'LINE-R', 'S1', 'SMN_R1', 'PART-R', 500, $2, '', 'threshold')`,
			time.Now().Add(-ago), rate)
		testutil.MustNoErr(t, err, "seed sample")
	}
	// Twenty samples two minutes apart, running at 70 s a unit, then the stop's
	// tail (900 s, 1800 s), then the line idle at 0.
	for i := range 20 {
		sample(time.Duration(120-2*i)*time.Minute, 1.0/70)
	}
	sample(78*time.Minute, 1.0/900)
	sample(76*time.Minute, 1.0/1800)
	for i := range 10 {
		sample(time.Duration(74-2*i)*time.Minute, 0)
	}
	// Before the window: a different rate, more samples than the window has.
	for i := range 40 {
		sample(20*24*time.Hour+time.Duration(i)*time.Minute, 1.0/5)
	}

	got, err := orders.LeadTimesByPayload(db, orders.LeadTimeRange{Start: time.Now().AddDate(0, 0, -14), End: time.Now()})
	testutil.MustNoErr(t, err, "lead times by payload")
	if rate := got["PART-R"].RatePerSec; math.Abs(rate-1.0/70) > 1e-9 {
		t.Errorf("rate = %v (a %.0f s cycle), want the running 1/70 (70 s): the stop's tail or the old samples leaked in",
			rate, 1/rate)
	}
}
