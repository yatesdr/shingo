//go:build docker

package main

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestVacatedLineHolds_APairSittingAtAnEmptyLine: a pair leg parked
// reserve-holding while its line holds no bin is counted once its last history
// row is older than the bound on the sim clock (the newest history row); a
// bin on the line, a leg with no sibling, or a younger wait is not counted.
func TestVacatedLineHolds_APairSittingAtAnEmptyLine(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	line := &nodes.Node{Name: "VLH-LINE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(line), "line")
	base := time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)
	leg := func(uuid, sibling string, at time.Time) *orders.Order {
		o := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID, o.SiblingOrderUUID = uuid, sibling
			o.OrderType, o.Status, o.ProcessNode = protocol.OrderTypeComplex, protocol.StatusQueued, line.Name
		})
		_, err := db.Exec(`UPDATE orders SET queue_cause = $1 WHERE id = $2`, string(dispatch.CauseReserveHolding), o.ID)
		testutil.MustNoErr(t, err, "cause")
		_, err = db.Exec(`DELETE FROM order_history WHERE order_id = $1`, o.ID)
		testutil.MustNoErr(t, err, "history")
		_, err = db.Exec(`INSERT INTO order_history (order_id, status, detail, created_at) VALUES ($1, 'queued', '', $2)`, o.ID, at)
		testutil.MustNoErr(t, err, "history row")
		return o
	}
	leg("vlh-a", "vlh-b", base)
	leg("vlh-b", "vlh-a", base.Add(25*time.Minute)) // the sim clock: 25 sim-min on
	leg("vlh-solo", "", base)

	n, oldest, err := vacatedLineHolds(db)
	testutil.MustNoErr(t, err, "the count")
	if n != 1 || oldest < 24.9 {
		t.Errorf("vacated-line holds %d (oldest %.1fm), want 1 at 25m: the old leg alone", n, oldest)
	}

	testdb.CreateBinAtNode(t, db, "VLH-PART", line.ID, "VLH-BIN")
	if n, _, err := vacatedLineHolds(db); err != nil || n != 0 {
		t.Errorf("with a bin on the line: %d (%v), want 0", n, err)
	}
}
