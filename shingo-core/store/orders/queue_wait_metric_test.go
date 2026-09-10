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

// TestAcquiringEntrySeamIsSpelledOnce is the drift pin between the two readers
// of "when did this order join the line".
//
// AvgL1QueueSeconds feeds the reorder-point calculator; the time_to_dispatch
// dwell pair feeds the board. They measure the same thing and they must measure
// the same population — a second spelling is how one of them silently keeps
// reading the old one, and the failure is invisible because both still return a
// number.
func TestAcquiringEntrySeamIsSpelledOnce(t *testing.T) {
	t.Parallel()
	var ttd domain.DwellPair
	for _, p := range orders.FlowDwellPairs() {
		if p.Key == "time_to_dispatch" {
			ttd = p
		}
	}
	want := domain.AcquiringEntryStatuses()
	if len(ttd.From) != len(want) {
		t.Fatalf("time_to_dispatch opens on %v, want the acquiring entry set %v", ttd.From, want)
	}
	for i := range want {
		if ttd.From[i] != want[i] {
			t.Errorf("time_to_dispatch opens on %v, want %v — AvgL1QueueSeconds takes its from-side "+
				"from AcquiringEntryStatuses, and the two must not drift", ttd.From, want)
		}
	}
	if !ttd.FromEarliest {
		t.Error("time_to_dispatch does not anchor on the FIRST acquiring row. AvgL1QueueSeconds " +
			"does, so the board and the reorder points would measure different spans for one wait.")
	}
}

// TestQueueWaitMetrics_CountAnOrderBornSourcing is the population the birth-rung
// move would otherwise have deleted from both instruments.
//
// A complex order is born `sourcing` and writes no `queued` history row. Keyed
// on `queued` alone, every one of them left time_to_dispatch and
// AvgL1QueueSeconds without appearing as a gap — and a mean computed over a
// silently narrowed population becomes a reorder point that under-replenishes.
func TestQueueWaitMetrics_CountAnOrderBornSourcing(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB

	base := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	win := orders.LeadTimeRange{Start: base.Add(-time.Hour), End: base.Add(2 * time.Hour)}
	s := time.Second

	// Born sourcing: no queued row anywhere in its history.
	o := &domain.Order{
		EdgeUUID: "born-sourcing", StationID: "line-1", OrderType: protocol.OrderTypeRetrieveEmpty,
		Status: protocol.StatusSourcing, Quantity: 1, PayloadCode: "PART-BS",
		DeliveryNode: "D", SourceNode: "S",
	}
	testutil.MustNoErr(t, orders.Create(db, o), "create")
	_, err := db.Exec(`DELETE FROM order_history WHERE order_id=$1`, o.ID)
	testutil.MustNoErr(t, err, "clear the birth row so the seeded ladder is the whole history")
	for _, e := range []struct {
		status protocol.Status
		off    time.Duration
	}{{protocol.StatusSourcing, 0}, {protocol.StatusDispatched, 30 * s}} {
		_, err := db.Exec(`INSERT INTO order_history (order_id, status, created_at) VALUES ($1,$2,$3)`,
			o.ID, string(e.status), base.Add(e.off))
		testutil.MustNoErr(t, err, "hist "+string(e.status))
	}

	q, err := orders.AvgL1QueueSeconds(db, "PART-BS", win)
	testutil.MustNoErr(t, err, "AvgL1QueueSeconds")
	if math.Abs(q-30) > 0.01 {
		t.Errorf("AvgL1QueueSeconds = %v, want 30.\n"+
			"An order born `sourcing` waited 30 seconds in the line and reached a robot. Measured "+
			"from `queued` alone it contributes NOTHING — not a zero, an absence — and the reorder "+
			"point computed from this mean is set as if that family never waits.", q)
	}

	rows, err := orders.DwellStats(db, orders.FlowDwellPairs(), "PART-BS", "", win)
	testutil.MustNoErr(t, err, "DwellStats")
	for _, r := range rows {
		if r.Key != "time_to_dispatch" {
			continue
		}
		if r.Count != 1 {
			t.Errorf("time_to_dispatch count = %d, want 1 — the born-sourcing order is missing "+
				"from the dwell board as well", r.Count)
		}
	}
}

// TestQueueWaitMetrics_APlainOrdersNumberIsUnchanged is the other half of the
// widening's claim. Adding `sourcing` to the from-set and anchoring on the first
// row must not move the number for an order that entered the line once, which is
// every plain order that does not re-queue.
func TestQueueWaitMetrics_APlainOrdersNumberIsUnchanged(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB

	base := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
	win := orders.LeadTimeRange{Start: base.Add(-time.Hour), End: base.Add(2 * time.Hour)}
	s := time.Second

	o := &domain.Order{
		EdgeUUID: "plain-unchanged", StationID: "line-1", OrderType: protocol.OrderTypeRetrieveEmpty,
		Status: protocol.StatusPending, Quantity: 1, PayloadCode: "PART-PU",
		DeliveryNode: "D", SourceNode: "S",
	}
	testutil.MustNoErr(t, orders.Create(db, o), "create")
	_, err := db.Exec(`DELETE FROM order_history WHERE order_id=$1`, o.ID)
	testutil.MustNoErr(t, err, "clear the birth row")
	// queued, then sourcing on the way out, then dispatched. The sourcing row is
	// the one the widened set could have grabbed: anchored on the LAST from-row
	// it would report 10s instead of 40s.
	for _, e := range []struct {
		status protocol.Status
		off    time.Duration
	}{
		{protocol.StatusQueued, 0},
		{protocol.StatusSourcing, 30 * s},
		{protocol.StatusDispatched, 40 * s},
	} {
		_, err := db.Exec(`INSERT INTO order_history (order_id, status, created_at) VALUES ($1,$2,$3)`,
			o.ID, string(e.status), base.Add(e.off))
		testutil.MustNoErr(t, err, "hist "+string(e.status))
	}

	q, err := orders.AvgL1QueueSeconds(db, "PART-PU", win)
	testutil.MustNoErr(t, err, "AvgL1QueueSeconds")
	if math.Abs(q-40) > 0.01 {
		t.Errorf("AvgL1QueueSeconds = %v, want 40 — the whole wait from joining the line to the "+
			"fleet call.\nAnchoring on the LAST acquiring row instead of the first would report 10 "+
			"and quietly shorten every plain order's measured queue segment.", q)
	}
}

// TestInFlightForDropoff_ShoppingCountsAndQueuedDoesNot states the direction of
// the birth-rung move's one measured consequence, in the place it is measured.
//
// The dropoff-capacity gate counts everything non-terminal EXCEPT `queued`. A
// complex order used to be born `queued` and so was invisible to the gate for as
// long as it stood there; born `sourcing`, it counts from birth. The gate
// therefore admits FEWER carriers to a node with complex demand aimed at it, and
// that is the deliberate behaviour the sim certifies — nothing in the code
// compensates for it.
//
// Both halves are asserted, because "counts more" is only meaningful beside the
// row that still does not count.
func TestInFlightForDropoff_ShoppingCountsAndQueuedDoesNot(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB

	mk := func(uuid string, status protocol.Status) {
		o := &domain.Order{
			EdgeUUID: uuid, StationID: "line-1", OrderType: "retrieve",
			Status: status, Quantity: 1, DeliveryNode: "CAP-NODE", SourceNode: "S",
		}
		testutil.MustNoErr(t, orders.Create(db, o), "create "+uuid)
	}

	mk("cap-queued", protocol.StatusQueued)
	n, err := orders.CountInFlightByDeliveryNode(db, "CAP-NODE")
	testutil.MustNoErr(t, err, "count with one queued order")
	if n != 0 {
		t.Fatalf("in-flight = %d with one queued order, want 0 — a queued order holds no "+
			"destination, and the scanner depends on it holding none", n)
	}

	mk("cap-sourcing", protocol.StatusSourcing)
	n, err = orders.CountInFlightByDeliveryNode(db, "CAP-NODE")
	testutil.MustNoErr(t, err, "count with a shopping order too")
	if n != 1 {
		t.Errorf("in-flight = %d with one queued and one sourcing order, want 1.\n"+
			"THIS IS THE DELTA the birth-rung move introduces: a complex order born `sourcing` "+
			"enters this count at birth where born `queued` it entered only on its first tick. "+
			"The gate admits fewer carriers to this node as a result, deliberately.", n)
	}
}
