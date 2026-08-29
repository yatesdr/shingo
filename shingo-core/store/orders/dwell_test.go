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

// DwellStats is the per-state dwell answer — where an order's time goes
// between being asked for and being confirmed. It reuses transitionCTE, so
// these assert the projection and the contract (count beside the percentiles,
// 0 for "no signal"), not the CTE itself, which lead_time_queries_test covers.

func TestDwellStats_PercentilesAndCounts(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	win := orders.LeadTimeRange{Start: base.Add(-time.Hour), End: base.Add(4 * time.Hour)}
	s := time.Second

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

	// Four orders whose queued→dispatched durations are 10, 20, 30, 40s.
	// p50 = 25, p95 = 38.5 (PERCENTILE_CONT interpolates).
	//
	// `dispatched`, not `acknowledged`: time_to_dispatch used to name a status
	// Core never writes, so the pair could only ever report zero — see
	// FlowDwellPairs. This fixture now walks the ladder a real order walks.
	for i, gap := range []time.Duration{10 * s, 20 * s, 30 * s, 40 * s} {
		off := time.Duration(i) * 10 * time.Minute
		seed(string(rune('a'+i)), "retrieve", "PART-A", []ev{
			{"queued", off}, {"dispatched", off + gap},
			{"in_transit", off + gap + 5*s}, {"delivered", off + gap + 65*s},
		})
	}
	// A staged leg that resumes, and one that ends at the destination — the two
	// exits staged has in the state machine, measured separately.
	seed("staged-resume", "complex", "PART-A", []ev{
		{"in_transit", 2 * time.Hour}, {"staged", 2*time.Hour + 10*s}, {"in_transit", 2*time.Hour + 100*s},
	})
	seed("staged-deliver", "complex", "PART-A", []ev{
		{"in_transit", 3 * time.Hour}, {"staged", 3*time.Hour + 10*s}, {"delivered", 3*time.Hour + 40*s},
	})

	rows, err := orders.DwellStats(db, orders.FlowDwellPairs(), "PART-A", "", win)
	testutil.MustNoErr(t, err, "DwellStats")

	got := map[string]domain.DwellStat{}
	for _, r := range rows {
		got[r.Key] = r
	}
	if len(rows) != len(orders.FlowDwellPairs()) {
		t.Fatalf("want one row per pair, got %d: %+v", len(rows), rows)
	}

	near := func(name string, a, b float64) {
		t.Helper()
		if math.Abs(a-b) > 0.01 {
			t.Errorf("%s = %v, want %v", name, a, b)
		}
	}

	ttd := got["time_to_dispatch"]
	if ttd.Count != 4 {
		t.Errorf("time_to_dispatch count = %d, want 4", ttd.Count)
	}
	near("time_to_dispatch p50", ttd.P50Seconds, 25)
	near("time_to_dispatch p95", ttd.P95Seconds, 38.5)

	// Both staged exits are counted, each on its own row — merging them would
	// average a 90s wait-for-release with a 30s final leg.
	if sr := got["staged_release"]; sr.Count != 1 || math.Abs(sr.P50Seconds-90) > 0.01 {
		t.Errorf("staged_release = %+v, want count 1 p50 90", sr)
	}
	if sd := got["staged_delivery"]; sd.Count != 1 || math.Abs(sd.P50Seconds-30) > 0.01 {
		t.Errorf("staged_delivery = %+v, want count 1 p50 30", sd)
	}
}

// No qualifying transitions is 0 with count 0, not an error and not a NULL
// scan failure — the package's "0 = no signal in the window" contract.
func TestDwellStats_EmptyWindowIsZeroNotError(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)

	far := orders.LeadTimeRange{
		Start: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC),
	}
	rows, err := orders.DwellStats(d.DB, orders.FlowDwellPairs(), "", "", far)
	testutil.MustNoErr(t, err, "DwellStats on an empty window")

	for _, r := range rows {
		if r.Count != 0 || r.P50Seconds != 0 || r.P95Seconds != 0 {
			t.Errorf("%s: want all zero on an empty window, got %+v", r.Key, r)
		}
	}
}

// payload_code and order_type scope the result — the same filters the four
// named helpers take, on the same CTE.
func TestDwellStats_FiltersScopeTheResult(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB

	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	win := orders.LeadTimeRange{Start: base.Add(-time.Hour), End: base.Add(time.Hour)}
	s := time.Second

	seed := func(uuid, orderType, payload string, gap time.Duration) {
		o := &domain.Order{
			EdgeUUID: uuid, StationID: "line-1", OrderType: protocol.OrderType(orderType), Status: "pending",
			Quantity: 1, PayloadCode: payload, DeliveryNode: "D", SourceNode: "S",
		}
		testutil.MustNoErr(t, orders.Create(db, o), "create "+uuid)
		var id int64
		testutil.MustNoErr(t, db.QueryRow(`SELECT id FROM orders WHERE edge_uuid=$1`, uuid).Scan(&id), "id "+uuid)
		for _, e := range []struct {
			status string
			off    time.Duration
		}{{"queued", 0}, {"acknowledged", gap}} {
			_, err := db.Exec(`INSERT INTO order_history (order_id, status, created_at) VALUES ($1,$2,$3)`,
				id, e.status, base.Add(e.off))
			testutil.MustNoErr(t, err, "hist "+uuid)
		}
	}
	seed("f-a", "retrieve", "PART-A", 10*s)
	seed("f-b", "retrieve", "PART-B", 60*s)
	seed("f-c", "move", "PART-A", 90*s)

	pairs := []domain.DwellPair{{Key: "ttd", From: "queued", To: "acknowledged"}}

	byPayload, err := orders.DwellStats(db, pairs, "PART-A", "", win)
	testutil.MustNoErr(t, err, "by payload")
	if byPayload[0].Count != 2 {
		t.Errorf("payload PART-A: count = %d, want 2 (retrieve + move)", byPayload[0].Count)
	}

	byBoth, err := orders.DwellStats(db, pairs, "PART-A", "retrieve", win)
	testutil.MustNoErr(t, err, "by payload+type")
	if byBoth[0].Count != 1 || math.Abs(byBoth[0].P50Seconds-10) > 0.01 {
		t.Errorf("payload PART-A + retrieve = %+v, want count 1 p50 10", byBoth[0])
	}
}
