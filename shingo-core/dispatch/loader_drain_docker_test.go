//go:build docker

package dispatch

import (
	"fmt"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// loader_drain_docker_test.go — an unloader's drain (dispatch/loader_drain.go):
// the carts its ticked boxes name, from its inbound source, into its free
// windows. These run with no scanner, so a drain's move is born and stays
// `pending`; what a dispatched one does to a full ask, and the triggers, are
// the engine's (engine/loader_drain_docker_test.go).

type drainFixture struct {
	db      *store.DB
	d       *Dispatcher
	source  *nodes.Node
	slots   []*nodes.Node
	windows []*nodes.Node
	loader  int64
	tote    *bins.BinType
	plain   *bins.BinType
	seq     int
}

// newDrainFixture builds an inbound source group of `slots` plain slots, a
// tote type (8 per layer) and a plain type, and a shared-window unloader with
// `windows` windows draining the given states from the group.
func newDrainFixture(t *testing.T, db *store.DB, prefix string, windows, slots int, drains ...protocol.CarrierState) *drainFixture {
	t.Helper()
	setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, nil)
	f := &drainFixture{db: db, d: d}
	grp, err := db.CreateNodeGroup(prefix + "-PRESS-OUT")
	testutil.MustNoErr(t, err, "the inbound source")
	testdb.MarkGroup(t, db, grp)
	f.source, err = db.GetNode(grp)
	testutil.MustNoErr(t, err, "read the source")
	for i := 1; i <= slots; i++ {
		s := &nodes.Node{Name: fmt.Sprintf("%s-PRESS-OUT-%d", prefix, i), Enabled: true, ParentID: &grp}
		testutil.MustNoErr(t, db.CreateNode(s), "slot")
		f.slots = append(f.slots, s)
	}
	per := 8
	f.tote = &bins.BinType{Code: prefix + "-TOTE", TotesPerLayer: &per}
	testutil.MustNoErr(t, db.CreateBinType(f.tote), "tote type")
	f.plain = &bins.BinType{Code: prefix + "-PLAIN"}
	testutil.MustNoErr(t, db.CreateBinType(f.plain), "plain type")
	f.loader, err = db.CreateLoader(loaders.Loader{
		Name: prefix + "-UNL", Role: loaders.RoleConsume, Layout: loaders.LayoutSharedWindow,
		Replenishment: loaders.ReplenishmentOperator, InboundSource: f.source.Name, Drains: drains,
	})
	testutil.MustNoErr(t, err, "the unloader")
	for i := 1; i <= windows; i++ {
		w := &nodes.Node{Name: fmt.Sprintf("%s-W%d", prefix, i), Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(w), "window")
		testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: f.loader, PositionNodeID: w.ID, SortOrder: i}), "home")
		f.windows = append(f.windows, w)
	}
	return f
}

// cart stands a part-less cart of type bt on node, with totes on it (nil:
// not counted).
func (f *drainFixture) cart(t *testing.T, nodeID int64, bt *bins.BinType, totes *int) *bins.Bin {
	t.Helper()
	f.seq++
	b := createTestBinAtNode(t, f.db, "", nodeID, fmt.Sprintf("BIN-%s-%d", bt.Code, f.seq))
	_, err := f.db.Exec(`UPDATE bins SET bin_type_id=$1, totes_on_cart=$2 WHERE id=$3`, bt.ID, totes, b.ID)
	testutil.MustNoErr(t, err, "the cart's type and count")
	return b
}

// setDrains re-ticks the unloader's boxes.
func (f *drainFixture) setDrains(t *testing.T, drains ...protocol.CarrierState) {
	t.Helper()
	l, err := f.db.GetLoader(f.loader)
	testutil.MustNoErr(t, err, "read the unloader")
	l.Drains = drains
	testutil.MustNoErr(t, f.db.UpdateLoader(*l), "tick the boxes")
}

// drainMoves returns the core-dr- orders, oldest first.
func drainMoves(t *testing.T, db *store.DB) []*orders.Order {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM orders WHERE edge_uuid LIKE $1 ORDER BY id`, DrainEdgeUUIDPrefix+"%")
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

func drainBinOf(o *orders.Order) int64 {
	if o.BinID == nil {
		return 0
	}
	return *o.BinID
}

func totes(n int) *int { return &n }

// TestDrain_MovesTheTickedStateIntoAFreeWindow: with leftovers ticked, the
// leftover cart is moved into the first window, named, and nothing else is;
// a second drain finds no leftover left to move; ticking tops & bottoms then
// sends that cart to the second window. Each box pulls its own state.
func TestDrain_MovesTheTickedStateIntoAFreeWindow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	f := newDrainFixture(t, db, "DRM", 2, 4, protocol.CarrierLeftover)
	leftover := f.cart(t, f.slots[0].ID, f.tote, totes(13))
	topsAndBottoms := f.cart(t, f.slots[1].ID, f.tote, totes(0))
	f.cart(t, f.slots[2].ID, f.plain, nil)
	f.cart(t, f.slots[3].ID, f.tote, nil)

	n, err := f.d.DrainLoader(f.loader)
	testutil.MustNoErr(t, err, "drain")
	moves := drainMoves(t, db)
	if n != 1 || len(moves) != 1 {
		t.Fatalf("moves = %d (returned %d), want 1: one leftover cart", len(moves), n)
	}
	m := moves[0]
	if drainBinOf(m) != leftover.ID || m.DeliveryNode != f.windows[0].Name {
		t.Errorf("moved bin %d to %s, want the leftover cart %d to the first window %s",
			drainBinOf(m), m.DeliveryNode, leftover.ID, f.windows[0].Name)
	}
	if m.OrderType != OrderTypeMove || m.SourceNode != f.slots[0].Name || m.OriginClass != protocol.OriginClassNoDemand {
		t.Errorf("the move is %s from %s, origin %s; want a move from %s, no_demand",
			m.OrderType, m.SourceNode, m.OriginClass, f.slots[0].Name)
	}
	if n, err := f.d.DrainLoader(f.loader); err != nil || n != 0 {
		t.Errorf("a second drain made %d moves (err %v), want none: the one leftover cart is spoken for", n, err)
	}

	f.setDrains(t, protocol.CarrierLeftover, protocol.CarrierTopsAndBottoms)
	n, err = f.d.DrainLoader(f.loader)
	testutil.MustNoErr(t, err, "drain with tops & bottoms ticked")
	moves = drainMoves(t, db)
	if n != 1 || len(moves) != 2 || drainBinOf(moves[1]) != topsAndBottoms.ID || moves[1].DeliveryNode != f.windows[1].Name {
		t.Fatalf("after ticking tops & bottoms: %d new move(s), %d in all; want the tops & bottoms cart %d into %s",
			n, len(moves), topsAndBottoms.ID, f.windows[1].Name)
	}
}

// TestDrain_TheRowAloneRefuses: a loader whose own row cannot drain is
// refused before the lock and before any read past the row (drainsAt): no box
// ticked, one spot per part (a dedicated position is never a drained cart's
// window), stage 1 of a pair, a produce loader.
func TestDrain_TheRowAloneRefuses(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	f := newDrainFixture(t, db, "DRR", 1, 1, protocol.CarrierLeftover)
	f.cart(t, f.slots[0].ID, f.tote, totes(13))
	l, err := db.GetLoader(f.loader)
	testutil.MustNoErr(t, err, "read")
	two := int64(999999)
	for _, c := range []struct {
		name string
		edit func(l *loaders.Loader)
	}{
		{"no box ticked", func(l *loaders.Loader) { l.Drains = nil }},
		{"one spot per part", func(l *loaders.Loader) { l.Layout = loaders.LayoutDedicatedPositions }},
		{"stage 1 of a pair", func(l *loaders.Loader) { l.SecondStageLoaderID = &two }},
		{"a produce loader", func(l *loaders.Loader) { l.Role = loaders.RoleProduce }},
	} {
		row := *l
		c.edit(&row)
		if drainsAt(&row) {
			t.Errorf("%s: the row drains", c.name)
		}
	}
	if !drainsAt(l) {
		t.Fatal("the control row does not drain")
	}
}

// TestDrain_ABareMarkerIsNeverDrained: with all four boxes ticked, a bare cart
// in the source stays where it is. The marker is no state a box drains; stage
// 2's own pull is the only one that moves it.
func TestDrain_ABareMarkerIsNeverDrained(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	f := newDrainFixture(t, db, "DRB", 1, 1, loaders.Drainable[:]...)
	markerID, err := db.EnsureBareMarker(f.tote.ID)
	testutil.MustNoErr(t, err, "the tote's marker")
	marker, err := db.GetBinType(markerID)
	testutil.MustNoErr(t, err, "read the marker")
	f.cart(t, f.slots[0].ID, marker, totes(0))
	n, err := f.d.DrainLoader(f.loader)
	testutil.MustNoErr(t, err, "drain")
	if n != 0 || len(drainMoves(t, db)) != 0 {
		t.Errorf("a bare marker was drained (%d moves)", n)
	}
}

// TestDrain_NeverFromAMaintainedGroup: an unloader never drains a maintained
// group (owner). A level declared on the inbound source after the save stops
// the pull — the save's own test asked again — and inside a plain source a
// group that declares a level is never pulled from, while a plain group beside
// it is.
//
// MUTATION: drop the DeclaresLevel re-check from drainLocked and the first
// half goes red; walk the source with DescendantsOf instead of
// UnmaintainedDescendantsOf and the second does.
func TestDrain_NeverFromAMaintainedGroup(t *testing.T) {
	t.Parallel()

	t.Run("a level declared on the source later", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		f := newDrainFixture(t, db, "DRL", 1, 1, protocol.CarrierLeftover)
		f.cart(t, f.slots[0].ID, f.tote, totes(13))
		testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{GroupNodeID: f.source.ID, BinTypeID: f.tote.ID, Want: 1}),
			"a level on the source")
		n, err := f.d.DrainLoader(f.loader)
		testutil.MustNoErr(t, err, "drain")
		if n != 0 || len(drainMoves(t, db)) != 0 {
			t.Errorf("drained %d cart(s) from a group that declares a level", n)
		}
	})

	t.Run("a group with a level inside the source", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		f := newDrainFixture(t, db, "DRD", 2, 0, protocol.CarrierLeftover)
		ngrp, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
		testutil.MustNoErr(t, err, "NGRP type")
		sub := func(name string) *nodes.Node {
			g := &nodes.Node{Name: name, NodeTypeID: &ngrp.ID, ParentID: &f.source.ID, Enabled: true, IsSynthetic: true}
			testutil.MustNoErr(t, db.CreateNode(g), name)
			testdb.MarkGroup(t, db, g.ID)
			s := &nodes.Node{Name: name + "-1", Enabled: true, ParentID: &g.ID}
			testutil.MustNoErr(t, db.CreateNode(s), name+" slot")
			return s
		}
		keptSlot, plainSlot := sub("DRD-KEPT"), sub("DRD-PLAIN")
		kept := f.cart(t, keptSlot.ID, f.tote, totes(13))
		plain := f.cart(t, plainSlot.ID, f.tote, totes(13))
		testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{GroupNodeID: *keptSlot.ParentID, BinTypeID: f.tote.ID, Want: 1}),
			"a level on the inner group")
		n, err := f.d.DrainLoader(f.loader)
		testutil.MustNoErr(t, err, "drain")
		moves := drainMoves(t, db)
		if n != 1 || len(moves) != 1 || drainBinOf(moves[0]) != plain.ID {
			t.Errorf("moves = %v, want only the plain group's cart %d (the kept group's %d stays)", moves, plain.ID, kept.ID)
		}
	})
}

// TestDrain_AFullAskFirstKeepsTheWindow: a full ask at the one window,
// waiting for a full that has not been found, speaks for the window, and the
// drain moves nothing: whichever asks first takes a free window, with no
// preference read. (A drain first, and the full ask after it, is the engine's:
// the drain's move is seen once it holds its claim.)
func TestDrain_AFullAskFirstKeepsTheWindow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	f := newDrainFixture(t, db, "DRF", 1, 1, protocol.CarrierLeftover)
	_, _, part := setupTestData(t, db)
	testutil.MustNoErr(t, db.UpsertLoaderPayload(loaders.Payload{LoaderID: f.loader, PayloadCode: part.Code}), "the part it serves")
	f.cart(t, f.slots[0].ID, f.tote, totes(13))
	cfg, ok, err := f.d.LoadReplenishConfig(f.loader)
	if err != nil || !ok {
		t.Fatalf("config: ok=%v err=%v", ok, err)
	}
	res, err := sideCycleParts(f.d, ReplenishRequest{StationID: "edge.line1", LoaderID: f.loader}, cfg, part.Code)
	testutil.MustNoErr(t, err, "the full ask")
	if len(res.Created) != 1 || res.Created[0].DeliveryNode != f.windows[0].Name {
		t.Fatalf("the full ask made %v, want one full to %s", res.Created, f.windows[0].Name)
	}
	n, err := f.d.DrainLoader(f.loader)
	testutil.MustNoErr(t, err, "drain")
	if n != 0 || len(drainMoves(t, db)) != 0 {
		t.Errorf("the drain made %d move(s) to a window a full is already asked for", n)
	}
}

// TestDrain_ConcurrentCallsMakeOneMovePerWindow: eight drains of one loader at
// once, two carts and one window: the loader's own lock serializes the
// decisions, so one move.
func TestDrain_ConcurrentCallsMakeOneMovePerWindow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	f := newDrainFixture(t, db, "DRC", 1, 2, protocol.CarrierLeftover)
	f.cart(t, f.slots[0].ID, f.tote, totes(13))
	f.cart(t, f.slots[1].ID, f.tote, totes(5))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.d.DrainLoader(f.loader); err != nil {
				t.Errorf("drain: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := drainMoves(t, db); len(got) != 1 {
		t.Errorf("eight concurrent drains made %d moves for one free window, want 1", len(got))
	}
}

// TestDrain_StatementCount counts what one drain costs (store.OpenCounting),
// with no scanner, so the acquire is not in it (it is every move's, the
// scanner's): a loader with no box ticked reads its row and stops; a box on
// with every window taken reads up to the windows' freedom; one move adds the
// cart list, the windows' capabilities, the admits reads and the move's own
// plan and birth. Pinned exactly so a new read is a decision, not a drift.
func TestDrain_StatementCount(t *testing.T) {
	t.Parallel()
	db, cfgDB := testdb.OpenWithConfig(t)
	f := newDrainFixture(t, db, "DRS", 2, 2, protocol.CarrierLeftover)
	f.cart(t, f.slots[0].ID, f.tote, totes(13))
	f.cart(t, f.slots[1].ID, f.tote, totes(5))
	off, err := db.CreateLoader(loaders.Loader{Name: "DRS-OFF", Role: loaders.RoleConsume, Layout: loaders.LayoutSharedWindow,
		Replenishment: loaders.ReplenishmentOperator, InboundSource: f.source.Name})
	testutil.MustNoErr(t, err, "an unloader with no box")
	cdb, counter, err := store.OpenCounting(cfgDB)
	testutil.MustNoErr(t, err, "counting db")
	t.Cleanup(func() { cdb.Close() })
	d, _ := newTestDispatcher(t, cdb, nil)
	cost := func(id int64) (int, int64) {
		counter.Reset()
		n, err := d.DrainLoader(id)
		testutil.MustNoErr(t, err, "drain")
		return n, counter.Count()
	}
	if n, c := cost(off); n != 0 || c != 1 {
		t.Errorf("no box ticked: %d moves, %d statements; want 0 and 1 (its row)\n%s", n, c, testutil.StatementBreakdown(counter.Statements()))
	}
	n, two := cost(f.loader)
	t.Logf("two windows, two leftover carts: %d moves, %d statements\n%s", n, two, testutil.StatementBreakdown(counter.Statements()))
	if n != 2 {
		t.Fatalf("moves = %d, want 2", n)
	}
	n, taken := cost(f.loader)
	t.Logf("every window taken: %d moves, %d statements\n%s", n, taken, testutil.StatementBreakdown(counter.Statements()))
	if n != 0 {
		t.Fatalf("moves = %d with every window taken, want 0", n)
	}
	// Every window taken, 11: the row, the source, its level test, the homes
	// and the member names, then per window its node, its bins and its
	// orders. Two moves, 48: those, each free window's reservations, the cart
	// list, the capabilities, the one cart type and each window's allowed
	// types, then per move the cart's node, the window's station, and the bin
	// move's plan and birth.
	const wantTwo, wantTaken = 48, 11
	if two != wantTwo || taken != wantTaken {
		t.Errorf("statements: two moves %d, every window taken %d; want %d and %d", two, taken, wantTwo, wantTaken)
	}
}
