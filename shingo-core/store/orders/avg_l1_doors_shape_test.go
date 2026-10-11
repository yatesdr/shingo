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

// TestAvgL1QueueSeconds_TheDoorsShape is the equality pin the ladder cutover
// moves, on the one shape the wire and loader doors write for a retrieve_empty
// (RULINGS-C1-P0, the telemetry prediction; PLAN row C1).
//
// The order is born `pending` at t0, waits `queued` from t0+Δ (Δ is the door's
// apply, milliseconds on a plant) and is dispatched at t0+Δ+w. AvgL1QueueSeconds
// anchors on the order's EARLIEST acquiring row (domain.AcquiringEntryStatuses).
// With {queued, sourcing} that is the queued row and the wait reads w; with
// {pending, queued} it is the birth row and the wait reads Δ + w: v4's
// definition, an order waits from birth.
//
// The prediction was "it moves by exactly Δ". Δ here is 3 s and w 20 s.
//
// RED at 647b65af by exactly Δ: it reads 20.
func TestAvgL1QueueSeconds_TheDoorsShape(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t).DB
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	win := orders.LeadTimeRange{Start: base.Add(-time.Hour), End: base.Add(2 * time.Hour)}
	const delta, wait = 3, 20

	o := &domain.Order{EdgeUUID: "door-shape", StationID: "line-1", OrderType: protocol.OrderTypeRetrieveEmpty,
		Status: protocol.StatusPending, Quantity: 1, PayloadCode: "PART-A", DeliveryNode: "D", SourceNode: "S"}
	testutil.MustNoErr(t, orders.Create(db, o), "create")
	var id int64
	testutil.MustNoErr(t, db.QueryRow(`SELECT id FROM orders WHERE edge_uuid = 'door-shape'`).Scan(&id), "id")
	// The creation row carries the wall clock; this fixture's history is the one below.
	_, err := db.Exec(`DELETE FROM order_history WHERE order_id = $1`, id)
	testutil.MustNoErr(t, err, "clear creation history")
	for _, h := range []struct {
		status protocol.Status
		code   string
		off    int
	}{
		{protocol.StatusPending, "", 0},
		{protocol.StatusQueued, string(protocol.QueueWaitingForMaterial), delta},
		{protocol.StatusDispatched, "", delta + wait},
	} {
		_, err := db.Exec(`INSERT INTO order_history (order_id, status, code, created_at) VALUES ($1, $2, $3, $4)`,
			id, string(h.status), h.code, base.Add(time.Duration(h.off)*time.Second))
		testutil.MustNoErr(t, err, "history "+string(h.status))
	}

	got, err := orders.AvgL1QueueSeconds(db, "PART-A", win)
	testutil.MustNoErr(t, err, "AvgL1QueueSeconds")
	if math.Abs(got-(delta+wait)) > 0.001 {
		t.Fatalf("AvgL1QueueSeconds = %v, want %d (Δ %d + w %d): the wait starts at birth. "+
			"If it reads %d the anchor is still the first queued row; anything else is a stop for the owner",
			got, delta+wait, delta, wait, wait)
	}
}
