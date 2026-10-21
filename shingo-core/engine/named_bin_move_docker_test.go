//go:build docker

package engine

import (
	"database/sql"
	"reflect"
	"sync/atomic"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/messaging"
	"shingocore/service"
	"shingocore/store/nodes"
)

// A MOVE THAT NAMES ITS BIN LIFTS THAT BIN OR NOTHING.
//
// The keep-staged sequence from the sequential sim: a station decides the
// spare on its spot is wrong and sends it back by a move; the changeover's own
// leg lifts that spare first; the refill lands on the spot; the move, still
// waiting, must not carry the fresh refill away. It names the bin it was for,
// and once that bin has left the spot it ends skipped.
func TestNamedBinMove_LiftsNothingOnceItsBinHasLeft(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)
	spot := &nodes.Node{Name: "NB-SPOT", Enabled: true}
	back := &nodes.Node{Name: "NB-BACK", Enabled: true}
	market := &nodes.Node{Name: "NB-MARKET", Enabled: true}
	for _, n := range []*nodes.Node{spot, back, market} {
		testutil.MustNoErr(t, db.CreateNode(n), "node "+n.Name)
	}
	spare := createTestBinAtNode(t, db, "", spot.ID, "NB-SPARE")
	refill := createTestBinAtNode(t, db, "", market.ID, "NB-REFILL")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// The changeover's leg lifts the spare off the spot.
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID: "nb-leg", Quantity: 1,
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionPickup, Node: spot.Name},
			{Action: protocol.ActionDropoff, Node: lineNode.Name},
		},
	})
	leg := testdb.RequireOrder(t, db, "nb-leg")
	if leg.BinID == nil || *leg.BinID != spare.ID {
		t.Fatalf("fixture: the leg claimed %v, want the spare %d", leg.BinID, spare.ID)
	}
	sim.DriveState(leg.VendorOrderID, "RUNNING")

	// The return for that spare arrives while the leg holds it.
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID: "nb-return", OrderType: protocol.OrderTypeMove, Quantity: 1,
		SourceNode: spot.Name, DeliveryNode: back.Name, BinID: spare.ID,
	})
	if o := testdb.RequireOrder(t, db, "nb-return"); o.BinID != nil {
		t.Fatalf("fixture: the return holds bin %d while the leg carries the spare", *o.BinID)
	}

	// The leg delivers the spare to the line, and the refill lands on the spot.
	sim.DriveState(leg.VendorOrderID, "FINISHED")
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{OrderUUID: "nb-leg", ReceiptType: "confirmed", FinalCount: 1})
	testdb.RequireBinAtNode(t, db, spare.ID, lineNode.ID)
	_, err := db.Exec(`UPDATE bins SET node_id=$1 WHERE id=$2`, spot.ID, refill.ID)
	testutil.MustNoErr(t, err, "the refill lands on the spot")

	eng.RunFulfillmentScan()

	ret := testdb.RequireOrder(t, db, "nb-return")
	if ret.Status != protocol.StatusSkipped {
		t.Errorf("the return is %q, want skipped: the bin it was for has left the spot", ret.Status)
	}
	if ret.BinID != nil {
		t.Errorf("the return holds bin %d, want none", *ret.BinID)
	}
	got, err := db.GetBin(refill.ID)
	testutil.MustNoErr(t, err, "refill")
	if got.ClaimedBy != nil || got.NodeID == nil || *got.NodeID != spot.ID {
		t.Errorf("the refill is claimed by %v at node %v, want unclaimed on the spot", got.ClaimedBy, got.NodeID)
	}
	var held int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM reservations WHERE order_id=$1`, ret.ID).Scan(&held), "holds")
	if held != 0 {
		t.Errorf("the return holds %d reservation(s), want none", held)
	}
}

// A move that names its bin, with the bin standing on its source, lifts it.
func TestNamedBinMove_LiftsItsBinWhenItStands(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	spot := &nodes.Node{Name: "NB2-SPOT", Enabled: true}
	back := &nodes.Node{Name: "NB2-BACK", Enabled: true}
	for _, n := range []*nodes.Node{spot, back} {
		testutil.MustNoErr(t, db.CreateNode(n), "node "+n.Name)
	}
	spare := createTestBinAtNode(t, db, "", spot.ID, "NB2-SPARE")
	eng := newTestEngine(t, db, simulator.New())

	eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "nb2-return", OrderType: protocol.OrderTypeMove, Quantity: 1,
		SourceNode: spot.Name, DeliveryNode: back.Name, BinID: spare.ID,
	})

	ret := testdb.RequireOrder(t, db, "nb2-return")
	if ret.BinID == nil || *ret.BinID != spare.ID || ret.VendorOrderID == "" {
		t.Fatalf("the return = status %q bin %v vendor %q, want dispatched with the spare %d",
			ret.Status, ret.BinID, ret.VendorOrderID, spare.ID)
	}
}

// holdOfBin is a bin's hold marker as stored: the flag and who set it.
func holdOfBin(t *testing.T, db interface {
	QueryRow(string, ...any) *sql.Row
}, binID int64) (bool, string) {
	t.Helper()
	var hold bool
	var by string
	testutil.MustNoErr(t, db.QueryRow(`SELECT quality_hold, hold_by FROM bins WHERE id=$1`, binID).Scan(&hold, &by), "the hold")
	return hold, by
}

// A STATION'S HOLD IS ONE ACT: the move that takes the bin to containment, the
// check that the bin stands where the station read it, and the hold marker are
// born together (protocol.OrderRequest.QualityHoldBy).
func TestNamedBinMove_HoldMarksTheBinAtBirth(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	spot := &nodes.Node{Name: "QH1-SPOT", Enabled: true}
	hold := &nodes.Node{Name: "QH1-HOLD", Enabled: true}
	for _, n := range []*nodes.Node{spot, hold} {
		testutil.MustNoErr(t, db.CreateNode(n), "node "+n.Name)
	}
	bin := createTestBinAtNode(t, db, "", spot.ID, "QH1-BIN")
	eng := newTestEngine(t, db, simulator.New())
	var pushed atomic.Int32
	eng.SetContainmentChangedFunc(func() { pushed.Add(1) })
	d := eng.Dispatcher()
	env := testEnvelope()
	env.ID = "env-qh1"

	refused := d.RefusalsFor(env, func() {
		d.HandleOrderRequest(env, &protocol.OrderRequest{
			OrderUUID: "qh1-move", OrderType: protocol.OrderTypeMove, Quantity: 1,
			SourceNode: spot.Name, DeliveryNode: hold.Name, BinID: bin.ID, QualityHoldBy: "edge.qh",
		})
	})
	if len(refused) != 0 {
		t.Fatalf("the hold was refused: %+v", refused)
	}
	o := testdb.RequireOrder(t, db, "qh1-move")
	if o.NamedBinID == nil || *o.NamedBinID != bin.ID {
		t.Errorf("the move names bin %v, want %d", o.NamedBinID, bin.ID)
	}
	if held, by := holdOfBin(t, db, bin.ID); !held || by != "edge.qh" {
		t.Errorf("the bin's hold = {%v %q}, want held by edge.qh", held, by)
	}
	if pushed.Load() == 0 {
		t.Error("the hold pushed no containment snapshot")
	}
}

// A hold whose bin is not on the source the station read is refused at
// intake with the sentence, and nothing is born: no order, no marker.
func TestNamedBinMove_HoldOfABinNotAtTheSourceIsRefused(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	spot := &nodes.Node{Name: "QH2-SPOT", Enabled: true}
	hold := &nodes.Node{Name: "QH2-HOLD", Enabled: true}
	away := &nodes.Node{Name: "QH2-AWAY", Enabled: true}
	for _, n := range []*nodes.Node{spot, hold, away} {
		testutil.MustNoErr(t, db.CreateNode(n), "node "+n.Name)
	}
	bin := createTestBinAtNode(t, db, "", away.ID, "QH2-BIN")
	eng := newTestEngine(t, db, simulator.New())
	d := eng.Dispatcher()
	env := testEnvelope()
	env.ID = "env-qh2"

	refused := d.RefusalsFor(env, func() {
		d.HandleOrderRequest(env, &protocol.OrderRequest{
			OrderUUID: "qh2-move", OrderType: protocol.OrderTypeMove, Quantity: 1,
			SourceNode: spot.Name, DeliveryNode: hold.Name, BinID: bin.ID, QualityHoldBy: "edge.qh",
		})
	})
	want := protocol.OrderError{OrderUUID: "qh2-move", ErrorCode: string(protocol.TermNoSourceBin),
		Detail: "bin QH2-BIN is not at that source (QH2-SPOT)"}
	if len(refused) != 1 || !reflect.DeepEqual(refused[0], want) {
		t.Fatalf("refusals = %+v, want exactly %+v", refused, want)
	}
	if o, err := db.GetOrderByUUID("qh2-move"); err == nil && o != nil {
		t.Errorf("a refused hold was born: order %d", o.ID)
	}
	if held, by := holdOfBin(t, db, bin.ID); held || by != "" {
		t.Errorf("a refused hold marked the bin: {%v %q}", held, by)
	}
}

// A move naming a bin that is not on its source is refused at intake, hold or
// no hold: nothing is born to wait and end skipped later.
func TestNamedBinMove_ABinNotAtTheSourceIsRefusedAtBirth(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	spot := &nodes.Node{Name: "NB3-SPOT", Enabled: true}
	back := &nodes.Node{Name: "NB3-BACK", Enabled: true}
	away := &nodes.Node{Name: "NB3-AWAY", Enabled: true}
	for _, n := range []*nodes.Node{spot, back, away} {
		testutil.MustNoErr(t, db.CreateNode(n), "node "+n.Name)
	}
	bin := createTestBinAtNode(t, db, "", away.ID, "NB3-BIN")
	eng := newTestEngine(t, db, simulator.New())
	eng.SetBinsRowsFunc(messaging.NewCoreDataService(db, nil, service.EpochAnnounce{}).NodeBinsRows)
	d := eng.Dispatcher()
	env := testEnvelope()
	env.ID = "env-nb3"

	refused := d.RefusalsFor(env, func() {
		d.HandleOrderRequest(env, &protocol.OrderRequest{
			OrderUUID: "nb3-return", OrderType: protocol.OrderTypeMove, Quantity: 1,
			SourceNode: spot.Name, DeliveryNode: back.Name, BinID: bin.ID,
		})
	})
	want := protocol.OrderError{OrderUUID: "nb3-return", ErrorCode: string(protocol.TermNoSourceBin),
		Detail: "bin NB3-BIN is not at that source (NB3-SPOT)",
		// Core's row for the source rides the refusal: the Edge redraws the spot
		// as empty before it shows the sentence.
		Rows: []protocol.NodeBinsRow{{Node: "NB3-SPOT", State: protocol.BinsStateEmpty}}}
	if len(refused) != 1 || !reflect.DeepEqual(refused[0], want) {
		t.Fatalf("refusals = %+v, want exactly %+v", refused, want)
	}
	if o, err := db.GetOrderByUUID("nb3-return"); err == nil && o != nil {
		t.Errorf("a move for a bin not on its source was born: order %d (%s)", o.ID, o.Status)
	}
}
