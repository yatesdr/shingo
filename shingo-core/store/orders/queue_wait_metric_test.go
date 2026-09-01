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

// TestQueueWaitMetrics_MeasureATransitionCoreActuallyWrites is the fixture that
// says what was wrong: an order's real timeline, with no `acknowledged` row in
// it, because Core has never written one.
//
// `acknowledged` reaches the ladder from exactly one arm, and that arm is
// documented dead in as many words: fleet.MapState never returns it
// (engine/wiring_vendor_status.go — "this arm is dead in practice", a
// never-fires guard against a future adapter). Both the queue-wait metrics
// measured queued → acknowledged, so both were STRUCTURALLY ZERO: not "no
// signal in this window", but no signal possible, ever, on any plant.
//
// That is not a harmless zero. AvgL1QueueSeconds feeds the replenishment
// threshold calculator's L1 lead time, and a lead time missing its queue
// segment sets every threshold too low — under-replenishment, silently, with a
// number on the screen to make it look measured.
//
// So the pair moves to queued → dispatched: the fleet call made, armor on. It is
// the honest end of the wait in the line, and both endpoints are transitions the
// lifecycle writes on every order that ever reaches a robot.
func TestQueueWaitMetrics_MeasureATransitionCoreActuallyWrites(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB

	base := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	win := orders.LeadTimeRange{Start: base.Add(-time.Hour), End: base.Add(2 * time.Hour)}
	s := time.Second

	type ev struct {
		status protocol.Status
		off    time.Duration
	}
	seed := func(uuid string, evs []ev) {
		o := &domain.Order{
			EdgeUUID: uuid, StationID: "line-1", OrderType: protocol.OrderTypeRetrieveEmpty,
			Status: protocol.StatusPending, Quantity: 1, PayloadCode: "PART-QW",
			DeliveryNode: "D", SourceNode: "S",
		}
		testutil.MustNoErr(t, orders.Create(db, o), "create "+uuid)
		for _, e := range evs {
			_, err := db.Exec(`INSERT INTO order_history (order_id, status, created_at) VALUES ($1,$2,$3)`,
				o.ID, string(e.status), base.Add(e.off))
			testutil.MustNoErr(t, err, "hist "+uuid+" "+string(e.status))
		}
	}

	// The ladder a real order walks: pending, queued, dispatched, in_transit,
	// delivered, confirmed. No `acknowledged` anywhere, because Core writes none.
	seed("qw-a", []ev{
		{protocol.StatusQueued, 0}, {protocol.StatusDispatched, 10 * s},
		{protocol.StatusInTransit, 15 * s}, {protocol.StatusDelivered, 25 * s},
		{protocol.StatusConfirmed, 45 * s},
	})
	seed("qw-b", []ev{
		{protocol.StatusQueued, 100 * s}, {protocol.StatusDispatched, 120 * s},
		{protocol.StatusInTransit, 125 * s}, {protocol.StatusDelivered, 145 * s},
		{protocol.StatusConfirmed, 180 * s},
	})

	q, err := orders.AvgL1QueueSeconds(db, "PART-QW", win)
	testutil.MustNoErr(t, err, "AvgL1QueueSeconds")
	if math.Abs(q-15) > 0.01 {
		t.Errorf("AvgL1QueueSeconds = %v, want 15 — the mean of a 10s and a 20s wait in the line.\n"+
			"A zero here is the metric measuring a transition Core never writes, which is not a "+
			"quiet window: the threshold calculator adds this to its L1 lead time and sets every "+
			"reorder point as if the queue took no time at all.", q)
	}

	rows, err := orders.DwellStats(db, orders.FlowDwellPairs(), "PART-QW", "", win)
	testutil.MustNoErr(t, err, "DwellStats")
	var ttd orders.DwellStat
	for _, r := range rows {
		if r.Key == "time_to_dispatch" {
			ttd = r
		}
	}
	if ttd.Count != 2 {
		t.Errorf("time_to_dispatch count = %d, want 2 — both orders waited in the line and both "+
			"reached a robot; a zero count means the pair names a status nothing writes", ttd.Count)
	}
	if math.Abs(ttd.P50Seconds-15) > 0.01 {
		t.Errorf("time_to_dispatch p50 = %v, want 15", ttd.P50Seconds)
	}
}
