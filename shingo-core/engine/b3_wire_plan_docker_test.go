//go:build docker

package engine

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// B3 · D1: acquiring reads the wire door's birth pick (orders.plan, v147) in
// place of a second pick, and picks again only when the plan no longer stands
// (SYNTH F4).
//
// The plant: two bins of one payload at two storage nodes, the older one first
// in FIFO order. A retrieve with no named source picks plant-wide, so a pick
// takes the older bin. Each case plans the NEWER one, so which bin leaves says
// whether the scanner took the plan or picked.

type planPlant struct {
	db             *store.DB
	eng            *Engine
	older, newer   *bins.Bin
	olderN, newerN *nodes.Node
	line           *nodes.Node
	payload        string
}

func newPlanPlant(t *testing.T) planPlant {
	t.Helper()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	mk := func(name string) *nodes.Node {
		n := &nodes.Node{Name: name, Zone: "A", Enabled: true, NodeTypeID: sd.StorageNode.NodeTypeID}
		testutil.MustNoErr(t, db.CreateNode(n), "create "+name)
		return n
	}
	p := planPlant{db: db, olderN: mk("B3P-STOR-OLD"), newerN: mk("B3P-STOR-NEW"), line: sd.LineNode, payload: sd.Payload.Code}
	p.older = testdb.CreateBinAtNode(t, db, p.payload, p.olderN.ID, "B3P-BIN-OLD")
	p.newer = testdb.CreateBinAtNode(t, db, p.payload, p.newerN.ID, "B3P-BIN-NEW")
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []int64{p.older.ID, p.newer.ID} {
		_, err := db.Exec(`UPDATE bins SET loaded_at=$1 WHERE id=$2`, base.Add(time.Duration(i)*time.Hour), id)
		testutil.MustNoErr(t, err, "order the bins for FIFO")
	}
	newer, err := db.GetBin(p.newer.ID)
	testutil.MustNoErr(t, err, "reload the newer bin")
	p.newer = newer
	p.eng = newTestEngine(t, db, testdb.NewTrackingBackend())
	return p
}

// planned writes a queued retrieve born with a plan naming the newer bin, as
// the wire door writes it.
func (p planPlant) planned(t *testing.T) *orders.Order {
	t.Helper()
	return testdb.CreateOrder(t, p.db, func(o *orders.Order) {
		o.EdgeUUID = fmt.Sprintf("b3-plan-%s", t.Name())
		o.OrderType = dispatch.OrderTypeRetrieve
		o.SourceIntent = dispatch.SourceIntentForType(dispatch.OrderTypeRetrieve)
		o.Status = protocol.StatusQueued
		o.PayloadCode = p.payload
		o.DeliveryNode = p.line.Name
		o.Plan = &domain.PlannedPick{BinID: p.newer.ID, Node: p.newerN.Name, Payload: p.newer.PayloadCode,
			Status: string(p.newer.Status), Confirmed: p.newer.ManifestConfirmed}
	})
}

func (p planPlant) dispatchedBin(t *testing.T, id int64) int64 {
	t.Helper()
	p.eng.RunFulfillmentScan()
	o, err := p.db.GetOrder(id)
	testutil.MustNoErr(t, err, "reload order")
	if o.Status != protocol.StatusDispatched && o.Status != protocol.StatusInTransit {
		t.Fatalf("order status = %s (%s), want dispatched", o.Status, o.QueueReason)
	}
	if o.BinID == nil {
		t.Fatal("dispatched with no bin")
	}
	return *o.BinID
}

// A standing plan is taken: the planned bin leaves, not the one a pick takes.
func TestB3D1_ScannerTakesAStandingPlan(t *testing.T) {
	p := newPlanPlant(t)
	o := p.planned(t)
	if got := p.dispatchedBin(t, o.ID); got != p.newer.ID {
		t.Errorf("dispatched bin %d, want the planned bin %d (the older %d is what a pick takes)", got, p.newer.ID, p.older.ID)
	}
}

// A plan whose bin another order holds is stale: the scanner picks again at
// once rather than parking on a bin it cannot reserve.
func TestB3D1_ScannerRepicksWhenThePlannedBinIsHeld(t *testing.T) {
	p := newPlanPlant(t)
	// The holder is past acquiring, so no pass touches it and its hold stays.
	holder := testdb.CreateOrder(t, p.db, func(o *orders.Order) { o.Status = protocol.StatusDispatched })
	testutil.MustNoErr(t, reservations.Acquire(p.db, holder.ID, holder.ID, p.newer.ID, "b3-holder"), "hold the planned bin")
	o := p.planned(t)
	if got := p.dispatchedBin(t, o.ID); got != p.older.ID {
		t.Errorf("dispatched bin %d, want the older bin %d (the planned one is held)", got, p.older.ID)
	}
}

// A plan whose bin has moved is stale: the premise is the bin at the planned
// node, so the scanner picks again rather than sending a robot to where the
// bin was.
func TestB3D1_ScannerRepicksWhenThePlannedBinMoved(t *testing.T) {
	p := newPlanPlant(t)
	elsewhere := &nodes.Node{Name: "B3P-STOR-ELSEWHERE", Zone: "A", Enabled: true, NodeTypeID: p.olderN.NodeTypeID}
	testutil.MustNoErr(t, p.db.CreateNode(elsewhere), "create a third node")
	_, err := p.db.Exec(`UPDATE bins SET node_id=$1 WHERE id=$2`, elsewhere.ID, p.newer.ID)
	testutil.MustNoErr(t, err, "move the planned bin")
	o := p.planned(t)
	if got := p.dispatchedBin(t, o.ID); got != p.older.ID {
		t.Errorf("dispatched bin %d, want the older bin %d (the planned one moved)", got, p.older.ID)
	}
}

// B3 · D3: a carried-bin recovery is born through Build, and a door whose
// station has no row writes a station-unaware row (v145, the orc's B2 Stop 1
// and 2 rulings). Recovery names no station, so it takes pending -> dispatched
// with no `queued` rung, and sends no station anything: before the fold its
// notices went to station "", which reaches every Edge.
func TestB3D3_RecoveryIsStationUnaware(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())
	_, binID := declaredChildFor(t, eng, db, "B3R", true)
	var outboxBefore int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM outbox`).Scan(&outboxBefore), "outbox mark")

	order, _, err := eng.RecoverCarriedBin(binID, "operator:test")
	testutil.MustNoErr(t, err, "recover carried bin")
	o, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload order")
	if !o.StationUnaware {
		t.Error("recovery row is station-aware; it names no station, so no station has a row for it")
	}
	hist, err := db.ListOrderHistory(order.ID)
	testutil.MustNoErr(t, err, "history")
	for _, h := range hist {
		if h.Status == protocol.StatusQueued {
			t.Errorf("recovery wrote a queued rung (%q); a station-unaware order goes pending -> dispatched", h.Detail)
		}
	}
	var toBlank int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM outbox WHERE id > $1 AND COALESCE(station_id, '') = ''`,
		outboxBefore).Scan(&toBlank), "notices to no station")
	if toBlank != 0 {
		t.Errorf("recovery sent %d notice(s) to station \"\"", toBlank)
	}
}
