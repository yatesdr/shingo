//go:build docker

package engine

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// stage2_pull_docker_test.go — Core moves the named bare cart from the wait
// group into a free stage-2 window (engine/stage2_pull.go). The Edge does
// nothing new: the move names its bin, so it goes straight to dispatch and
// never reaches an empty finder, and it lands the way any Core-authored move
// lands (TestStage2Pull_LandedMoveConfirmsAndFreesItsClaim).

type s2Pair struct {
	stage1, stage2 int64
	windows        []*nodes.Node
}

type s2Fixture struct {
	db      *store.DB
	loaders *service.LoaderService
	group   *nodes.Node
	slots   []*nodes.Node
	carrier *bins.BinType
	marker  *bins.BinType
	seq     int
}

// newS2Fixture builds a wait group of `slots` plain slots and one cart type
// with its marker. No engine: callers pick a started or unstarted one.
func newS2Fixture(t *testing.T, db *store.DB, prefix string, slots int) *s2Fixture {
	t.Helper()
	f := &s2Fixture{db: db, loaders: service.NewLoaderService(db, nil)}
	grpID, err := db.CreateNodeGroup(prefix + "-WAIT")
	testutil.MustNoErr(t, err, "wait group")
	f.group, err = db.GetNode(grpID)
	testutil.MustNoErr(t, err, "read wait group")
	for i := 1; i <= slots; i++ {
		s := &nodes.Node{Name: fmt.Sprintf("%s-WAIT-%d", prefix, i), Enabled: true, ParentID: &grpID}
		testutil.MustNoErr(t, db.CreateNode(s), "slot")
		f.slots = append(f.slots, s)
	}
	f.carrier, f.marker = s2CartType(t, db, prefix+"-CART")
	return f
}

// s2CartType creates a cart type and its bare marker.
func s2CartType(t *testing.T, db *store.DB, code string) (*bins.BinType, *bins.BinType) {
	t.Helper()
	carrier := &bins.BinType{Code: code}
	testutil.MustNoErr(t, db.CreateBinType(carrier), "cart type")
	id, err := db.EnsureBareMarker(carrier.ID)
	testutil.MustNoErr(t, err, "marker")
	marker, err := db.GetBinType(id)
	testutil.MustNoErr(t, err, "read marker")
	return carrier, marker
}

// pair creates a two-stage pair whose stage 2 pulls from the fixture's wait
// group, with n stage-2 windows.
func (f *s2Fixture) pair(t *testing.T, name string, n int) s2Pair {
	t.Helper()
	s1, s2, err := f.loaders.CreateTwoStage(service.TwoStageCreate{Name: name})
	testutil.MustNoErr(t, err, "create pair")
	p := s2Pair{stage1: s1, stage2: s2}
	for i := 1; i <= n; i++ {
		w := &nodes.Node{Name: fmt.Sprintf("%s-S2-W%d", name, i), Enabled: true}
		testutil.MustNoErr(t, f.db.CreateNode(w), "window")
		testutil.MustNoErr(t, f.loaders.SetHome(s2, w.ID, "", "", 0), "stage-2 window")
		p.windows = append(p.windows, w)
	}
	two, err := f.loaders.Get(s2)
	testutil.MustNoErr(t, err, "read stage 2")
	testutil.MustNoErr(t, f.loaders.Update(service.LoaderUpdate{
		ID: s2, Name: two.Name, InboundSource: f.group.Name,
	}), "stage 2 pulls from the wait group")
	return p
}

// cart stands a cart of the given type on a node, arriving `age` ago.
func (f *s2Fixture) cart(t *testing.T, nodeID int64, bt *bins.BinType, age time.Duration) *bins.Bin {
	t.Helper()
	f.seq++
	b := createTestBinAtNode(t, f.db, "", nodeID, fmt.Sprintf("BIN-S2-%s-%d", bt.Code, f.seq))
	_, err := f.db.Exec(`UPDATE bins SET bin_type_id=$1, updated_at=NOW() - $2::interval WHERE id=$3`,
		bt.ID, fmt.Sprintf("%d seconds", int(age.Seconds())), b.ID)
	testutil.MustNoErr(t, err, "retype cart")
	return b
}

// s2Moves returns the core-s2- orders, oldest first.
func s2Moves(t *testing.T, db *store.DB) []*orders.Order {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM orders WHERE edge_uuid LIKE $1 ORDER BY id`, Stage2EdgeUUIDPrefix+"%")
	testutil.MustNoErr(t, err, "list core-s2 orders")
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

// awaitMoves polls for n core-s2- orders — the triggers pull on a goroutine.
func awaitMoves(t *testing.T, db *store.DB, n int) []*orders.Order {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := s2Moves(t, db)
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func binOf(o *orders.Order) int64 {
	if o.BinID == nil {
		return 0
	}
	return *o.BinID
}

func TestStage2Pull_MovesOldestOwnCartIntoFreeWindow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	f := newS2Fixture(t, db, "S2O", 3)
	p := f.pair(t, "S2O", 1)
	newer := f.cart(t, f.slots[0].ID, f.marker, time.Minute)
	older := f.cart(t, f.slots[1].ID, f.marker, time.Hour)
	f.cart(t, f.slots[2].ID, f.carrier, 2*time.Hour) // not bare: an ordinary empty, never pulled
	eng := newUnstartedEngine(t, db, simulator.New())
	eng.Start()
	t.Cleanup(eng.Stop)

	awaitMoves(t, db, 1)
	if n, err := eng.PullStage2(p.stage1); err != nil || n != 0 {
		t.Errorf("second pull made %d moves (err %v), want none: the one window is spoken for", n, err)
	}
	moves := s2Moves(t, db)
	if len(moves) != 1 {
		t.Fatalf("core-s2 moves = %d, want 1 (one free window)", len(moves))
	}
	m := moves[0]
	if binOf(m) != older.ID {
		t.Errorf("moved bin %d, want the oldest bare cart %d (newer %d)", binOf(m), older.ID, newer.ID)
	}
	if m.DeliveryNode != p.windows[0].Name {
		t.Errorf("delivers to %s, want the free window %s", m.DeliveryNode, p.windows[0].Name)
	}
}

func TestStage2Pull_IgnoresOtherGroups(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	f := newS2Fixture(t, db, "S2G", 1)
	p := f.pair(t, "S2G", 1)
	otherID, err := db.CreateNodeGroup("S2G-OTHER")
	testutil.MustNoErr(t, err, "other group")
	slot := &nodes.Node{Name: "S2G-OTHER-1", Enabled: true, ParentID: &otherID}
	testutil.MustNoErr(t, db.CreateNode(slot), "other slot")
	loose := &nodes.Node{Name: "S2G-LOOSE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(loose), "loose node")
	f.cart(t, slot.ID, f.marker, time.Hour)
	f.cart(t, loose.ID, f.marker, time.Hour)
	eng := newTestEngine(t, db, simulator.New())

	n, err := eng.PullStage2(p.stage1)
	testutil.MustNoErr(t, err, "pull")
	if n != 0 || len(s2Moves(t, db)) != 0 {
		t.Errorf("pull made %d moves from carts outside the wait group, want none", n)
	}
}

func TestStage2Pull_WindowWithInboundOrBinIsNotFree(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	f := newS2Fixture(t, db, "S2F", 2)
	p := f.pair(t, "S2F", 2)
	f.cart(t, f.slots[0].ID, f.marker, time.Hour)
	f.cart(t, f.slots[1].ID, f.marker, time.Minute)
	createTestBinAtNode(t, db, "", p.windows[0].ID, "BIN-S2F-SITTING")
	// An order already on its way to window 2, before the engine starts, so
	// the startup sweep meets the same two windows the pull below does.
	inbound := &orders.Order{
		EdgeUUID: "s2f-inbound", StationID: "test-station", OrderType: dispatch.OrderTypeMove,
		Status: dispatch.StatusPending, Quantity: 1, DeliveryNode: p.windows[1].Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(inbound), "inbound order to window 2")
	eng := newTestEngine(t, db, simulator.New())

	n, err := eng.PullStage2(p.stage1)
	testutil.MustNoErr(t, err, "pull")
	if got := s2Moves(t, db); n != 0 || len(got) != 0 {
		t.Errorf("pull made %d moves (%d in all), want none: window 1 holds a bin and window 2 has an order on its way", n, len(got))
	}
}

func TestStage2Pull_FiresOnPickupOnArrivalAndAtStartup(t *testing.T) {
	t.Parallel()

	t.Run("startup", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		f := newS2Fixture(t, db, "S2T1", 1)
		f.pair(t, "S2T1", 1)
		f.cart(t, f.slots[0].ID, f.marker, time.Hour)
		newTestEngine(t, db, simulator.New())
		if got := awaitMoves(t, db, 1); len(got) != 1 {
			t.Errorf("startup sweep made %d moves, want 1", len(got))
		}
	})

	t.Run("pickup frees a window", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		storage, _, _ := setupTestData(t, db)
		f := newS2Fixture(t, db, "S2T2", 1)
		p := f.pair(t, "S2T2", 1)
		sitting := createTestBinAtNode(t, db, "", p.windows[0].ID, "BIN-S2T2-SITTING")
		f.cart(t, f.slots[0].ID, f.marker, time.Hour)
		eng := newTestEngine(t, db, simulator.New())
		time.Sleep(300 * time.Millisecond) // the startup sweep finds no free window
		if got := s2Moves(t, db); len(got) != 0 {
			t.Fatalf("moves before the pickup = %d, want 0", len(got))
		}
		_, err := db.Exec(`UPDATE bins SET node_id=$1 WHERE id=$2`, storage.ID, sitting.ID)
		testutil.MustNoErr(t, err, "the U2 lifts the cart off")
		eng.Events.Emit(Event{Type: EventBinEnteredTransit, Payload: BinEnteredTransitEvent{
			BinID: sitting.ID, FromNodeID: p.windows[0].ID,
		}})
		if got := awaitMoves(t, db, 1); len(got) != 1 {
			t.Errorf("pickup from a stage-2 window made %d moves, want 1", len(got))
		}
	})

	t.Run("cart arrives in the wait group", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		f := newS2Fixture(t, db, "S2T3", 1)
		f.pair(t, "S2T3", 1)
		eng := newTestEngine(t, db, simulator.New())
		time.Sleep(300 * time.Millisecond) // the startup sweep finds no cart
		f.cart(t, f.slots[0].ID, f.marker, 0)
		arrived := &orders.Order{
			EdgeUUID: "s2t3-u2", StationID: "test-station", OrderType: dispatch.OrderTypeMove,
			Status: dispatch.StatusConfirmed, Quantity: 1, DeliveryNode: f.slots[0].Name,
		}
		testutil.MustNoErr(t, db.CreateOrder(arrived), "stage 1's U2")
		eng.Events.Emit(Event{Type: EventOrderCompleted, Payload: OrderCompletedEvent{
			OrderID: arrived.ID, EdgeUUID: arrived.EdgeUUID, StationID: arrived.StationID,
		}})
		if got := awaitMoves(t, db, 1); len(got) != 1 {
			t.Errorf("a cart arriving in the wait group made %d moves, want 1", len(got))
		}
	})
}

func TestStage2Pull_ConcurrentTriggersMakeOneMove(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	f := newS2Fixture(t, db, "S2C", 2)
	p := f.pair(t, "S2C", 1)
	f.cart(t, f.slots[0].ID, f.marker, time.Hour)
	f.cart(t, f.slots[1].ID, f.marker, time.Minute)
	eng := newTestEngine(t, db, simulator.New()) // its startup sweep is a ninth trigger

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := eng.PullStage2(p.stage1); err != nil {
				t.Errorf("pull: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := s2Moves(t, db); len(got) != 1 {
		t.Errorf("eight concurrent pulls made %d moves for one free window, want 1", len(got))
	}
}

// Two pairs name one wait group, the way several loaders share a supermarket.
// Each window admits only its own cart type; the pull takes the cart the window
// admits, not the oldest cart in the group.
func TestStage2Pull_SharedWaitGroupTakesOnlyCartsTheWindowAdmits(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	f := newS2Fixture(t, db, "S2S", 2)
	cartB, markerB := s2CartType(t, db, "S2S-CART-B")
	pa := f.pair(t, "S2S-A", 1)
	pb := f.pair(t, "S2S-B", 1)
	testutil.MustNoErr(t, db.SetNodeBinTypes(pa.windows[0].ID, []int64{f.carrier.ID}), "window A takes cart A")
	testutil.MustNoErr(t, db.SetNodeBinTypes(pb.windows[0].ID, []int64{cartB.ID}), "window B takes cart B")
	a := f.cart(t, f.slots[0].ID, f.marker, 2*time.Hour) // older: the naive pick
	b := f.cart(t, f.slots[1].ID, markerB, time.Hour)
	eng := newTestEngine(t, db, simulator.New())

	// Pair B first, so the older cart A is the one a type-blind pull would take.
	_, err := eng.PullStage2(pb.stage1)
	testutil.MustNoErr(t, err, "pull B")
	_, err = eng.PullStage2(pa.stage1)
	testutil.MustNoErr(t, err, "pull A")
	moves := awaitMoves(t, db, 2)
	got := map[string]int64{}
	for _, m := range moves {
		got[m.DeliveryNode] = binOf(m)
	}
	if len(moves) != 2 || got[pb.windows[0].Name] != b.ID || got[pa.windows[0].Name] != a.ID {
		t.Errorf("moves by window = %v, want %s <- cart B %d and %s <- cart A %d: each window takes only the cart it admits",
			got, pb.windows[0].Name, b.ID, pa.windows[0].Name, a.ID)
	}
}

// TestStage2Pull_CycleTwoWindowsThreeCarts is the U0 cycle, Core's half: three
// carts wait, two windows fill, a window's cart gets its type back (SEND ON)
// and leaves (the Edge's U2), and the third cart is pulled into the freed
// window.
func TestStage2Pull_CycleTwoWindowsThreeCarts(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	f := newS2Fixture(t, db, "S2Y", 3)
	empties := &nodes.Node{Name: "S2Y-EMPTIES", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(empties), "where stage 2 sends a cart")
	f.pair(t, "S2Y", 2)
	c1 := f.cart(t, f.slots[0].ID, f.marker, 3*time.Hour)
	c2 := f.cart(t, f.slots[1].ID, f.marker, 2*time.Hour)
	c3 := f.cart(t, f.slots[2].ID, f.marker, time.Hour)
	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	moves := awaitMoves(t, db, 2)
	if len(moves) != 2 || binOf(moves[0]) == c3.ID || binOf(moves[1]) == c3.ID {
		t.Fatalf("first pulls = %v, want the two oldest carts %d and %d into the two windows", moves, c1.ID, c2.ID)
	}
	for _, m := range moves {
		// The row can be read before the pull has handed it to the fleet.
		for deadline := time.Now().Add(5 * time.Second); m.VendorOrderID == "" && time.Now().Before(deadline); {
			time.Sleep(20 * time.Millisecond)
			o, err := db.GetOrder(m.ID)
			testutil.MustNoErr(t, err, "move")
			m = o
		}
		sim.DriveSimpleLifecycle(m.VendorOrderID)
		if o := awaitOrderStatus(t, db, m.ID, dispatch.StatusConfirmed); o.Status != dispatch.StatusConfirmed {
			t.Fatalf("move %d is %s after landing, want confirmed", o.ID, o.Status)
		}
	}

	// SEND ON at window 1: the cart gets its type back, and the Edge's U2
	// carries it out.
	landed, err := db.GetBin(binOf(moves[0]))
	testutil.MustNoErr(t, err, "landed cart")
	win := landed.NodeID
	_, err = eng.ClearForReuseStampAndBookDeparture(landed.ID, *win, nil, bins.StampCarrier)
	testutil.MustNoErr(t, err, "SEND ON")
	back, err := db.GetBin(landed.ID)
	testutil.MustNoErr(t, err, "cart after SEND ON")
	if back.BinTypeID != f.carrier.ID {
		t.Fatalf("SEND ON stamped %s, want the carrier %s", back.BinTypeCode, f.carrier.Code)
	}
	out, err := eng.CreateBinMove(BinMoveRequest{
		Selection: BinSelectionByLabel, BinLabel: back.Label, DestNodeID: empties.ID, StationID: "test-station",
	})
	testutil.MustNoErr(t, err, "stage 2's U2")
	u2, err := db.GetOrder(out.OrderID)
	testutil.MustNoErr(t, err, "U2 order")
	sim.DriveSimpleLifecycle(u2.VendorOrderID)
	eng.Events.Emit(Event{Type: EventBinEnteredTransit, Payload: BinEnteredTransitEvent{
		BinID: back.ID, OrderID: u2.ID, FromNodeID: *win,
	}})

	moves = awaitMoves(t, db, 3)
	if len(moves) != 3 || binOf(moves[2]) != c3.ID {
		t.Fatalf("after the window freed, moves = %d, want the third cart %d pulled", len(moves), c3.ID)
	}
	w, err := db.GetNode(*win)
	testutil.MustNoErr(t, err, "window")
	if moves[2].DeliveryNode != w.Name {
		t.Errorf("third cart delivers to %s, want the freed window %s", moves[2].DeliveryNode, w.Name)
	}
}
