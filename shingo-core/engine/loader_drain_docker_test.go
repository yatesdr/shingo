//go:build docker

package engine

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// loader_drain_docker_test.go — an unloader's drain on a running Core: the
// triggers it shares with stage 2's pull (window_pulls.go), a drain's move
// against a full ask at the same window, and the cart SEND BACK leaves in a
// press's outbound group reaching a window. The decision itself is
// dispatch's (dispatch/loader_drain_docker_test.go).

type drainRig struct {
	db      *store.DB
	source  *nodes.Node
	slots   []*nodes.Node
	windows []*nodes.Node
	loader  int64
	tote    *bins.BinType
	seq     int
}

// newDrainRig builds a press's outbound group of `slots` slots and a
// single-station unloader with `windows` windows draining it, saved through
// the loader service (so the save's checks pass it), with the given boxes.
func newDrainRig(t *testing.T, db *store.DB, prefix string, windows, slots int, drains ...protocol.CarrierState) *drainRig {
	t.Helper()
	setupTestData(t, db)
	r := &drainRig{db: db}
	grp, err := db.CreateNodeGroup(prefix + "-PRESS-OUT")
	testutil.MustNoErr(t, err, "the press's outbound group")
	testdb.MarkGroup(t, db, grp)
	r.source, err = db.GetNode(grp)
	testutil.MustNoErr(t, err, "read the group")
	for i := 1; i <= slots; i++ {
		s := &nodes.Node{Name: fmt.Sprintf("%s-PRESS-OUT-%d", prefix, i), Enabled: true, ParentID: &grp}
		testutil.MustNoErr(t, db.CreateNode(s), "slot")
		r.slots = append(r.slots, s)
	}
	per := 8
	r.tote = &bins.BinType{Code: prefix + "-TOTE", TotesPerLayer: &per}
	testutil.MustNoErr(t, db.CreateBinType(r.tote), "tote type")
	ls := service.NewLoaderService(db, nil)
	r.loader, err = ls.CreateLoader(service.LoaderCreate{Name: prefix + "-UNL", Role: loaders.RoleConsume,
		InboundSource: r.source.Name, Drains: drains})
	testutil.MustNoErr(t, err, "the unloader, through the save")
	for i := 1; i <= windows; i++ {
		w := &nodes.Node{Name: fmt.Sprintf("%s-W%d", prefix, i), Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(w), "window")
		testutil.MustNoErr(t, ls.SetHome(r.loader, w.ID, "", "", 0), "window")
		r.windows = append(r.windows, w)
	}
	return r
}

// leftover stands a leftover tote cart (a layer and five, no part) on node:
// the cart SEND BACK leaves at a press.
func (r *drainRig) leftover(t *testing.T, nodeID int64) *bins.Bin {
	t.Helper()
	r.seq++
	b := createTestBinAtNode(t, r.db, "", nodeID, fmt.Sprintf("BIN-%s-%d", r.tote.Code, r.seq))
	_, err := r.db.Exec(`UPDATE bins SET bin_type_id=$1, totes_on_cart=13 WHERE id=$2`, r.tote.ID, b.ID)
	testutil.MustNoErr(t, err, "a leftover cart")
	return b
}

// drainMoves returns the core-dr- orders, oldest first.
func drainMoves(t *testing.T, db *store.DB) []*orders.Order {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM orders WHERE edge_uuid LIKE $1 ORDER BY id`, dispatch.DrainEdgeUUIDPrefix+"%")
	testutil.MustNoErr(t, err, "list core-dr orders")
	defer rows.Close()
	var out []*orders.Order
	for rows.Next() {
		var id int64
		testutil.MustNoErr(t, rows.Scan(&id), "scan")
		o, err := db.GetOrder(id)
		testutil.MustNoErr(t, err, "order")
		out = append(out, o)
	}
	testutil.MustNoErr(t, rows.Err(), "rows")
	return out
}

// awaitDrainMoves polls for n core-dr- orders: the triggers drain on a goroutine.
func awaitDrainMoves(t *testing.T, db *store.DB, n int) []*orders.Order {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := drainMoves(t, db)
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// awaitDispatched polls an order until the fleet holds it.
func awaitDispatched(t *testing.T, db *store.DB, o *orders.Order) *orders.Order {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); o.VendorOrderID == "" && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		got, err := db.GetOrder(o.ID)
		testutil.MustNoErr(t, err, "order")
		o = got
	}
	return o
}

// TestDrain_FiresOnPickupOnArrivalAndAtStartup: stage 2's three triggers run
// the drain too. A leftover cart waiting at startup is drained by the sweep; a
// bin lifted off a full window frees it and drains; a cart arriving in the
// source by an order is drained.
//
// MUTATION: drop the drains loop from sweepWindowPulls, the drains lookup from
// onWindowLeft, or the drains loop from pullOnArrival, and its row goes red.
func TestDrain_FiresOnPickupOnArrivalAndAtStartup(t *testing.T) {
	t.Parallel()

	t.Run("startup", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		r := newDrainRig(t, db, "DRT1", 1, 1, protocol.CarrierLeftover)
		r.leftover(t, r.slots[0].ID)
		newTestEngine(t, db, simulator.New())
		if got := awaitDrainMoves(t, db, 1); len(got) != 1 {
			t.Errorf("startup sweep drained %d carts, want 1", len(got))
		}
	})

	t.Run("pickup frees a window", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		storage, _, _ := setupTestData(t, db)
		r := newDrainRig(t, db, "DRT2", 1, 1, protocol.CarrierLeftover)
		sitting := createTestBinAtNode(t, db, "", r.windows[0].ID, "BIN-DRT2-SITTING")
		r.leftover(t, r.slots[0].ID)
		eng := newTestEngine(t, db, simulator.New())
		time.Sleep(300 * time.Millisecond) // the startup sweep finds no free window
		if got := drainMoves(t, db); len(got) != 0 {
			t.Fatalf("drains before the pickup = %d, want 0", len(got))
		}
		_, err := db.Exec(`UPDATE bins SET node_id=$1 WHERE id=$2`, storage.ID, sitting.ID)
		testutil.MustNoErr(t, err, "the crew's CLEAR carries the cart off")
		eng.Events.Emit(Event{Type: EventBinEnteredTransit, Payload: BinEnteredTransitEvent{
			BinID: sitting.ID, FromNodeID: r.windows[0].ID,
		}})
		if got := awaitDrainMoves(t, db, 1); len(got) != 1 {
			t.Errorf("a pickup from a draining unloader's window drained %d carts, want 1", len(got))
		}
	})

	t.Run("cart arrives in the source", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		r := newDrainRig(t, db, "DRT3", 1, 1, protocol.CarrierLeftover)
		eng := newTestEngine(t, db, simulator.New())
		time.Sleep(300 * time.Millisecond) // the startup sweep finds no cart
		r.leftover(t, r.slots[0].ID)
		arrived := &orders.Order{
			EdgeUUID: "drt3-send-back", StationID: "test-station", OrderType: dispatch.OrderTypeMove,
			Status: dispatch.StatusConfirmed, Quantity: 1, DeliveryNode: r.slots[0].Name,
		}
		testutil.MustNoErr(t, db.CreateOrder(arrived), "the press's move into its outbound group")
		eng.Events.Emit(Event{Type: EventOrderCompleted, Payload: OrderCompletedEvent{
			OrderID: arrived.ID, EdgeUUID: arrived.EdgeUUID, StationID: arrived.StationID,
		}})
		if got := awaitDrainMoves(t, db, 1); len(got) != 1 {
			t.Errorf("a cart arriving in the inbound source drained %d carts, want 1", len(got))
		}
	})
}

// TestDrain_SendBackLeftoverCartReachesAWindow is the case W14 was asked for:
// a press whose outbound is a plain group, not the unloader, and the cart SEND
// BACK leaves there — a tote type at a layer and five, no part — with
// "leftovers" ticked on the unloader draining the group. The cart reaches a
// window: the move is dispatched, lands, and Core confirms it (there is no
// receipt to wait for), so the cart stands on the window with no claim and the
// crew's CLEAR can source it.
//
// MUTATION: drop the core-dr- arm from landedReceipt and the move stays
// delivered with the cart still claimed.
func TestDrain_SendBackLeftoverCartReachesAWindow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	r := newDrainRig(t, db, "DRSB", 1, 1, protocol.CarrierLeftover)
	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	time.Sleep(300 * time.Millisecond) // nothing to drain at startup
	cart := r.leftover(t, r.slots[0].ID)
	sentBack := &orders.Order{
		EdgeUUID: "drsb-send-back", StationID: "test-station", OrderType: dispatch.OrderTypeMove,
		Status: dispatch.StatusConfirmed, Quantity: 1, DeliveryNode: r.slots[0].Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(sentBack), "SEND BACK's move into the press's outbound")
	eng.Events.Emit(Event{Type: EventOrderCompleted, Payload: OrderCompletedEvent{
		OrderID: sentBack.ID, EdgeUUID: sentBack.EdgeUUID, StationID: sentBack.StationID,
	}})

	moves := awaitDrainMoves(t, db, 1)
	if len(moves) != 1 || moves[0].BinID == nil || *moves[0].BinID != cart.ID {
		t.Fatalf("drain moves = %v, want the leftover cart %d", moves, cart.ID)
	}
	m := awaitDispatched(t, db, moves[0])
	if m.VendorOrderID == "" {
		t.Fatalf("the drain's move was not dispatched (status %s, %q)", m.Status, m.QueueReason)
	}
	sim.DriveSimpleLifecycle(m.VendorOrderID)
	if o := awaitOrderStatus(t, db, m.ID, dispatch.StatusConfirmed); o.Status != dispatch.StatusConfirmed {
		t.Fatalf("the drain's move is %s after landing, want confirmed: nobody else confirms it", o.Status)
	}
	got, err := db.GetBin(cart.ID)
	testutil.MustNoErr(t, err, "the cart after landing")
	if got.NodeID == nil || *got.NodeID != r.windows[0].ID {
		t.Errorf("the cart is at %v, want the window %s", got.NodeID, r.windows[0].Name)
	}
	if got.ClaimedBy != nil {
		t.Errorf("the cart is still claimed by order %d: the crew's CLEAR could not source it", *got.ClaimedBy)
	}
}

// TestDrain_AndAFullAskNeverShareAWindow: the drain takes the one free window
// first; its move is acquired before DrainLoader returns, under the loader's
// lock, and holds its claim, so the full ask the next release makes at once
// finds every window taken and orders nothing — it waits for the next window,
// as it does when the windows are full. Once the drained cart has landed and
// is cleared off, the full ask takes the window. (The full ask first is
// dispatch's TestDrain_AFullAskFirstKeepsTheWindow.)
//
// MUTATION: drop windowMove's acquire, and the full ask is born for the window
// the drained cart is to go to. (Moving the acquire after the unlock is not
// seen by a test that asks in sequence; it is the named residual's width.)
func TestDrain_AndAFullAskNeverShareAWindow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storage, _, part := setupTestData(t, db)
	r := newDrainRig(t, db, "DRW", 1, 1, protocol.CarrierLeftover)
	testutil.MustNoErr(t, db.UpsertLoaderPayload(loaders.Payload{LoaderID: r.loader, PayloadCode: part.Code}), "the part it serves")
	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	time.Sleep(300 * time.Millisecond) // the startup sweep finds no cart
	cart := r.leftover(t, r.slots[0].ID)

	n, err := eng.dispatcher.DrainLoader(r.loader)
	testutil.MustNoErr(t, err, "the drain")
	moves := drainMoves(t, db)
	if n != 1 || len(moves) != 1 {
		t.Fatalf("the drain made %d move(s), want 1", len(moves))
	}
	m := moves[0]
	fullAsk := func() dispatch.ReplenishResult {
		t.Helper()
		cfg, ok, err := eng.dispatcher.LoadReplenishConfig(r.loader)
		if err != nil || !ok {
			t.Fatalf("config: ok=%v err=%v", ok, err)
		}
		res, err := eng.dispatcher.ReplenishLoader(dispatch.ReplenishRequest{
			Origin: dispatch.ReplenishBySideCycle, OrderType: dispatch.OrderTypeRetrieve, Want: 1,
			StationID: "test-station", LoaderID: r.loader, Payloads: []string{part.Code},
		}, cfg)
		testutil.MustNoErr(t, err, "a release's full ask")
		return res
	}
	res := fullAsk()
	if len(res.Created) != 0 {
		t.Fatalf("the full ask made %d order(s) to a window the drained cart is on its way to", len(res.Created))
	}
	if _, held := res.Parts[0].HeldBy[r.windows[0].Name]; !held {
		t.Errorf("the full ask's outcome %+v does not name the taken window", res.Parts[0])
	}
	live, err := db.CountActiveOrdersByDeliveryNode(r.windows[0].Name)
	testutil.MustNoErr(t, err, "orders to the window")
	if live != 1 {
		t.Errorf("%d live orders deliver to the one window, want 1 (the drain's)", live)
	}

	// The drained cart lands, and the crew clears it off: the window is free
	// and the next full ask takes it.
	if m = awaitDispatched(t, db, m); m.VendorOrderID == "" {
		t.Fatalf("the drain's move was not dispatched (status %s)", m.Status)
	}
	sim.DriveSimpleLifecycle(m.VendorOrderID)
	if o := awaitOrderStatus(t, db, m.ID, dispatch.StatusConfirmed); o.Status != dispatch.StatusConfirmed {
		t.Fatalf("the drain's move is %s after landing, want confirmed", o.Status)
	}
	_, err = db.Exec(`UPDATE bins SET node_id=$1 WHERE id=$2`, storage.ID, cart.ID)
	testutil.MustNoErr(t, err, "the crew clears the cart off")
	if res := fullAsk(); len(res.Created) != 1 || res.Created[0].DeliveryNode != r.windows[0].Name {
		t.Errorf("with the window free again the full ask made %+v, want one full to %s", res.Created, r.windows[0].Name)
	}
}
