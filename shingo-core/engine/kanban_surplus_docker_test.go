//go:build docker

package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// kanban_surplus_docker_test.go — the surplus arm on a real database: S short, K full,
// X above its want.

// surplusRig is a lineside K of n positions with three types on one floor and one on
// another, and an unloader of four windows whose outbound is K.
type surplusRig struct {
	db      *store.DB
	eng     *Engine
	sim     *simulator.SimulatorBackend
	k       *nodes.Node
	kids    []*nodes.Node
	small   int64
	big     int64
	totes   int64
	t32     int64
	windows []*nodes.Node
	next    int // K's next position to stand a cart on
}

func newSurplusRig(t *testing.T, n int) *surplusRig {
	t.Helper()
	db := testdb.Open(t)
	sim := simulator.New()
	return surplusRigOn(t, db, sim, newTestEngine(t, db, sim), n)
}

func surplusRigOn(t *testing.T, db *store.DB, sim *simulator.SimulatorBackend, eng *Engine, n int) *surplusRig {
	t.Helper()
	r := &surplusRig{db: db, eng: eng, sim: sim}
	typ := func(code string, w, l, h float64) int64 {
		var id int64
		testutil.MustNoErr(t, db.QueryRow(
			`INSERT INTO bin_types (code, width_in, length_in, height_in) VALUES ($1,$2,$3,$4) RETURNING id`,
			code, w, l, h).Scan(&id), "type "+code)
		return id
	}
	r.small, r.big = typ("SP-SMALL", 45, 48, 25), typ("SP-BIG", 48, 45, 34)
	r.totes, r.t32 = typ("SP-TOTES", 45, 48, 6), typ("SP-32", 32, 30, 25)
	r.k, r.kids = kanbanCostFlat(t, db, "SP-K", n)
	testdb.KanbanKeeper(t, db, r.k.ID, "sp-edge")
	lid, err := db.CreateLoader(loaders.Loader{Name: "SP-UNL", Role: loaders.RoleConsume,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator, OutboundDest: r.k.Name})
	testutil.MustNoErr(t, err, "unloader")
	for i := 0; i < 4; i++ {
		w := &nodes.Node{Name: fmt.Sprintf("SP-W%d", i+1), Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(w), "window")
		testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: lid, PositionNodeID: w.ID, SortOrder: i}), "window row")
		r.windows = append(r.windows, w)
	}
	return r
}

func (r *surplusRig) level(t *testing.T, g *nodes.Node, bt int64, want int) {
	t.Helper()
	testutil.MustNoErr(t, r.db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: g.ID, BinTypeID: bt, Want: want}), "level")
}

// stand puts one empty on K's next position, each newer than the last by a minute.
func (r *surplusRig) stand(t *testing.T, bt int64) int64 {
	t.Helper()
	id := landAt(t, r.db, bt, r.kids[r.next].ID, fmt.Sprintf("SP-CART-%d", r.next))
	_, err := r.db.Exec(`UPDATE bins SET updated_at = $1 WHERE id = $2`,
		time.Date(2026, 1, 1, 0, r.next, 0, 0, time.UTC), id)
	testutil.MustNoErr(t, err, "age the cart")
	r.next++
	return id
}

// blockWindows stands a full cart on the first n windows.
func (r *surplusRig) blockWindows(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := landAt(t, r.db, r.small, r.windows[i].ID, fmt.Sprintf("SP-FULL-%d", i))
		testutil.MustNoErr(t, r.db.SetBinManifest(id, `{"items": []}`, "SP-PART", 10), "fill it")
	}
}

// moves is the arm's orders, oldest first.
func (r *surplusRig) moves(t *testing.T) []*orders.Order {
	t.Helper()
	rows, err := r.db.Query(`SELECT id FROM orders WHERE edge_uuid LIKE 'core-sp-%' ORDER BY id`)
	testutil.MustNoErr(t, err, "the arm's orders")
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		testutil.MustNoErr(t, rows.Scan(&id), "scan")
		ids = append(ids, id)
	}
	testutil.MustNoErr(t, rows.Err(), "rows")
	out := make([]*orders.Order, 0, len(ids))
	for _, id := range ids {
		o, err := r.db.GetOrder(id)
		testutil.MustNoErr(t, err, "order")
		out = append(out, o)
	}
	return out
}

// land drives an arm's move to its end, and waits for Core's confirm.
func (r *surplusRig) land(t *testing.T, o *orders.Order) {
	t.Helper()
	fresh, err := r.db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "order")
	if fresh.VendorOrderID == "" {
		t.Fatalf("order %d (%s) never reached the fleet: %s %q", o.ID, fresh.DeliveryNode, fresh.Status, fresh.QueueReason)
	}
	r.sim.DriveSimpleLifecycle(fresh.VendorOrderID)
	if got := awaitOrderStatus(t, r.db, o.ID, dispatch.StatusConfirmed); got.Status != dispatch.StatusConfirmed {
		t.Fatalf("order %d landed and is %s, want confirmed", o.ID, got.Status)
	}
}

// zone is a flat group of n positions with a level of types at want, no supports.
func (r *surplusRig) zone(t *testing.T, name string, n int, levels map[int64]int) (*nodes.Node, []*nodes.Node) {
	t.Helper()
	g, kids := kanbanCostFlat(t, r.db, name, n)
	for bt, want := range levels {
		r.level(t, g, bt, want)
	}
	return g, kids
}

func (r *surplusRig) tick(t *testing.T) *KanbanKeeper {
	t.Helper()
	m := r.eng.KanbanKeeper()
	m.Tick()
	return m
}

// TestKanbanPin_NothingMovesAResidentOutOfAGroup's two halves: above its level with
// nothing short nothing moves; short with no position free, the resident moves.
func TestKanbanPin_NothingMovesAResidentOutOfAGroup(t *testing.T) {
	t.Parallel()
	r := newSurplusRig(t, 2)
	r.level(t, r.k, r.small, 1)
	over1, over2 := r.stand(t, r.small), r.stand(t, r.small)
	for i := 0; i < 3; i++ {
		r.tick(t)
	}
	st := mntState(t, r.eng.KanbanKeeper(), "SP-K", "SP-SMALL")
	if st.Resident != 2 || st.Gap != -1 || st.Created != 0 || st.OriginID != "" {
		t.Errorf("the type above its level, nothing short: %+v, want 2 resident, gap -1, nothing asked or open", st)
	}
	if got := r.moves(t); len(got) != 0 {
		t.Fatalf("nothing is short and %d carts moved", len(got))
	}
	testdb.RequireBinAtNode(t, r.db, over1, r.kids[0].ID)
	testdb.RequireBinAtNode(t, r.db, over2, r.kids[1].ID)

	// Now another declared type is short and they are the reason it has no position.
	r.level(t, r.k, r.totes, 1)
	r.tick(t)
	got := r.moves(t)
	if len(got) != 1 || surplusCart(t, got[0]) != over1 {
		t.Fatalf("short with no position free: moves %+v, want the oldest resident %d", got, over1)
	}
}

// TestKanbanSurplus_ReTypeEndToEnd: S short, K full, X above its want, windows free. The
// oldest X goes to a window under its own prefix as nobody's demand; it lands and frees
// its claim; the walk lights it S; CLEAR re-types it; its send-out returns to K as S,
// counted as coming, and K holds its want. One free window stays free throughout.
func TestKanbanSurplus_ReTypeEndToEnd(t *testing.T) {
	t.Parallel()
	r := newSurplusRig(t, 3)
	r.level(t, r.k, r.small, 1)
	r.level(t, r.k, r.totes, 2)
	oldest := r.stand(t, r.small)
	r.stand(t, r.small)
	r.stand(t, r.small)
	r.blockWindows(t, 2) // two free: one move

	m := r.tick(t)
	got := r.moves(t)
	if len(got) != 1 {
		t.Fatalf("%d moves with two windows free, want one", len(got))
	}
	mv := got[0]
	if c := surplusCart(t, mv); c != oldest {
		t.Errorf("moved %v, want the oldest %d", c, oldest)
	}
	if !strings.HasPrefix(mv.EdgeUUID, dispatch.SurplusEdgeUUIDPrefix) || mv.OriginClass != protocol.OriginClassNoDemand || mv.OriginID != "" {
		t.Errorf("the move's provenance: %q %q %q, want the arm's prefix, no demand, no episode", mv.EdgeUUID, mv.OriginClass, mv.OriginID)
	}
	if mv.DeliveryNode != r.windows[2].Name {
		t.Errorf("delivered to %s, want the first free window %s", mv.DeliveryNode, r.windows[2].Name)
	}
	if st := mntState(t, m, "SP-K", "SP-SMALL"); st.Asked != 0 || st.Coming != 0 || st.OriginID != "" {
		t.Errorf("the surplus type's line: %+v; the move is no ask and no coming of it", st)
	}

	r.land(t, mv)
	cart, err := r.db.GetBin(oldest)
	testutil.MustNoErr(t, err, "the cart")
	if cart.NodeID == nil || *cart.NodeID != r.windows[2].ID || cart.ClaimedBy != nil {
		t.Fatalf("landed: at %v claimed by %v, want on the window and free", cart.NodeID, cart.ClaimedBy)
	}

	// THE WALK LIGHTS IT, on the line an Edge is sent.
	windows, err := r.db.ListKanbanWindows(r.k.ID)
	testutil.MustNoErr(t, err, "windows")
	rows := make([]protocol.NodeBinsRow, 0, len(windows))
	for _, w := range windows {
		if w.Row.Occupied {
			rows = append(rows, w.Row)
		}
	}
	levels, err := r.db.ListKanbanLevels(r.k.ID)
	testutil.MustNoErr(t, err, "levels")
	lines, err := r.eng.UnloaderGroupLines([]store.GroupLineAsk{{Group: r.k, Levels: levels, Windows: rows}})
	testutil.MustNoErr(t, err, "the line")
	lit := ""
	for _, a := range lines[0].Assignments {
		if a.Node == r.windows[2].Name {
			lit = a.BinType
		}
	}
	if lit != "SP-TOTES" {
		t.Fatalf("the arm's cart is lit %q in %+v, want SP-TOTES", lit, lines[0].Assignments)
	}

	// CLEAR re-types it, and the send-out names it, as the Edge's sendCartOut does.
	totes := r.totes
	_, err = r.eng.ClearForReuseAndBookDeparture(oldest, r.windows[2].ID, &totes)
	testutil.MustNoErr(t, err, "CLEAR as totes")
	r.eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: "sp-u2",
		OrderType: protocol.OrderTypeMove, SourceNode: r.windows[2].Name, DeliveryNode: r.k.Name, Quantity: 1, BinID: oldest})
	st := func() KanbanLineState { return mntState(t, r.tick(t), "SP-K", "SP-TOTES") }
	if s := st(); s.Coming != 1 || s.Gap != 1 {
		t.Errorf("after the send-out: %+v, want coming 1, gap 1: counted as coming, and once", s)
	}
	back := testdb.RequireOrder(t, r.db, "sp-u2")
	if back.VendorOrderID == "" {
		t.Fatalf("the send-out is %s: %s", back.Status, back.QueueReason)
	}
	r.sim.DriveSimpleLifecycle(back.VendorOrderID)
	r.eng.Dispatcher().HandleOrderReceipt(&protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: back.StationID}},
		&protocol.OrderReceipt{OrderUUID: back.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1})
	if s := st(); s.Resident != 1 || s.Coming != 0 {
		t.Errorf("home: %+v, want one resident totes, nothing coming", s)
	}
	testdb.RequireBinAtNode(t, r.db, oldest, r.kids[0].ID)
}

// TestKanbanSurplus_NeverTheLastFreeWindow: two free, one move; three free and need two,
// two moves; one free, no move and the cause on the surplus type's line.
func TestKanbanSurplus_NeverTheLastFreeWindow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		blocked, moves int
		cause          string
	}{
		{blocked: 2, moves: 1, cause: "keeps its last one free"},
		{blocked: 1, moves: 2},
		{blocked: 0, moves: 2},
		{blocked: 3, moves: 0, cause: "keeps its last one free"},
		{blocked: 4, moves: 0, cause: "keeps its last one free"},
	} {
		t.Run(fmt.Sprintf("%d free", 4-tc.blocked), func(t *testing.T) {
			t.Parallel()
			r := newSurplusRig(t, 3)
			r.level(t, r.k, r.small, 1)
			r.level(t, r.k, r.totes, 2) // need two
			for i := 0; i < 3; i++ {
				r.stand(t, r.small)
			}
			r.blockWindows(t, tc.blocked)
			m := r.tick(t)
			if got := r.moves(t); len(got) != tc.moves {
				t.Fatalf("%d moves, want %d", len(got), tc.moves)
			}
			st := mntState(t, m, "SP-K", "SP-SMALL")
			if tc.cause == "" {
				if st.SurplusCause != "" {
					t.Errorf("a cause with nothing left to move: %q", st.SurplusCause)
				}
			} else if !strings.Contains(st.SurplusCause, tc.cause) {
				t.Errorf("the surplus's cause = %q, want it to say %q", st.SurplusCause, tc.cause)
			}
			if st.OldestAskCause != "" {
				t.Errorf("a surplus has no ask: oldest_ask_cause %q", st.OldestAskCause)
			}
		})
	}
}

// TestKanbanSurplus_LargestOnTheFloorThenOldest: the oldest cart in K is a 32x30, off the
// floor. The oldest on the floor is BIG, one above its want. SMALL, two above, moves, and
// its oldest. Age alone would take either of the others.
func TestKanbanSurplus_LargestOnTheFloorThenOldest(t *testing.T) {
	t.Parallel()
	r := newSurplusRig(t, 6)
	r.level(t, r.k, r.small, 1)
	r.level(t, r.k, r.big, 1)
	r.level(t, r.k, r.t32, 0)
	r.level(t, r.k, r.totes, 1)
	r.stand(t, r.t32)
	r.stand(t, r.big)
	r.stand(t, r.big)
	oldestSmall := r.stand(t, r.small)
	r.stand(t, r.small)
	r.stand(t, r.small)
	r.tick(t)
	got := r.moves(t)
	if len(got) != 1 || surplusCart(t, got[0]) != oldestSmall {
		t.Fatalf("moves %+v, want only the oldest of the largest surplus on the floor, bin %d", got, oldestSmall)
	}
	if got[0].DeliveryNode != r.windows[0].Name {
		t.Errorf("delivered to %s, want a window", got[0].DeliveryNode)
	}
}

// TestKanbanSurplus_AnotherFloorGoesToTheFirstSourceWithRoom: K short of 32x30, its
// surplus on 45x48. A source with no level of the type, then one at its level, have no
// room; the third does, for one; its slot takes it and the landed move confirms. The
// next tick has no room left and waits, with the cause on the type's line.
func TestKanbanSurplus_AnotherFloorGoesToTheFirstSourceWithRoom(t *testing.T) {
	t.Parallel()
	r := newSurplusRig(t, 3)
	r.level(t, r.k, r.totes, 1)
	r.level(t, r.k, r.t32, 2)
	oldest := r.stand(t, r.totes)
	r.stand(t, r.totes)
	r.stand(t, r.totes)
	noLevel, _ := r.zone(t, "SP-Z32", 4, map[int64]int{r.t32: 2})
	atLevel, full := r.zone(t, "SP-ZFULL", 4, map[int64]int{r.totes: 1})
	landAt(t, r.db, r.totes, full[0].ID, "SP-ZFULL-1")
	roomy, _ := r.zone(t, "SP-Z45", 4, map[int64]int{r.totes: 1})
	testdb.KanbanSources(t, r.db, r.k.ID, noLevel.ID, atLevel.ID, roomy.ID)

	r.tick(t)
	got := r.moves(t)
	if len(got) != 1 || surplusCart(t, got[0]) != oldest {
		t.Fatalf("moves %+v, want the oldest surplus, bin %d", got, oldest)
	}
	dest, err := r.db.GetNodeByDotName(got[0].DeliveryNode)
	testutil.MustNoErr(t, err, "the slot")
	if dest.ParentID == nil || *dest.ParentID != roomy.ID {
		t.Fatalf("delivered to %s, want a position of %s", got[0].DeliveryNode, roomy.Name)
	}
	if got[0].OriginClass != protocol.OriginClassNoDemand || got[0].OriginID != "" {
		t.Errorf("the move's provenance: %q %q, want no demand and no episode", got[0].OriginClass, got[0].OriginID)
	}

	// In flight: its source is spoken for, and the move is not made twice.
	st := mntState(t, r.tick(t), "SP-K", "SP-TOTES")
	if n := len(r.moves(t)); n != 1 {
		t.Errorf("%d moves after a second tick, want still one", n)
	}
	if !strings.Contains(st.SurplusCause, "no source of SP-K has room") {
		t.Errorf("the surplus left, every source at its level: cause %q", st.SurplusCause)
	}

	r.land(t, got[0])
	cart, err := r.db.GetBin(oldest)
	testutil.MustNoErr(t, err, "the cart")
	if cart.NodeID == nil || *cart.NodeID != dest.ID || cart.ClaimedBy != nil {
		t.Errorf("landed: at %v claimed by %v, want on %s and free", cart.NodeID, cart.ClaimedBy, dest.Name)
	}
}

// TestKanbanSurplus_NeverToAPlainGroupOrTheOverflow: a plain group with free slots stands
// in K's sources and as its overflow, and a source with a level has supports. No cart
// goes to any of them. It waits, and says so.
func TestKanbanSurplus_NeverToAPlainGroupOrTheOverflow(t *testing.T) {
	t.Parallel()
	r := newSurplusRig(t, 3)
	r.level(t, r.k, r.totes, 1)
	r.level(t, r.k, r.t32, 2)
	for i := 0; i < 3; i++ {
		r.stand(t, r.totes)
	}
	plain, _ := kanbanCostFlat(t, r.db, "SP-PLAIN", 4)
	otherLineside, _ := r.zone(t, "SP-K2", 4, map[int64]int{r.totes: 3})
	testdb.KanbanKeeper(t, r.db, otherLineside.ID, "sp-edge")
	testdb.KanbanSources(t, r.db, r.k.ID, plain.ID, otherLineside.ID)
	testdb.KanbanOverflow(t, r.db, r.k.ID, plain.Name)

	st := mntState(t, r.tick(t), "SP-K", "SP-TOTES")
	if got := r.moves(t); len(got) != 0 {
		t.Fatalf("%d carts moved to %s: never to a plain group, the overflow or another process's kanban", len(got), got[0].DeliveryNode)
	}
	if !strings.Contains(st.SurplusCause, "no source of SP-K has room") {
		t.Errorf("cause %q, want no source with room", st.SurplusCause)
	}
}

// TestKanbanSurplus_NeverMovesWhatTheCountRefuses: the oldest carts in K are claimed,
// reserved, locked, staged and loaded, and K reads none of them as a resident. The arm
// lists only what it counts, so the one above its want it moves is the plain one, and
// a cart reserved since the list was read refuses the issue: nothing here takes a hold.
func TestKanbanSurplus_NeverMovesWhatTheCountRefuses(t *testing.T) {
	t.Parallel()
	r := newSurplusRig(t, 8)
	r.level(t, r.k, r.small, 1)
	r.level(t, r.k, r.totes, 1)
	claimed, reserved, locked := r.stand(t, r.small), r.stand(t, r.small), r.stand(t, r.small)
	staged, loaded := r.stand(t, r.small), r.stand(t, r.small)
	plain1, plain2 := r.stand(t, r.small), r.stand(t, r.small)
	r.stand(t, r.small) // K full
	holder := func(uuid string) int64 {
		return testdb.CreateOrder(t, r.db, func(o *orders.Order) { o.EdgeUUID = uuid }).ID
	}
	testdb.ClaimBinForTest(t, r.db, claimed, dispatched(t, r.db, "sp-claim"))
	testdb.ReserveBin(t, r.db, holder("sp-reserve"), reserved)
	testutil.MustNoErr(t, r.db.LockBin(locked, "someone"), "lock")
	testutil.MustNoErr(t, r.db.StageBin(staged, nil), "stage")
	testutil.MustNoErr(t, r.db.SetBinManifest(loaded, `{"items": []}`, "SP-PART", 10), "load")

	listed, err := r.db.ListKanbanSurplus(r.k.ID, []string{"SP-SMALL"})
	testutil.MustNoErr(t, err, "the list")
	if len(listed) != 3 || listed[0].ID != plain1 || listed[1].ID != plain2 {
		t.Fatalf("listed %d carts, first %d; want the three plain ones, oldest first, %d then %d", len(listed), listed[0].ID, plain1, plain2)
	}

	r.tick(t)
	got := r.moves(t)
	if len(got) != 1 || surplusCart(t, got[0]) != plain1 {
		t.Fatalf("moves %+v, want only the oldest plain cart %d", got, plain1)
	}

	// The race the list cannot see: reserved since it was read. The issue refuses.
	testdb.ReserveBin(t, r.db, holder("sp-late"), plain2)
	stale, err := r.db.GetBin(plain2)
	testutil.MustNoErr(t, err, "the stale cart")
	loaderID := func() int64 {
		ws, err := r.db.ListKanbanWindows(r.k.ID)
		testutil.MustNoErr(t, err, "windows")
		return ws[0].LoaderID
	}()
	moved, _, err := r.eng.Dispatcher().SurplusToWindows(loaderID, r.windows[1:], []*bins.Bin{stale, listed[2]}, "stale")
	testutil.MustNoErr(t, err, "the moves")
	if len(moved) != 1 || moved[0] != listed[2].ID {
		t.Errorf("moved %v, want the reserved cart passed over for %d", moved, listed[2].ID)
	}
	after, err := r.db.GetBin(plain2)
	testutil.MustNoErr(t, err, "after")
	if after.ClaimedBy != nil || after.NodeID == nil || *after.NodeID != *stale.NodeID {
		t.Errorf("the reserved cart moved: %+v", after)
	}
}

// TestKanbanSurplus_TheAsksAreNetted: S short 3, one arm cart at a window lit S, two on
// their way: the keeper asks none, and resident + asked + the arm's never passes the want.
// Each counts once, on the keeper's count and on the walk's.
func TestKanbanSurplus_TheAsksAreNetted(t *testing.T) {
	t.Parallel()
	r := newSurplusRig(t, 4)
	r.level(t, r.k, r.small, 0)
	r.level(t, r.k, r.totes, 3)
	for i := 0; i < 3; i++ {
		r.stand(t, r.small)
	}
	for i := 0; i < 3; i++ {
		mntOutsideEmpty(t, r.db, r.totes, fmt.Sprintf("SP-OUT%d", i))
	}
	// A cart standing in the 4th child would hide the free slot: fill it, then free it.
	block := r.stand(t, r.small)
	m := r.tick(t)
	got := r.moves(t)
	if len(got) != 3 {
		t.Fatalf("%d moves, want three: K full, four windows, three above", len(got))
	}
	r.land(t, got[0])
	_, err := r.db.Exec(`DELETE FROM bins WHERE id = $1`, block)
	testutil.MustNoErr(t, err, "free a child")

	// Two carts of three still lift: 1 landed, 2 live; all three lit totes.
	m.Tick()
	st := mntState(t, m, "SP-K", "SP-TOTES")
	if st.Gap != 3 || st.Created != 0 {
		t.Errorf("three lit against a gap of three: %+v, want gap 3 and nothing asked", st)
	}
	if !strings.Contains(st.HeldBy, "3 at the unloader's windows") {
		t.Errorf("held by %q, want the windows named", st.HeldBy)
	}

	// The want rises by one: the keeper asks the one the carts do not cover.
	r.level(t, r.k, r.totes, 4)
	m.Tick()
	st = mntState(t, m, "SP-K", "SP-TOTES")
	if st.Gap != 4 || st.Created != 1 {
		t.Errorf("want 4, three lit: %+v, want gap 4 and one ask", st)
	}

	// Landed, the line an Edge is sent lights the same three carts the keeper left out.
	r.land(t, got[1])
	r.land(t, got[2])
	windows, err := r.db.ListKanbanWindows(r.k.ID)
	testutil.MustNoErr(t, err, "windows")
	var rows []protocol.NodeBinsRow
	marked := map[string]bool{}
	for _, w := range windows {
		if w.Row.Occupied {
			rows = append(rows, w.Row)
		}
		if w.Surplus {
			marked[w.Row.Node] = true
		}
	}
	levels, err := r.db.ListKanbanLevels(r.k.ID)
	testutil.MustNoErr(t, err, "levels")
	lines, err := r.eng.UnloaderGroupLines([]store.GroupLineAsk{{Group: r.k, Levels: levels, Windows: rows}})
	testutil.MustNoErr(t, err, "the line")
	lit := 0
	for _, a := range lines[0].Assignments {
		if a.BinType == "SP-TOTES" && marked[a.Node] {
			lit++
		}
	}
	if len(marked) != 3 || lit != 3 {
		t.Errorf("%d windows carry the arm's cart and the line lights %d as totes, want 3 and 3: %+v", len(marked), lit, lines[0].Assignments)
	}
}

// TestKanbanSurplus_ARestartMidMoveIsNotStranded: a move issued by a Core with no scanner
// stands pending across the restart. The next Core's tick reads it back and makes no
// second move; its scanner hands it to the fleet as it does any pending order; it lands
// and confirms. Nothing outside the rows carried it, as core-s2- and core-dr- moves.
func TestKanbanSurplus_ARestartMidMoveIsNotStranded(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sim := simulator.New()
	first := newUnstartedEngine(t, db, sim)
	first.dispatcher = first.newDispatcher(&dispatchEmitter{bus: first.Events, engine: first}, &dispatch.DefaultResolver{DB: db})
	r := surplusRigOn(t, db, sim, first, 3)
	r.level(t, r.k, r.small, 1)
	r.level(t, r.k, r.totes, 1)
	r.stand(t, r.small)
	r.stand(t, r.small)
	r.stand(t, r.small)
	r.tick(t)
	got := r.moves(t)
	if len(got) != 1 || got[0].Status != protocol.StatusPending {
		t.Fatalf("moves %+v, want one, pending: nothing has acquired it", got)
	}

	r.eng = newTestEngine(t, db, sim)
	st := mntState(t, r.tick(t), "SP-K", "SP-TOTES")
	if n := len(r.moves(t)); n != 1 {
		t.Fatalf("%d moves after the restart, want the same one", n)
	}
	if st.Gap != 1 || st.Created != 0 {
		t.Errorf("after the restart: %+v, want the arm's cart netted", st)
	}
	r.eng.RunFulfillmentScan()
	r.land(t, got[0])
}

// TestKanbanSurplus_CoverMovingAwayDrivesIt: nothing is short and K holds its want, until
// a changeover opens. The cover moves: the old type stands above its want with no child
// free, the target is short, and the arm moves. Across floors the cart goes to a source,
// and with every source at its level it waits.
func TestKanbanSurplus_CoverMovingAwayDrivesIt(t *testing.T) {
	t.Parallel()
	open := func(t *testing.T, r *surplusRig, process, part, support string) {
		t.Helper()
		id := fmt.Sprintf("c0c0c0c0-0000-4000-8000-%012d", r.k.ID)
		testutil.MustNoErr(t, r.db.UpsertDemandOrigin(store.DemandOrigin{OriginID: id, Revision: 1, EpisodeKey: "co|" + id,
			Kind: protocol.EpisodeKindChangeover, StationID: "sp-edge", ProcessID: process, CoreNodeName: support,
			PayloadCode: part, OpenedAt: time.Now().UTC()}), "open the changeover")
	}
	setup := func(t *testing.T, name string) (*surplusRig, string) {
		r := newSurplusRig(t, 4)
		r.level(t, r.k, r.small, 1)
		r.level(t, r.k, r.totes, 1)
		r.level(t, r.k, r.t32, 0)
		testdb.KanbanCover(t, r.db, r.k.ID, 2)
		coverPart(t, r.db, name+"-SP", r.small)
		coverPart(t, r.db, name+"-TP", r.totes)
		coverPart(t, r.db, name+"-32", r.t32)
		coverProcess(t, r.db, name, coverStyle{"SMALL", true, [][2]string{{"SP-K-SUPPORT", name + "-SP"}}},
			coverStyle{"TOTES", false, [][2]string{{"SP-K-SUPPORT", name + "-TP"}}},
			coverStyle{"C32", false, [][2]string{{"SP-K-SUPPORT", name + "-32"}}})
		for i := 0; i < 3; i++ {
			r.stand(t, r.small)
		}
		r.stand(t, r.totes)
		r.tick(t)
		if got := r.moves(t); len(got) != 0 {
			t.Fatalf("steady: %d moves", len(got))
		}
		return r, name
	}

	t.Run("within the floor", func(t *testing.T) {
		t.Parallel()
		r, name := setup(t, "SPA")
		open(t, r, name, name+"-TP", "SP-K-SUPPORT")
		m := r.tick(t)
		if st := mntState(t, m, "SP-K", "SP-SMALL"); st.Want != 1 || st.Resident != 3 {
			t.Fatalf("small's line: %+v, want want 1 and the three it read", st)
		}
		if got := r.moves(t); len(got) != 2 {
			t.Fatalf("%d moves, want 2: small 2 above with totes short 2 and K full", len(got))
		}
	})
	t.Run("across floors, a source short", func(t *testing.T) {
		t.Parallel()
		r, name := setup(t, "SPB")
		z, _ := r.zone(t, "SP-Z45B", 4, map[int64]int{r.small: 2})
		testdb.KanbanSources(t, r.db, r.k.ID, z.ID)
		open(t, r, name, name+"-32", "SP-K-SUPPORT")
		r.tick(t)
		got := r.moves(t)
		if len(got) != 2 {
			t.Fatalf("%d moves, want 2: 32x30 short 2, K full, the zone short 2 of small", len(got))
		}
		dest, err := r.db.GetNodeByDotName(got[0].DeliveryNode)
		testutil.MustNoErr(t, err, "slot")
		if dest.ParentID == nil || *dest.ParentID != z.ID {
			t.Errorf("delivered to %s, want the zone", got[0].DeliveryNode)
		}
	})
	t.Run("across floors, every source at its level", func(t *testing.T) {
		t.Parallel()
		r, name := setup(t, "SPC")
		z, kids := r.zone(t, "SP-Z45C", 4, map[int64]int{r.small: 1})
		landAt(t, r.db, r.small, kids[0].ID, "SP-Z45C-1")
		testdb.KanbanSources(t, r.db, r.k.ID, z.ID)
		open(t, r, name, name+"-32", "SP-K-SUPPORT")
		st := mntState(t, r.tick(t), "SP-K", "SP-SMALL")
		if got := r.moves(t); len(got) != 0 {
			t.Fatalf("%d moves with no source short", len(got))
		}
		if !strings.Contains(st.SurplusCause, "no source") {
			t.Errorf("cause %q", st.SurplusCause)
		}
	})
}

// TestKanbanSurplus_ANonLinesideKanbanNeverFires: the same shape with no supports: a
// level short, every child held, a type above its want, an unloader whose outbound is
// it, a short source. It runs no keeper, so nothing moves and nothing is read for it.
func TestKanbanSurplus_ANonLinesideKanbanNeverFires(t *testing.T) {
	t.Parallel()
	r := newSurplusRig(t, 2)
	r.level(t, r.k, r.small, 1)
	r.level(t, r.k, r.totes, 1)
	r.stand(t, r.small)
	r.stand(t, r.small)
	testutil.MustNoErr(t, nodes.SetKanbanSupports(r.db.DB, r.k.ID, nil), "no supports")
	st := mntState(t, r.tick(t), "SP-K", "SP-SMALL")
	if got := r.moves(t); len(got) != 0 {
		t.Fatalf("%d carts moved out of a kanban with no supports", len(got))
	}
	if st.SurplusCause != "" {
		t.Errorf("a cause on a kanban that runs no keeper: %q", st.SurplusCause)
	}
}

// TestKanbanSurplus_NoChildFreeIsExact: the arm fires on the group's own slot read, not
// on slots less residents. A child standing free holds it, with nothing to source for S.
// A child holding a loaded cart, which the keeper's count does not see, does not.
func TestKanbanSurplus_NoChildFreeIsExact(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) *surplusRig {
		r := newSurplusRig(t, 3)
		r.level(t, r.k, r.small, 1)
		r.level(t, r.k, r.totes, 1)
		r.stand(t, r.small)
		r.stand(t, r.small)
		return r
	}
	t.Run("a child free", func(t *testing.T) {
		t.Parallel()
		r := setup(t)
		st := mntState(t, r.tick(t), "SP-K", "SP-TOTES")
		if got := r.moves(t); len(got) != 0 {
			t.Fatalf("%d moves with a child free", len(got))
		}
		if st.Gap != 1 || !strings.Contains(st.HeldBy, "no empty") {
			t.Errorf("%+v, want the gap and why nothing was asked", st)
		}
	})
	t.Run("the child holds a loaded cart", func(t *testing.T) {
		t.Parallel()
		r := setup(t)
		testutil.MustNoErr(t, r.db.SetBinManifest(r.stand(t, r.small), `{"items": []}`, "SP-PART", 10), "load it")
		r.tick(t)
		if got := r.moves(t); len(got) != 1 {
			t.Fatalf("%d moves, want 1: slots 3 less residents 2 reads one free, and it is not", len(got))
		}
	})
}

// TestKanbanSurplus_ASourcesComingsFillItsRoom: a source under its level by residents
// with a cart on its way to it has no room, which its own cap, reading residents alone,
// would admit. A person's move stands in for the cart on its way.
func TestKanbanSurplus_ASourcesComingsFillItsRoom(t *testing.T) {
	t.Parallel()
	r := newSurplusRig(t, 2)
	r.level(t, r.k, r.totes, 0)
	r.level(t, r.k, r.t32, 1)
	r.stand(t, r.totes)
	r.stand(t, r.totes)
	z, kids := r.zone(t, "SP-Z45", 4, map[int64]int{r.totes: 1})
	testdb.KanbanSources(t, r.db, r.k.ID, z.ID)
	dock := &nodes.Node{Name: "SP-DOCK", Enabled: true}
	testutil.MustNoErr(t, r.db.CreateNode(dock), "dock")
	comer := landAt(t, r.db, r.totes, dock.ID, "SP-COMER")
	testdb.CreateOrder(t, r.db, func(o *orders.Order) {
		o.EdgeUUID, o.OrderType, o.Status = "sp-comer", protocol.OrderTypeMove, protocol.StatusDispatched
		o.SourceNode, o.DeliveryNode = dock.Name, kids[0].Name
		o.HoldFromBirth(comer)
	})
	testdb.ClaimBinForTest(t, r.db, comer, testdb.RequireOrder(t, r.db, "sp-comer").ID)

	st := mntState(t, r.tick(t), "SP-K", "SP-TOTES")
	if got := r.moves(t); len(got) != 0 {
		t.Fatalf("moves %+v, want none: the source's level is spoken for", got)
	}
	if !strings.Contains(st.SurplusCause, "no source of SP-K has room") {
		t.Errorf("cause %q", st.SurplusCause)
	}
}

// TestKanbanSurplus_R2Shape walks a small-to-totes changeover at the tick on a kanban with a
// lender (the rig had this shape until plants/kanban-press.yaml went to one lineside kanban):
// K 8 children under small (5 + 1 + 1, one free), cover 4 moving to totes. One ask fills the free child,
// three of small's four spare carts go to the windows in the first tick and the last
// window stays free. Lit totes, they are netted, so the keeper asks no more, and the
// plain market beside K is never asked.
func TestKanbanSurplus_R2Shape(t *testing.T) {
	t.Parallel()
	r := newSurplusRig(t, 8)
	z45, z45Kids := r.zone(t, "SP-Z45", 8, map[int64]int{r.small: 2, r.big: 2, r.totes: 2})
	for i, bt := range []int64{r.small, r.small, r.big, r.big, r.totes, r.totes} {
		landAt(t, r.db, bt, z45Kids[i].ID, fmt.Sprintf("SP-Z45-%d", i))
	}
	sm, smKids := kanbanCostFlat(t, r.db, "SP-SM", 4)
	landAt(t, r.db, r.small, smKids[0].ID, "SP-SM-1")
	testdb.KanbanSources(t, r.db, r.k.ID, z45.ID)
	r.level(t, r.k, r.small, 1)
	r.level(t, r.k, r.big, 1)
	r.level(t, r.k, r.totes, 1)
	testdb.KanbanCover(t, r.db, r.k.ID, 4)
	coverPart(t, r.db, "SPR2-S", r.small)
	coverPart(t, r.db, "SPR2-T", r.totes)
	coverProcess(t, r.db, "SPR2", coverStyle{"SMALL", true, [][2]string{{"SP-K-SUPPORT", "SPR2-S"}}},
		coverStyle{"TOTES", false, [][2]string{{"SP-K-SUPPORT", "SPR2-T"}}})
	for i := 0; i < 5; i++ {
		r.stand(t, r.small)
	}
	r.stand(t, r.big)
	r.stand(t, r.totes)
	r.tick(t)
	if got := r.moves(t); len(got) != 0 {
		t.Fatalf("steady under small: %d moves", len(got))
	}

	id := "c0c0c0c0-0000-4000-8000-000000000221"
	testutil.MustNoErr(t, r.db.UpsertDemandOrigin(store.DemandOrigin{OriginID: id, Revision: 1, EpisodeKey: "co|" + id,
		Kind: protocol.EpisodeKindChangeover, StationID: "sp-edge", ProcessID: "SPR2", CoreNodeName: "SP-K-SUPPORT",
		PayloadCode: "SPR2-T", OpenedAt: time.Now().UTC()}), "open the changeover")
	m := r.tick(t)
	tt := mntState(t, m, "SP-K", "SP-TOTES")
	if tt.Want != 5 || tt.Gap != 4 || tt.Created != 1 {
		t.Errorf("totes at START: %+v, want want 5, gap 4, one ask into the free child", tt)
	}
	got := r.moves(t)
	if len(got) != 3 {
		t.Fatalf("%d moves at START, want 3: four windows free, one kept", len(got))
	}
	for _, o := range got {
		if !strings.HasPrefix(o.DeliveryNode, "SP-W") {
			t.Errorf("delivered to %s, want a window", o.DeliveryNode)
		}
		r.land(t, o)
	}

	// Three lit, one asked: the gap is spoken for, and small's last spare stays.
	m.Tick()
	tt = mntState(t, m, "SP-K", "SP-TOTES")
	if tt.Gap != 3 || tt.Created != 0 {
		t.Errorf("totes with three at the windows: %+v, want gap 3 and no ask", tt)
	}
	if n := len(r.moves(t)); n != 3 {
		t.Errorf("%d moves, want still 3: nothing is left short", n)
	}
	var toSM int
	testutil.MustNoErr(t, r.db.QueryRow(`SELECT COUNT(*) FROM orders o JOIN nodes n ON n.name = o.delivery_node
		WHERE n.parent_id = $1`, sm.ID).Scan(&toSM), "orders to SM")
	if toSM != 0 {
		t.Errorf("%d orders to the plain market", toSM)
	}
}

// TestKanbanSurplus_R3Shape walks a floor-leaving changeover at the tick, on the shape the rig
// had with lenders (plants/kanban-press.yaml has since gone to one lineside kanban). K 8
// children, 1 + 1 + 1 + cover 4 on the running type; Z45 at 2/2/2; Z32 at 6; the spare zone
// plain and K's overflow and last source.
func TestKanbanSurplus_R3Shape(t *testing.T) {
	t.Parallel()
	r := newSurplusRig(t, 8)
	z45, z45Kids := r.zone(t, "SP-Z45", 8, map[int64]int{r.small: 2, r.big: 2, r.totes: 2})
	z32, z32Kids := r.zone(t, "SP-Z32", 8, map[int64]int{r.t32: 6})
	spare, _ := kanbanCostFlat(t, r.db, "SP-SPARE", 10)
	testdb.KanbanSources(t, r.db, r.k.ID, z45.ID, z32.ID, spare.ID)
	testdb.KanbanOverflow(t, r.db, r.k.ID, spare.Name)
	r.level(t, r.k, r.small, 1)
	r.level(t, r.k, r.big, 1)
	r.level(t, r.k, r.totes, 1)
	r.level(t, r.k, r.t32, 0)
	testdb.KanbanCover(t, r.db, r.k.ID, 4)
	coverPart(t, r.db, "SPR-T", r.totes)
	coverPart(t, r.db, "SPR-32", r.t32)
	coverProcess(t, r.db, "SPR", coverStyle{"TOTES", true, [][2]string{{"SP-K-SUPPORT", "SPR-T"}}},
		coverStyle{"C32", false, [][2]string{{"SP-K-SUPPORT", "SPR-32"}}})
	// Under totes: K small 1, big 1, totes 5, one child free. The lenders whole.
	r.stand(t, r.small)
	r.stand(t, r.big)
	var kTotes []int64
	for i := 0; i < 5; i++ {
		kTotes = append(kTotes, r.stand(t, r.totes))
	}
	for i, bt := range []int64{r.small, r.small, r.big, r.big, r.totes, r.totes} {
		landAt(t, r.db, bt, z45Kids[i].ID, fmt.Sprintf("SP-Z45-%d", i))
	}
	for i := 0; i < 5; i++ { // the sixth is the 32x30 K asks for
		landAt(t, r.db, r.t32, z32Kids[i].ID, fmt.Sprintf("SP-Z32-%d", i))
	}
	r.tick(t)
	if got := r.moves(t); len(got) != 0 {
		t.Fatalf("steady under totes: %d moves", len(got))
	}

	// START KP-32. 32x30 wants 4. One child free: one ask, three short with none free.
	// Totes are 4 above their want, and Z45 is whole, and Z32 declares no totes, and the
	// spare zone is plain: the arm waits, K holds 7 of 45x48, and says why.
	id := "c0c0c0c0-0000-4000-8000-000000000321"
	testutil.MustNoErr(t, r.db.UpsertDemandOrigin(store.DemandOrigin{OriginID: id, Revision: 1, EpisodeKey: "co|" + id,
		Kind: protocol.EpisodeKindChangeover, StationID: "sp-edge", ProcessID: "SPR", CoreNodeName: "SP-K-SUPPORT",
		PayloadCode: "SPR-32", OpenedAt: time.Now().UTC()}), "open the changeover")
	m := r.tick(t)
	c, tt := mntState(t, m, "SP-K", "SP-32"), mntState(t, m, "SP-K", "SP-TOTES")
	if c.Want != 4 || c.Created != 1 || c.Gap != 4 {
		t.Errorf("32x30 under KP-32: %+v, want want 4, one ask into the one free child", c)
	}
	if tt.Resident != 5 || tt.Want != 1 || !strings.Contains(tt.SurplusCause, "no source of SP-K has room") {
		t.Errorf("totes under KP-32: %+v, want 5 resident of 1 wanted, waiting with the cause", tt)
	}
	if got := r.moves(t); len(got) != 0 {
		t.Fatalf("Z45 whole, spare plain: %d moves; the spare zone is reached by doors and sources only", len(got))
	}

	// Z45 short of totes by 2 (K borrowed): two go home.
	_, err := r.db.Exec(`DELETE FROM bins WHERE label IN ('SP-Z45-4', 'SP-Z45-5')`)
	testutil.MustNoErr(t, err, "Z45 lends its totes")
	r.tick(t)
	got := r.moves(t)
	if len(got) != 2 || surplusCart(t, got[0]) != kTotes[0] || surplusCart(t, got[1]) != kTotes[1] {
		t.Fatalf("moves %+v, want K's two oldest totes", got)
	}
	for _, o := range got {
		dest, err := r.db.GetNodeByDotName(o.DeliveryNode)
		testutil.MustNoErr(t, err, "slot")
		if dest.ParentID == nil || *dest.ParentID != z45.ID {
			t.Errorf("delivered to %s, want Z45", o.DeliveryNode)
		}
	}
}

// TestKanbanSurplus_R3Return_TheLastCartOnlyLeavesWithKFull is the cutover back: K 8/8
// with one 32x30 above its want. It moves only while totes read short with no child
// free, which is the positions' legs holding K's totes before a robot lifts them. Once
// the children are free the asks fill them, nothing reads short, and it stays.
func TestKanbanSurplus_R3Return_TheLastCartOnlyLeavesWithKFull(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (*surplusRig, []int64) {
		r := newSurplusRig(t, 8)
		z32, _ := r.zone(t, "SP-Z32", 8, map[int64]int{r.t32: 6})
		z45, z45Kids := r.zone(t, "SP-Z45", 8, map[int64]int{r.totes: 2})
		for i := 0; i < 2; i++ {
			landAt(t, r.db, r.totes, z45Kids[i].ID, fmt.Sprintf("SP-Z45-%d", i))
		}
		testdb.KanbanSources(t, r.db, r.k.ID, z45.ID, z32.ID)
		r.level(t, r.k, r.small, 1)
		r.level(t, r.k, r.big, 1)
		r.level(t, r.k, r.totes, 5)
		r.level(t, r.k, r.t32, 0)
		r.stand(t, r.small)
		r.stand(t, r.big)
		r.stand(t, r.t32)
		var totes []int64
		for i := 0; i < 5; i++ {
			totes = append(totes, r.stand(t, r.totes))
		}
		r.tick(t)
		if got := r.moves(t); len(got) != 0 {
			t.Fatalf("K whole on totes: %d moves with nothing short", len(got))
		}
		return r, totes
	}
	t.Run("a leg holds a cart and no child is free", func(t *testing.T) {
		t.Parallel()
		r, totes := setup(t)
		testdb.ClaimBinForTest(t, r.db, totes[0], dispatched(t, r.db, "sp-leg"))
		r.tick(t)
		if got := r.moves(t); len(got) != 1 || !strings.HasPrefix(got[0].DeliveryNode, "SP-Z32") {
			t.Fatalf("moves %+v, want the 32x30 to Z32", got)
		}
	})
	t.Run("the carts already lifted", func(t *testing.T) {
		t.Parallel()
		r, totes := setup(t)
		_, err := r.db.Exec(`DELETE FROM bins WHERE id = ANY($1)`, totes[:2])
		testutil.MustNoErr(t, err, "two lifted")
		st := mntState(t, r.tick(t), "SP-K", "SP-32")
		if got := r.moves(t); len(got) != 0 {
			t.Fatalf("moves %+v: a child is free for each short cart, so the arm has no business", got)
		}
		if st.Resident != 1 || st.Gap != -1 || st.SurplusCause != "" {
			t.Errorf("the 32x30's line: %+v: it stands above its want, Z32 short, and nothing says so", st)
		}
	})
}

// dispatched is an order a robot has, which is what a hard claim belongs to.
func dispatched(t *testing.T, db *store.DB, uuid string) int64 {
	t.Helper()
	return testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = uuid
		o.Status = protocol.StatusDispatched
	}).ID
}

// surplusCart is the one cart an order of the arm's carries, 0 for none.
func surplusCart(t *testing.T, o *orders.Order) int64 {
	t.Helper()
	if c := testdb.OnlyCart(t, o); c != nil {
		return *c
	}
	return 0
}
