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

// TestAvgL1QueueSeconds_EveryRung is the equality pin for the ladder cutover.
//
// AvgL1QueueSeconds feeds the reorder-point calculator, and its from-side is
// domain.AcquiringEntryStatuses. When the ladder changes which rungs an order
// stands on, this number can move, and a moved number moves every reorder point
// in the plant. So it is pinned here over one order per shape the ladder
// produces today, each with a known history, and the pin asserts TODAY's value.
// A change that moves it is a ruling for the owner, not a slice.
//
// Each order's contribution under today's from-set {queued, sourcing}
// (earliest acquiring row → last `dispatched` at or after it):
//
//	shape                                   rows (seconds from base)                         today
//	A  wire order, parked queued            pending 0, queued 5, dispatched 15               10
//	B  scanner soft-hold                    pending 0, sourcing 2, dispatched 12             10
//	C  complex, born sourcing               sourcing 0, dispatched 30                        30
//	D  immediate dispatch, no wait          pending 0, dispatched 4                          not counted
//	E  several waits                        pending 0, queued 3, sourcing 8, queued 10,
//	                                        sourcing 20, dispatched 25                       22
//	F  fleet refusal and re-dispatch        pending 0, sourcing 1, dispatched 6,
//	                                        sourcing 7, dispatched 40                        39
//	G  never dispatched                     pending 0, queued 2, cancelled 50                not counted
//	H  a retrieve, not retrieve_empty       pending 0, queued 1, dispatched 90               not counted
//	I  another payload                      pending 0, queued 0, dispatched 500              not counted (PART-A)
//
// Mean over A, B, C, E, F = 111 / 5 = 22.2.
//
// D is the one to watch at the cutover: born `pending` and dispatched with no
// acquiring row, it is invisible today, and a from-set that includes `pending`
// counts it (and shortens A and B's start). F shows the MAX(dispatched) choice:
// a demoted order is measured to its LAST dispatch.
func TestAvgL1QueueSeconds_EveryRung(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	win := orders.LeadTimeRange{Start: base.Add(-time.Hour), End: base.Add(2 * time.Hour)}

	type ev struct {
		status protocol.Status
		off    int // seconds from base
	}
	seed := func(uuid, orderType, payload string, evs ...ev) {
		o := &domain.Order{
			EdgeUUID: uuid, StationID: "line-1", OrderType: protocol.OrderType(orderType), Status: evs[0].status,
			Quantity: 1, PayloadCode: payload, DeliveryNode: "D", SourceNode: "S",
		}
		testutil.MustNoErr(t, orders.Create(db, o), "create "+uuid)
		var id int64
		testutil.MustNoErr(t, db.QueryRow(`SELECT id FROM orders WHERE edge_uuid=$1`, uuid).Scan(&id), "id "+uuid)
		// The creation row is stamped at the wall clock; the fixture's history is
		// the one below, so only it is in the window.
		_, err := db.Exec(`DELETE FROM order_history WHERE order_id = $1`, id)
		testutil.MustNoErr(t, err, "clear creation history "+uuid)
		for _, e := range evs {
			_, err := db.Exec(`INSERT INTO order_history (order_id, status, created_at) VALUES ($1,$2,$3)`,
				id, string(e.status), base.Add(time.Duration(e.off)*time.Second))
			testutil.MustNoErr(t, err, "hist "+uuid+" "+string(e.status))
		}
	}
	const (
		pending    = protocol.StatusPending
		queued     = protocol.StatusQueued
		sourcing   = protocol.StatusSourcing
		dispatched = protocol.StatusDispatched
		cancelled  = protocol.StatusCancelled
	)

	seed("A", "retrieve_empty", "PART-A", ev{pending, 0}, ev{queued, 5}, ev{dispatched, 15})
	seed("B", "retrieve_empty", "PART-A", ev{pending, 0}, ev{sourcing, 2}, ev{dispatched, 12})
	seed("C", "retrieve_empty", "PART-A", ev{sourcing, 0}, ev{dispatched, 30})
	seed("D", "retrieve_empty", "PART-A", ev{pending, 0}, ev{dispatched, 4})
	seed("E", "retrieve_empty", "PART-A", ev{pending, 0}, ev{queued, 3}, ev{sourcing, 8}, ev{queued, 10},
		ev{sourcing, 20}, ev{dispatched, 25})
	seed("F", "retrieve_empty", "PART-A", ev{pending, 0}, ev{sourcing, 1}, ev{dispatched, 6},
		ev{sourcing, 7}, ev{dispatched, 40})
	seed("G", "retrieve_empty", "PART-A", ev{pending, 0}, ev{queued, 2}, ev{cancelled, 50})
	seed("H", "retrieve", "PART-A", ev{pending, 0}, ev{queued, 1}, ev{dispatched, 90})
	seed("I", "retrieve_empty", "PART-Z", ev{pending, 0}, ev{queued, 0}, ev{dispatched, 500})

	got, err := orders.AvgL1QueueSeconds(db, "PART-A", win)
	testutil.MustNoErr(t, err, "AvgL1QueueSeconds")
	const want = 22.2
	if math.Abs(got-want) > 0.001 {
		t.Fatalf("AvgL1QueueSeconds = %v, want %v (A 10, B 10, C 30, E 22, F 39 over five orders).\n"+
			"This number becomes every reorder point. If a ladder change moved it, that is a ruling "+
			"for the owner: say which shape moved and by how much, don't re-pin it.", got, want)
	}
}
