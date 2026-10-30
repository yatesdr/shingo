//go:build docker

package service

import (
	"context"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// Pins for the /inventory redesign (lane D, 2026-10-29), committed green at the
// base before the page changed. Predictions are in the work folder
// (PREDICTIONS-inventory-page-pins-2026-10-29.md).

// TestInventoryPin_SystemUOPSum pins the in-loop sum the page prints, for the
// three cases the brief names: confirmed bins, a bin with no count, and a
// lineside pile.
func TestInventoryPin_SystemUOPSum(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	const payload = "PIN-SUM"
	at := &nodes.Node{Name: "PIN-SUM-AT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(at), "create node")
	svc := NewInventoryService(db)
	sum := func() PayloadSystemUOP {
		t.Helper()
		res, err := svc.SystemUOPForPayload(context.Background(), []string{payload})
		testutil.MustNoErr(t, err, "SystemUOPForPayload")
		if len(res.Counts) != 1 {
			t.Fatalf("counts = %+v, want one", res.Counts)
		}
		return res.Counts[0]
	}

	// (a) confirmed bins.
	createTestBin(t, db, at.ID, "PIN-SUM-A", payload, 30)
	createTestBin(t, db, at.ID, "PIN-SUM-B", payload, 40)
	if got := sum(); got.BinUOP != 70 {
		t.Errorf("(a) two confirmed bins 30+40: bins %d, want 70", got.BinUOP)
	}

	// (b) a bin with no count. uop_remaining is NOT NULL, so NULL never reaches
	// the sum; the case a plant reaches is an unconfirmed manifest, and the sum
	// counts its uop_remaining as it stands.
	unc := createTestBin(t, db, at.ID, "PIN-SUM-UNC", payload, 5)
	if _, err := db.Exec(`UPDATE bins SET uop_remaining = NULL WHERE id = $1`, unc.ID); err == nil {
		t.Errorf("(b) uop_remaining accepted NULL; the schema was NOT NULL")
	}
	_, err := db.Exec(`UPDATE bins SET manifest_confirmed = false WHERE id = $1`, unc.ID)
	testutil.MustNoErr(t, err, "unconfirm")
	if got := sum(); got.BinUOP != 75 {
		t.Errorf("(b) an unconfirmed bin of 5 beside 70: bins %d, want 75 (counted)", got.BinUOP)
	}

	// (c) a lineside pile: active counts, stranded does not.
	_, err = db.Exec(`INSERT INTO lineside_buckets (station, core_node_name, payload_code, state, qty)
		VALUES ('PIN-ST', 'PIN-SUM-AT', $1, 'active', 11), ('PIN-ST', 'PIN-SUM-AT', $1, 'stranded', 5)`, payload)
	testutil.MustNoErr(t, err, "seed piles")
	got := sum()
	t.Logf("PIN inventory/system-uop bins=%d buckets=%d total=%d", got.BinUOP, got.BucketUOP, got.TotalUOP)
	if got.BucketUOP != 11 || got.TotalUOP != 86 {
		t.Errorf("(c) piles active 11 + stranded 5: buckets %d total %d, want 11 and 86", got.BucketUOP, got.TotalUOP)
	}
}

// seedLeadTimeOrder writes one order of orderType for payload with the given
// status history, each status at its offset (seconds) from start.
func seedLeadTimeOrder(t *testing.T, db *store.DB, uuid, orderType, payload string, start time.Time, steps []leadStep) {
	t.Helper()
	var id int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO orders (edge_uuid, order_type, status, payload_code, delivery_node)
		VALUES ($1, $2, $3, $4, 'PIN-CALC-HOME') RETURNING id`,
		uuid, orderType, steps[len(steps)-1].status, payload).Scan(&id), "insert order")
	for _, s := range steps {
		_, err := db.Exec(`INSERT INTO order_history (order_id, status, created_at) VALUES ($1, $2, $3)`,
			id, s.status, start.Add(time.Duration(s.at)*time.Second))
		testutil.MustNoErr(t, err, "insert history")
	}
}

type leadStep struct {
	status string
	at     int
}

// TestInventoryPin_CalculatorOnePair pins the calculator's suggestion and
// confidence for one (loader position, payload) pair from known order history:
// three L1 retrieve_empty orders (queue 60 s, transit 120 s, load 300 s) and
// three retrieves (market to cell 90 s), at a 30 s cycle and the default safety.
func TestInventoryPin_CalculatorOnePair(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	const payload = "PIN-CALC"
	base := time.Now().Add(-2 * time.Hour)
	for i, u := range []string{"pin-l1-a", "pin-l1-b", "pin-l1-c"} {
		seedLeadTimeOrder(t, db, u, "retrieve_empty", payload, base.Add(time.Duration(i)*10*time.Minute), []leadStep{
			{"queued", 0}, {"dispatched", 60}, {"in_transit", 70}, {"delivered", 190}, {"confirmed", 490},
		})
	}
	for i, u := range []string{"pin-r-a", "pin-r-b", "pin-r-c"} {
		seedLeadTimeOrder(t, db, u, "retrieve", payload, base.Add(time.Duration(i)*10*time.Minute), []leadStep{
			{"queued", 0}, {"dispatched", 5}, {"in_transit", 10}, {"delivered", 100},
		})
	}

	res, err := NewThresholdCalculatorService(db).CalculateDays("PIN-CALC-HOME", payload, 14, 0, 30, 0)
	testutil.MustNoErr(t, err, "calculate")
	in := res.Inputs
	t.Logf("PIN inventory/calculator queue=%.0f transit=%.0f load=%.0f l2=%.0f m2c=%.0f l1=%d cell=%d conf=%s samples=%d/%d",
		in.L1QueueSeconds, in.L1TransitSeconds, in.L2LoadSeconds, in.L2TransitSeconds, in.MarketToCellSeconds,
		res.Outputs.L1Threshold, res.Outputs.CellReorder, res.Confidence, res.SamplesL1, res.SamplesRetrieve)
	if in.L1QueueSeconds != 60 || in.L1TransitSeconds != 120 || in.L2LoadSeconds != 300 ||
		in.L2TransitSeconds != 0 || in.MarketToCellSeconds != 90 {
		t.Errorf("inputs = %+v, want queue 60, transit 120, load 300, l2 0, market 90", in)
	}
	if res.Outputs.L1Threshold != 24 || res.Outputs.CellReorder != 5 || res.Confidence != "LOW" {
		t.Errorf("suggestion = L1 %d, cell %d, %s; want 24, 5, LOW", res.Outputs.L1Threshold, res.Outputs.CellReorder, res.Confidence)
	}
	if res.SamplesL1 != 3 || res.SamplesRetrieve != 3 {
		t.Errorf("samples = %d L1, %d retrieve; want 3 and 3", res.SamplesL1, res.SamplesRetrieve)
	}

	// The page's one aggregate (I1) reads the same inputs for every payload
	// at once, and agrees with the four helpers and the two counts.
	window := orders.LeadTimeRange{Start: time.Now().AddDate(0, 0, -14), End: time.Now()}
	one, err := NewThresholdCalculatorService(db).Calculate(CalculateRequest{PayloadCode: payload, DateRange: window, CycleSeconds: 30})
	testutil.MustNoErr(t, err, "calculate one")
	all, err := db.LeadTimesByPayload(window)
	testutil.MustNoErr(t, err, "lead times by payload")
	agg := all[payload]
	t.Logf("PIN inventory/lead-aggregate queue=%.0f transit=%.0f load=%.0f m2c=%.0f samples=%d/%d",
		agg.L1QueueSeconds, agg.L1TransitSeconds, agg.L2LoadSeconds, agg.MarketToCellSeconds, agg.SamplesL1, agg.SamplesRetrieve)
	if agg.L1QueueSeconds != one.Inputs.L1QueueSeconds || agg.L1TransitSeconds != one.Inputs.L1TransitSeconds ||
		agg.L2LoadSeconds != one.Inputs.L2LoadSeconds || agg.MarketToCellSeconds != one.Inputs.MarketToCellSeconds ||
		agg.SamplesL1 != one.SamplesL1 || agg.SamplesRetrieve != one.SamplesRetrieve {
		t.Errorf("aggregate %+v disagrees with the calculator's own reads %+v (samples %d/%d)",
			agg, one.Inputs, one.SamplesL1, one.SamplesRetrieve)
	}
	got := CalculateThresholds(ThresholdCalculatorInputs{CycleSeconds: 30, L1QueueSeconds: agg.L1QueueSeconds,
		L1TransitSeconds: agg.L1TransitSeconds, L2LoadSeconds: agg.L2LoadSeconds, MarketToCellSeconds: agg.MarketToCellSeconds})
	if got.L1Threshold != one.Outputs.L1Threshold {
		t.Errorf("suggestion from the aggregate %d, from the calculator %d", got.L1Threshold, one.Outputs.L1Threshold)
	}
}
