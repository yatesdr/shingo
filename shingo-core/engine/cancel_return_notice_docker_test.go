//go:build docker

package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// binReturnNotices returns every BinReturn in the outbox for one cancelled
// order, in send order. The outbox is the seam: SendDataToEdge enqueues there.
func binReturnNotices(t *testing.T, db *store.DB, orderUUID string) (out []protocol.BinReturn, stations []string) {
	t.Helper()
	rows, err := db.DB.Query(`SELECT payload, station_id FROM outbox ORDER BY id`)
	testutil.MustNoErr(t, err, "read outbox")
	defer rows.Close()
	for rows.Next() {
		var payload []byte
		var station string
		if rows.Scan(&payload, &station) != nil {
			continue
		}
		var env struct {
			P struct {
				Subject string          `json:"subject"`
				Data    json.RawMessage `json:"data"`
			} `json:"p"`
		}
		if json.Unmarshal(payload, &env) != nil || env.P.Subject != protocol.SubjectBinReturn {
			continue
		}
		var br protocol.BinReturn
		if json.Unmarshal(env.P.Data, &br) == nil && br.OrderUUID == orderUUID {
			out = append(out, br)
			stations = append(stations, station)
		}
	}
	return out, stations
}

// The station that cancelled the order hears what became of its bin:
// returning at dispatch, then returned when the return completes.
func TestCancelReturnNotice_ReturningThenReturned(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	g, slot := storeGroupWithSlot(t, db, "CRN")
	seedClaim(t, db, "PROC-CRN", "STY", "LINE-CRN", "CRN-P", g.Name, true)
	bin, carrier := seedCancelledCarry(t, db, "AMR-CRN", "CRN-P", "LINE-CRN")
	cacheRobot(eng, loadedDispatchable("AMR-CRN"))
	eng.sweepCarriedBins()
	ret := theReturn(t, db, bin.ID)

	got, stations := binReturnNotices(t, db, carrier.EdgeUUID)
	if len(got) != 1 || got[0].State != protocol.BinReturnReturning || got[0].Destination != slot.Name ||
		got[0].BinLabel != bin.Label || stations[0] != carrier.StationID {
		t.Fatalf("notices after dispatch = %+v to %v, want one 'returning' to %s for %s", got, stations, carrier.StationID, slot.Name)
	}

	// Through the real lifecycle: delivered (the bin is down), then confirmed.
	// Both fire EventOrderCompleted; "returned" is sent once.
	live, err := db.GetOrder(ret.ID)
	testutil.MustNoErr(t, err, "re-read the return")
	testutil.MustNoErr(t, eng.dispatcher.Lifecycle().MarkDelivered(live, "test"), "deliver the return")
	live, err = db.GetOrder(ret.ID)
	testutil.MustNoErr(t, err, "re-read the delivered return")
	_, err = eng.dispatcher.Lifecycle().ConfirmReceipt(live, "", "auto", 0)
	testutil.MustNoErr(t, err, "confirm the return")
	got, _ = binReturnNotices(t, db, carrier.EdgeUUID)
	if len(got) != 2 || got[1].State != protocol.BinReturnReturned || got[1].Destination != slot.Name {
		t.Errorf("notices after completion = %+v, want returning then returned to %s", got, slot.Name)
	}
}

// A return that ends without placing the bin is a hold, naming why.
func TestCancelReturnNotice_AFailedReturnIsAHold(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	g, _ := storeGroupWithSlot(t, db, "CRNF")
	seedClaim(t, db, "PROC-CRNF", "STY", "LINE-CRNF", "CRNF-P", g.Name, true)
	bin, carrier := seedCancelledCarry(t, db, "AMR-CRNF", "CRNF-P", "LINE-CRNF")
	cacheRobot(eng, loadedDispatchable("AMR-CRNF"))
	eng.sweepCarriedBins()
	ret := theReturn(t, db, bin.ID)

	eng.failOrderAndEmit(ret.ID, "fleet_failed", "fleet refused")
	got, _ := binReturnNotices(t, db, carrier.EdgeUUID)
	if len(got) != 2 || got[1].State != protocol.BinReturnHeld || got[1].Reason == "" {
		t.Errorf("notices = %+v, want returning then held with a reason", got)
	}
}

// No return made: the station hears held, with the hold's sentence.
func TestCancelReturnNotice_AHoldIsNamed(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	bin, carrier := seedCancelledCarry(t, db, "AMR-CRNH", "CRNH-NOCLAIM", "LINE-CRNH")
	cacheRobot(eng, loadedDispatchable("AMR-CRNH"))
	eng.sweepCarriedBins()
	assertNoRecoveryOrder(t, db, bin.ID)

	got, _ := binReturnNotices(t, db, carrier.EdgeUUID)
	if len(got) != 1 || got[0].State != protocol.BinReturnHeld || got[0].Reason == "" {
		t.Errorf("notices = %+v, want one held with the reason", got)
	}
}

// AN ORDINARY ORDER'S TERMINAL EVENT COSTS returnOrderEnded NOTHING. The
// payload carries recovers_order_id, so an order that returns no bin is turned
// away before any read — counted in statements, not timed. The return order's
// own call is counted too, so the zero is a counter that was watching.
func TestCancelReturnNotice_AnOrdinaryOrderEndingReadsNothing(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	ordinary := &orders.Order{EdgeUUID: "crn-ordinary", StationID: "edge.test", OrderType: "retrieve",
		Status: protocol.StatusConfirmed, Quantity: 1}
	testutil.MustNoErr(t, db.CreateOrder(ordinary), "an ordinary order")
	cancelled := &orders.Order{EdgeUUID: "crn-cancelled", StationID: "edge.test", OrderType: "retrieve",
		Status: protocol.StatusCancelled, Quantity: 1}
	testutil.MustNoErr(t, db.CreateOrder(cancelled), "a cancelled order")
	ret := &orders.Order{EdgeUUID: "crn-return", OrderType: "move", Status: protocol.StatusDelivered,
		Quantity: 1, RecoversOrderID: &cancelled.ID}
	testutil.MustNoErr(t, db.CreateOrder(ret), "its return")

	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open counting db")
	t.Cleanup(func() { cdb.Close() })
	eng := newUnstartedEngine(t, cdb, simulator.New())

	counter.Reset()
	eng.returnOrderEnded(ordinary.ID, nil, protocol.BinReturnReturned, "")
	if got := counter.Count(); got != 0 {
		t.Errorf("an ordinary order's terminal event cost %d statement(s) in returnOrderEnded, want 0", got)
	}

	counter.Reset()
	eng.returnOrderEnded(ret.ID, ret.RecoversOrderID, protocol.BinReturnReturned, "")
	if counter.Count() == 0 {
		t.Error("the return order's terminal event read nothing — the counter is not watching this engine")
	}
}

// A DIG LEG IS CORE'S OWN ORDER: its uuid is minted by Core and the Edge has no
// row for it, so a notice keyed on it is stored at the station and shown on no
// order. The notice goes to the nearest ancestor, the order the Edge placed,
// whose station row shows it under the cancelled order.
func TestCancelReturnNotice_ADigLegsNoticeGoesToTheOrderTheEdgePlaced(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	parent := &orders.Order{EdgeUUID: "crnd-parent", StationID: "edge.test", OrderType: "retrieve",
		Status: protocol.StatusCancelled, Quantity: 1}
	testutil.MustNoErr(t, db.CreateOrder(parent), "the order the Edge placed")
	bin, leg := seedCancelledCarry(t, db, "AMR-CRND", "CRND-NOCLAIM", "LINE-CRND")
	_, err := db.DB.Exec(`UPDATE orders SET parent_order_id=$1, station_id=$2 WHERE id=$3`,
		parent.ID, parent.StationID, leg.ID)
	testutil.MustNoErr(t, err, "make the carrier a dig leg of the parent")
	cacheRobot(eng, loadedDispatchable("AMR-CRND"))
	eng.sweepCarriedBins()
	assertNoRecoveryOrder(t, db, bin.ID)

	if got, _ := binReturnNotices(t, db, leg.EdgeUUID); len(got) != 0 {
		t.Errorf("notices keyed on the dig leg's own uuid = %+v, want none: the Edge has no row for it", got)
	}
	got, stations := binReturnNotices(t, db, parent.EdgeUUID)
	if len(got) != 1 || got[0].State != protocol.BinReturnHeld || got[0].Reason == "" ||
		stations[0] != parent.StationID {
		t.Errorf("notices keyed on the parent = %+v to %v, want one held with the reason to %s",
			got, stations, parent.StationID)
	}
}
