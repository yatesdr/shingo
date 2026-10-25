//go:build docker

package engine

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/messaging"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// A STATION'S MOVE FOR A BIN A ROBOT HOLDS IS REFUSED, NAMING THE HOLDER.
//
// The keep-staged sequence from the sequential sim: a station decides the
// spare on its spot is wrong and sends it back by a move, while the
// changeover's own leg has already claimed that spare and is on its way. The
// move is issued for its cart and takes only a pending hold, never a robot's,
// so it is refused at its issue with the holder named, and nothing is written.
func TestNamedBinMove_ARobotsBinIsRefusedNamingTheHolder(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)
	spot := &nodes.Node{Name: "NB-SPOT", Enabled: true}
	back := &nodes.Node{Name: "NB-BACK", Enabled: true}
	for _, n := range []*nodes.Node{spot, back} {
		testutil.MustNoErr(t, db.CreateNode(n), "node "+n.Name)
	}
	spare := createTestBinAtNode(t, db, "", spot.ID, "NB-SPARE")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()
	env.ID = "env-nb"

	// The changeover's leg claims the spare off the spot.
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

	refused := d.RefusalsFor(env, func() {
		d.HandleOrderRequest(env, &protocol.OrderRequest{
			OrderUUID: "nb-return", OrderType: protocol.OrderTypeMove, Quantity: 1,
			SourceNode: spot.Name, DeliveryNode: back.Name, BinID: spare.ID,
		})
	})
	if len(refused) != 1 || refused[0].ErrorCode != string(protocol.TermClaimFailed) ||
		refused[0].Detail != fmt.Sprintf("bin NB-SPARE is held by order %d", leg.ID) {
		t.Fatalf("refusals = %+v, want one claim_failed naming order %d", refused, leg.ID)
	}
	if o, err := db.GetOrderByUUID("nb-return"); err == nil && o != nil {
		t.Errorf("a move for a robot's bin was issued: order %d (%s)", o.ID, o.Status)
	}
}

// A MOVE PARKED ON A FULL OUTBOUND HOLDS ITS CART, AND LIFTS NOTHING ONCE IT
// HAS LEFT.
//
// The move's drop is occupied, so its gate parks it; it holds its cart all the
// while (issued for it). The cart is then taken off the spot by hand, outside
// any order, and a refill lands there. When the drop frees, the move must not
// carry the refill away: its hand's claim takes the bin it is for only while it
// stands on the source, so the move ends skipped.
func TestNamedBinMove_LiftsNothingOnceItsBinHasLeft(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	spot := &nodes.Node{Name: "NBP-SPOT", Enabled: true}
	back := &nodes.Node{Name: "NBP-BACK", Enabled: true}
	aside := &nodes.Node{Name: "NBP-ASIDE", Enabled: true}
	market := &nodes.Node{Name: "NBP-MARKET", Enabled: true}
	for _, n := range []*nodes.Node{spot, back, aside, market} {
		testutil.MustNoErr(t, db.CreateNode(n), "node "+n.Name)
	}
	spare := createTestBinAtNode(t, db, "", spot.ID, "NBP-SPARE")
	blocking := createTestBinAtNode(t, db, "", back.ID, "NBP-ON-BACK")
	refill := createTestBinAtNode(t, db, "", market.ID, "NBP-REFILL")
	eng := newTestEngine(t, db, simulator.New())

	eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "nbp-return", OrderType: protocol.OrderTypeMove, Quantity: 1,
		SourceNode: spot.Name, DeliveryNode: back.Name, BinID: spare.ID,
	})
	ret := testdb.RequireOrder(t, db, "nbp-return")
	if ret.Status != protocol.StatusQueued || ret.VendorOrderID != "" {
		t.Fatalf("fixture: the return is %q (vendor %q), want parked on its full drop", ret.Status, ret.VendorOrderID)
	}
	var held int64
	testutil.MustNoErr(t, db.QueryRow(`SELECT order_id FROM reservations WHERE bin_id=$1 AND state='pending'`,
		spare.ID).Scan(&held), "the spare's hold")
	if held != ret.ID {
		t.Fatalf("the parked return holds nothing on its cart (holder %d): a move parked on its gate keeps its cart", held)
	}

	// By hand, outside any order: the spare goes aside, the refill lands on the
	// spot, and the drop frees.
	for _, mv := range []struct{ bin, node int64 }{{spare.ID, aside.ID}, {refill.ID, spot.ID}, {blocking.ID, market.ID}} {
		_, err := db.Exec(`UPDATE bins SET node_id=$1 WHERE id=$2`, mv.node, mv.bin)
		testutil.MustNoErr(t, err, "move a bin by hand")
	}
	eng.RunFulfillmentScan()

	ret = testdb.RequireOrder(t, db, "nbp-return")
	if ret.Status != protocol.StatusSkipped {
		t.Errorf("the return is %q, want skipped: the bin it was for has left the spot", ret.Status)
	}
	got, err := db.GetBin(refill.ID)
	testutil.MustNoErr(t, err, "refill")
	if got.ClaimedBy != nil || got.NodeID == nil || *got.NodeID != spot.ID {
		t.Errorf("the refill is claimed by %v at node %v, want unclaimed on the spot", got.ClaimedBy, got.NodeID)
	}
	var holds int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM reservations WHERE order_id=$1`, ret.ID).Scan(&holds), "holds")
	if holds != 0 {
		t.Errorf("the return holds %d reservation(s), want none", holds)
	}
}

// TWO BINS ON THE SOURCE: THE MOVE LIFTS ITS OWN. The station names the older
// bin on a spot holding two; the move is issued for that bin, never the other,
// which a find of the spot returns (it ended skipped when the find chose).
func TestNamedBinMove_TwoBinsOnTheSourceLiftsItsOwn(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	spot := &nodes.Node{Name: "NB2B-SPOT", Enabled: true}
	back := &nodes.Node{Name: "NB2B-BACK", Enabled: true}
	for _, n := range []*nodes.Node{spot, back} {
		testutil.MustNoErr(t, db.CreateNode(n), "node "+n.Name)
	}
	named := createTestBinAtNode(t, db, "", spot.ID, "NB2B-NAMED")
	other := createTestBinAtNode(t, db, "", spot.ID, "NB2B-OTHER")
	eng := newTestEngine(t, db, simulator.New())

	eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "nb2b-return", OrderType: protocol.OrderTypeMove, Quantity: 1,
		SourceNode: spot.Name, DeliveryNode: back.Name, BinID: named.ID,
	})
	ret := testdb.RequireOrder(t, db, "nb2b-return")
	if ret.BinID == nil || *ret.BinID != named.ID || ret.VendorOrderID == "" || ret.Status == protocol.StatusSkipped {
		t.Fatalf("the return = %q bin %v vendor %q, want dispatched with its named bin %d", ret.Status, ret.BinID, ret.VendorOrderID, named.ID)
	}
	if got, err := db.GetBin(other.ID); err != nil || got.ClaimedBy != nil {
		t.Errorf("the other bin on the spot is claimed by %v (%v), want left alone", got.ClaimedBy, err)
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

// A STATION'S MOVE FOR A LOCKED BIN IS REFUSED AT ITS ISSUE. The door's one
// read of the bin says it is locked; nothing is written and the station is told
// why.
func TestNamedBinMove_ALockedBinIsRefused(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	spot := &nodes.Node{Name: "NBL-SPOT", Enabled: true}
	back := &nodes.Node{Name: "NBL-BACK", Enabled: true}
	for _, n := range []*nodes.Node{spot, back} {
		testutil.MustNoErr(t, db.CreateNode(n), "node "+n.Name)
	}
	bin := createTestBinAtNode(t, db, "", spot.ID, "NBL-BIN")
	_, err := db.Exec(`UPDATE bins SET locked = true WHERE id = $1`, bin.ID)
	testutil.MustNoErr(t, err, "lock the bin")
	d := newTestEngine(t, db, simulator.New()).Dispatcher()
	env := testEnvelope()
	env.ID = "env-nbl"

	refused := d.RefusalsFor(env, func() {
		d.HandleOrderRequest(env, &protocol.OrderRequest{
			OrderUUID: "nbl-return", OrderType: protocol.OrderTypeMove, Quantity: 1,
			SourceNode: spot.Name, DeliveryNode: back.Name, BinID: bin.ID,
		})
	})
	if len(refused) != 1 || refused[0].ErrorCode != string(protocol.TermNoSourceBin) || refused[0].Detail != "bin NBL-BIN is locked" {
		t.Fatalf("refusals = %+v, want one no_source_bin \"bin NBL-BIN is locked\"", refused)
	}
	if o, err := db.GetOrderByUUID("nbl-return"); err == nil && o != nil {
		t.Errorf("a move for a locked bin was issued: order %d (%s)", o.ID, o.Status)
	}
}

// A STATION'S MOVE HOLDS ITS DROP FROM ITS ISSUE WHEN THE DROP IS ONE CARRIER'S AT
// A TIME (Need.Window, since 2026-10-25): a pending slot row on a storage
// drop at the commit that writes its row, and nothing on a plain node, which its
// acquire holds no more than before. The drop is the one the decision settled
// (plan.Plan.Dest), so the row is on the node the move goes to. An exclusive drop
// another order holds refuses the move at its issue (rule 2): the station is
// told claim_failed and nothing is written.
func TestNamedBinMove_HoldsAnExclusiveDropFromItsIssue(t *testing.T) {
	t.Parallel()
	issue := func(t *testing.T, prefix string, storage bool, held bool) (*store.DB, *nodes.Node, []protocol.OrderError) {
		t.Helper()
		db := testdb.IssueCensusDB(t)
		setupTestData(t, db)
		stor, err := db.GetNodeTypeByCode("STOR")
		testutil.MustNoErr(t, err, "the STOR type")
		spot := &nodes.Node{Name: prefix + "-SPOT", Enabled: true}
		drop := &nodes.Node{Name: prefix + "-DROP", Enabled: true}
		if storage {
			drop.NodeTypeID = &stor.ID
		}
		for _, n := range []*nodes.Node{spot, drop} {
			testutil.MustNoErr(t, db.CreateNode(n), "node "+n.Name)
		}
		if held {
			// A robot's move on its way to the drop holds it (a dispatched order:
			// no scanner pass acts on it, so its row stands while the move is sent).
			stranger := testdb.CreateOrder(t, db, func(o *orders.Order) {
				o.OrderType, o.Status, o.DeliveryNode = protocol.OrderTypeMove, protocol.StatusDispatched, drop.Name
			})
			testutil.MustNoErr(t, db.ReserveSlot(drop.ID, stranger.ID), "a stranger holds the drop")
		}
		spare := createTestBinAtNode(t, db, "", spot.ID, prefix+"-SPARE")
		d := newTestEngine(t, db, simulator.New()).Dispatcher()
		env := testEnvelope()
		env.ID = "env-" + strings.ToLower(prefix)
		refused := d.RefusalsFor(env, func() {
			d.HandleOrderRequest(env, &protocol.OrderRequest{
				OrderUUID: strings.ToLower(prefix) + "-move", OrderType: protocol.OrderTypeMove, Quantity: 1,
				SourceNode: spot.Name, DeliveryNode: drop.Name, BinID: spare.ID,
			})
		})
		return db, drop, refused
	}
	atCommit := func(t *testing.T, db *store.DB, uuid string) testdb.AtCommit {
		t.Helper()
		o := testdb.RequireOrder(t, db, uuid)
		at, _, ok := testdb.IssuedAtCommit(t, db, o.ID)
		if !ok {
			t.Fatalf("no at-commit record for %s", uuid)
		}
		return at
	}

	t.Run("a storage drop is held from the issue", func(t *testing.T) {
		t.Parallel()
		db, drop, refused := issue(t, "NBWS", true, false)
		if len(refused) != 0 {
			t.Fatalf("the move was refused: %+v", refused)
		}
		if at := atCommit(t, db, "nbws-move"); !at.SlotRow {
			t.Errorf("the move held %s at its issue's commit, want %s's slot row", at, drop.Name)
		}
	})
	t.Run("a plain drop is not", func(t *testing.T) {
		t.Parallel()
		db, drop, refused := issue(t, "NBWP", false, false)
		if len(refused) != 0 {
			t.Fatalf("the move was refused: %+v", refused)
		}
		if at := atCommit(t, db, "nbwp-move"); at.SlotRow {
			t.Errorf("the move held %s at its issue's commit: %s is no one carrier's", at, drop.Name)
		}
	})
	t.Run("a storage drop another order holds refuses the issue", func(t *testing.T) {
		t.Parallel()
		db, drop, refused := issue(t, "NBWH", true, true)
		if len(refused) != 1 || refused[0].ErrorCode != string(protocol.TermClaimFailed) ||
			!strings.Contains(refused[0].Detail, drop.Name) {
			t.Fatalf("refusals = %+v, want one claim_failed naming %s", refused, drop.Name)
		}
		if o, err := db.GetOrderByUUID("nbwh-move"); err == nil && o != nil {
			t.Errorf("a move into a held drop was issued: order %d (%s)", o.ID, o.Status)
		}
	})
}
