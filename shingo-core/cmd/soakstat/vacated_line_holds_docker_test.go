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

// TestVacatedLineHolds_APairAtAVacantDependentPosition: a pair leg parked
// reserve-holding at a press fed from behind is counted while the press's
// dependent position (its active claim's paired position) holds no bin, once
// its last history row is older than the bound on the sim clock (the newest
// history row). A bin on the dependent position, a vacant press with its
// dependent position held, a leg with no sibling, or a younger wait is not
// counted.
func TestVacatedLineHolds_APairAtAVacantDependentPosition(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	node := func(name string) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(n), name)
		return n
	}
	press, onDeck, market := node("VLH-PRESS"), node("VLH-ONDECK"), node("VLH-MARKET")
	_, err := db.Exec(`INSERT INTO style_claims (process_id, style_id, core_node_name, role, swap_mode, payload_code, paired_core_node)
		VALUES ('VLH-P', 'VLH-S', $1, 'consume', $2, 'VLH-PART', $3)`, press.Name, string(protocol.SwapModeTwoRobotPressIndex), onDeck.Name)
	testutil.MustNoErr(t, err, "a press fed from behind")
	_, err = db.Exec(`INSERT INTO process_styles (process_id, style_id, station_id, is_active) VALUES ('VLH-P', 'VLH-S', 'vlh-line', true)`)
	testutil.MustNoErr(t, err, "its style, active at the station")
	base := time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)
	leg := func(uuid, sibling string, at time.Time) {
		o := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID, o.SiblingOrderUUID, o.StationID = uuid, sibling, "vlh-line"
			o.OrderType, o.Status, o.ProcessNode = protocol.OrderTypeComplex, protocol.StatusQueued, press.Name
		})
		_, err := db.Exec(`UPDATE orders SET queue_cause = $1 WHERE id = $2`, string(dispatch.CauseReserveHolding), o.ID)
		testutil.MustNoErr(t, err, "cause")
		_, err = db.Exec(`DELETE FROM order_history WHERE order_id = $1`, o.ID)
		testutil.MustNoErr(t, err, "history")
		_, err = db.Exec(`INSERT INTO order_history (order_id, status, detail, created_at) VALUES ($1, 'queued', '', $2)`, o.ID, at)
		testutil.MustNoErr(t, err, "history row")
	}
	leg("vlh-a", "vlh-b", base)
	leg("vlh-b", "vlh-a", base.Add(25*time.Minute)) // the sim clock: 25 sim-min on
	leg("vlh-solo", "", base)
	count := func(when string, want int) {
		t.Helper()
		n, oldest, err := vacatedLineHolds(db)
		testutil.MustNoErr(t, err, when)
		if n != want || (want > 0 && oldest < 24.9) {
			t.Errorf("%s: vacated-line holds %d (oldest %.1fm), want %d", when, n, oldest, want)
		}
	}

	line := testdb.CreateBinAtNode(t, db, "VLH-PART", press.ID, "VLH-LINE-BIN")
	count("the press holds its cart, the dependent position none: the old leg alone, at 25m", 1)

	testdb.CreateBinAtNode(t, db, "VLH-PART", onDeck.ID, "VLH-ONDECK-BIN")
	count("a bin on the dependent position", 0)

	_, err = db.Exec(`UPDATE bins SET node_id = $1 WHERE id = $2`, market.ID, line.ID)
	testutil.MustNoErr(t, err, "the press's cart lifted")
	count("the press vacant, its dependent position held", 0)
}
