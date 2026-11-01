//go:build docker

package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/internal/testdb"
	"shingocore/messaging"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// kanban_pins_cost_docker_test.go — what the four kanban paths cost today, in
// statements (store.OpenCounting), on one plant shape: a lineside group K of 8
// flat positions with four levels and four supported positions, its overflow
// Z45, a 32x30 zone Z32, a second lineside group K2, a plain market SM with
// lanes, and an unloader of four windows whose outbound is K.
//
// Each const is the number measured at this tree, asserted exactly so a read added
// or removed is a decision.
const (
	// keeperTickSteady is one steady tick, nothing short, whatever the plant's size:
	// the picture, the open episodes with their asks, the comings, the residents.
	keeperTickSteady = 4
	// keeperTickBurstToFirstAsk is a tick with one type short by 4, up to the first
	// ask's transaction: the steady four, the read of the unloaders' windows, the
	// forecast, the episode's open, the group's properties, slot state and children's
	// types, and the two node reads of the ask's admission. Each ask after it chooses
	// its slot with no statement.
	keeperTickBurstToFirstAsk = 12
	// surplusArmNoWindow is a tick the surplus arm's precondition holds on with one
	// window open: the steady four, the windows, the forecast, the episode's open, the
	// slot state, the properties and types the choice reads. The windows' read says one
	// is open, so nothing more.
	surplusArmNoWindow = 10
	// surplusArmToFirstMove is the same tick with two windows open, up to the move's
	// transaction: that, the surplus list, freeWindows' three reads a window, the
	// loader's capability list, the window's Allowed Bin Types and the cart's type row,
	// and the move's plan.
	surplusArmToFirstMove = 28
	// snapshotBuild is one bins feed build for the Edge watching the windows: the
	// read snapshot (5: BEGIN, the names, the orders inbound, the bins, COMMIT), the
	// picture of its groups, then UnloaderGroupLines (open episodes, types in play,
	// comings, residents), however many groups and levels.
	snapshotBuild = 10
	// snapshotBodyBytes is that snapshot's body as it goes to the Edge with every cart
	// lit for K: three assignments of a type (114 over the base's 1000, K whole and none
	// said), and the fields a source's row carries cost no byte here.
	snapshotBodyBytes = 1114
	// snapshotBodyBytesWhole is it with K whole and nothing short: the base's 1000, and
	// an assignment for each of the three carts saying so (146 of them, none_short's
	// own key 18 a row). Only rows that carry the field pay.
	snapshotBodyBytesWhole = 1146
	// intakeDoorToASource is the intake door up to the order's birth, for an empty
	// pushed at K with K whole, into Z45 short of that type, however many of Z45's
	// positions are free: the cart, K's node, the door's one read, Z45's properties,
	// slot state and children's types, and the two node reads of the issue's window.
	intakeDoorToASource = 8
	// intakeDoorNoWindow is the same door for a move at K from a node that is no
	// window: the cap's path, into K's overflow Z45. It pays the door's one read to
	// learn that, and takes the overflow over one read of Z45. Measured before this
	// work, with the overflow's name and the cart read twice: 45.
	intakeDoorNoWindow = 15
	// parkedArm is a scanner pass with one push parked at K that Z45 takes, however
	// many of its positions are free, from the pass's start to the re-aim: the order
	// list and read, K's node, the cart, the door's one read, Z45's properties, slot
	// state and children's types.
	parkedArm = 8
	// parkedPass is that whole pass: the arm, then the acquire's lane admission and
	// the hand to the fleet. Nothing in the tail is this work's; the second gate and
	// resolve the arm used to leave in it went with the arm. 59 on order-carts' rows: the
	// tail reads an order's cart as 4 SELECT order_bins where it read 5 SELECT orders.
	parkedPass = 59
	// cancelReturnDoor is the cancel-return's store of a window's cancelled push into
	// K, Z45 short: the position's loader, the door's one read, Z45's three.
	cancelReturnDoor = 5
)

// kanbanCostRig is the shape, on a database the counting engine reads.
type kanbanCostRig struct {
	db      *store.DB
	eng     *Engine
	counter *store.QueryCounter
	small   int64
	big     int64
	totes   int64
	t32     int64
	k, z45  *nodes.Node
	k2, z32 *nodes.Node
	windows []*nodes.Node
	push    int64 // the empty small standing at windows[1]
}

// kanbanCostFlat is a flat group of n positions.
func kanbanCostFlat(t *testing.T, db *store.DB, name string, n int) (*nodes.Node, []*nodes.Node) {
	t.Helper()
	id, err := nodes.CreateGroup(db.DB, name)
	testutil.MustNoErr(t, err, "group "+name)
	testdb.MarkGroup(t, db, id)
	var kids []*nodes.Node
	for i := 0; i < n; i++ {
		c := &nodes.Node{Name: fmt.Sprintf("%s-P%d", name, i+1), Enabled: true, ParentID: &id}
		testutil.MustNoErr(t, db.CreateNode(c), "position")
		kids = append(kids, c)
	}
	g, err := db.GetNode(id)
	testutil.MustNoErr(t, err, "read group "+name)
	return g, kids
}

// newKanbanCostRig builds the shape. z45Residents says how many of each 45x48
// type stand in Z45: small, big, totes, whatever its levels of 2 each say.
func newKanbanCostRig(t *testing.T, z45Small, z45Big, z45Totes int) *kanbanCostRig {
	t.Helper()
	db, cfg := testdb.OpenWithConfig(t)
	r := &kanbanCostRig{db: db}
	typ := func(code string, w, l, h float64) int64 {
		var id int64
		testutil.MustNoErr(t, db.QueryRow(
			`INSERT INTO bin_types (code, width_in, length_in, height_in) VALUES ($1,$2,$3,$4) RETURNING id`,
			code, w, l, h).Scan(&id), "type "+code)
		return id
	}
	r.small, r.big = typ("KC-SMALL", 45, 48, 25), typ("KC-BIG", 45, 48, 34)
	r.totes, r.t32 = typ("KC-TOTES", 45, 48, 6), typ("KC-32X30", 32, 30, 25)

	level := func(g *nodes.Node, bt int64, want int) {
		testutil.MustNoErr(t, db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: g.ID, BinTypeID: bt, Want: want}), "level")
	}
	stand := func(bt int64, at []*nodes.Node, label string) []*nodes.Node {
		for i, n := range at {
			landAt(t, db, bt, n.ID, fmt.Sprintf("%s-%d", label, i+1))
		}
		return at
	}
	keeper := func(g *nodes.Node, supports ...string) {
		testdb.KanbanStation(t, db, g.ID, "kc-edge")
		var ids []int64
		for _, name := range supports {
			n := &nodes.Node{Name: name, Enabled: true}
			testutil.MustNoErr(t, db.CreateNode(n), "supported position")
			ids = append(ids, n.ID)
		}
		testutil.MustNoErr(t, nodes.SetKanbanSupports(db.DB, g.ID, ids), "supports")
	}

	var kids []*nodes.Node
	r.k, kids = kanbanCostFlat(t, db, "KC-K", 8)
	keeper(r.k, "KC-P6-LF", "KC-P6-LB", "KC-P6-RF", "KC-P6-RB")
	level(r.k, r.small, 1)
	level(r.k, r.big, 1)
	level(r.k, r.totes, 1)
	level(r.k, r.t32, 0)
	stand(r.small, kids[:1], "KC-K-S")
	stand(r.big, kids[1:2], "KC-K-B")
	stand(r.totes, kids[2:3], "KC-K-T")

	r.k2, kids = kanbanCostFlat(t, db, "KC-K2", 4)
	keeper(r.k2, "KC-WELD-L", "KC-WELD-R")
	level(r.k2, r.small, 1)
	stand(r.small, kids[:1], "KC-K2-S")

	// Z45 and Z32 declare levels and name no station, so they run no keeper.
	r.z45, kids = kanbanCostFlat(t, db, "KC-Z45", 8)
	level(r.z45, r.small, 2)
	level(r.z45, r.big, 2)
	level(r.z45, r.totes, 2)
	stand(r.small, kids[:z45Small], "KC-Z45-S")
	stand(r.big, kids[z45Small:z45Small+z45Big], "KC-Z45-B")
	stand(r.totes, kids[z45Small+z45Big:z45Small+z45Big+z45Totes], "KC-Z45-T")
	testdb.KanbanOverflow(t, db, r.k.ID, r.z45.Name)
	r.z32, kids = kanbanCostFlat(t, db, "KC-Z32", 8)
	level(r.z32, r.t32, 6)
	stand(r.t32, kids[:6], "KC-Z32")
	testdb.KanbanSources(t, db, r.k.ID, r.z45.ID, r.z32.ID)

	// Plain markets: lanes, no level, each lane one more synthetic node, which the
	// tick no longer pays for.
	for _, m := range []string{"KC-SM", "KC-M2", "KC-M3"} {
		_, _, slots := laneGroup(t, db, m, 6, 3)
		if m == "KC-SM" {
			landAt(t, db, r.small, slots[0][2].ID, "KC-SM-SPARE-S")
			landAt(t, db, r.big, slots[1][2].ID, "KC-SM-SPARE-B")
		}
	}

	lid, err := db.CreateLoader(loaders.Loader{Name: "KC-UNL", Role: loaders.RoleConsume,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator, OutboundDest: r.k.Name})
	testutil.MustNoErr(t, err, "unloader")
	for i := 0; i < 4; i++ {
		w := &nodes.Node{Name: fmt.Sprintf("KC-W%d", i+1), Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(w), "window")
		testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: lid, PositionNodeID: w.ID, SortOrder: i}), "window row")
		r.windows = append(r.windows, w)
	}
	full := landAt(t, db, r.small, r.windows[0].ID, "KC-W1-FULL")
	testutil.MustNoErr(t, db.SetBinManifest(full, `{"items": []}`, "KC-PART", 10), "fill the window's cart")
	r.push = landAt(t, db, r.small, r.windows[1].ID, "KC-W2-EMPTY")
	landAt(t, db, r.big, r.windows[2].ID, "KC-W3-EMPTY")

	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "counting db")
	t.Cleanup(func() { cdb.Close() })
	r.counter = counter
	r.eng = newUnstartedEngine(t, cdb, testdb.NewTrackingBackend())
	resolver := &dispatch.DefaultResolver{DB: cdb}
	r.eng.dispatcher = r.eng.newDispatcher(&dispatchEmitter{bus: r.eng.Events, engine: r.eng}, resolver)
	r.eng.startFulfillment(resolver)
	return r
}

// inGroup reports whether a node name is a position of group g.
func (r *kanbanCostRig) inGroup(t *testing.T, name string, g *nodes.Node) bool {
	t.Helper()
	n, err := r.db.GetNodeByDotName(name)
	testutil.MustNoErr(t, err, "read "+name)
	return n != nil && n.ParentID != nil && *n.ParentID == g.ID
}

// reAim is the index of the statement that re-aims an order's delivery_node,
// len(stmts) with none.
func reAim(stmts []string) int {
	for i, s := range stmts {
		if strings.HasPrefix(strings.TrimSpace(s), "UPDATE orders SET delivery_node") {
			return i
		}
	}
	return len(stmts)
}

// TestKanbanCost_KeeperTick: one steady tick is four statements whatever the
// plant's size: the same with more kanbans, a station on the two with no supports,
// and one kanban running as a keeper more. Then the burst: one type short by 4.
func TestKanbanCost_KeeperTick(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 2, 2, 2)
	m := r.eng.KanbanKeeper()

	r.counter.Reset()
	m.Tick()
	n := r.counter.Count()
	t.Logf("STATEMENTS: keeper tick, steady: %d\n%s", n, testutil.StatementBreakdown(r.counter.Statements()))
	for _, st := range m.Snapshot() {
		if st.Gap > 0 || st.Created != 0 || st.OriginID != "" {
			t.Fatalf("fixture drift: the tick is not steady: %+v", st)
		}
	}
	if len(m.Snapshot()) != 9 {
		t.Fatalf("fixture drift: %d lines, want K's 4, K2's 1, Z45's 3 and Z32's 1: %+v", len(m.Snapshot()), m.Snapshot())
	}
	if n != keeperTickSteady {
		t.Errorf("keeper tick = %d statements, want %d", n, keeperTickSteady)
	}

	for _, g := range []*nodes.Node{r.z45, r.z32} {
		testdb.KanbanStation(t, r.db, g.ID, "kc-edge")
	}
	more, _ := kanbanCostFlat(t, r.db, "KC-K3", 4)
	testdb.KanbanKeeper(t, r.db, more.ID, "kc-edge")
	testutil.MustNoErr(t, r.db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: more.ID, BinTypeID: r.small, Want: 0}), "a level")
	r.counter.Reset()
	m.Tick()
	if got := r.counter.Count(); got != keeperTickSteady {
		t.Errorf("the same tick with more of the plant: %d statements, want %d\n%s", got, keeperTickSteady,
			testutil.StatementBreakdown(r.counter.Statements()))
	}

	// The burst: K wants 5 of small with 1 standing, four asks.
	testutil.MustNoErr(t, r.db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: r.k.ID, BinTypeID: r.small, Want: 5}), "the burst")
	r.counter.Reset()
	m.Tick()
	got := r.counter.Count()
	if st := mntState(t, m, "KC-K", "KC-SMALL"); st.Created != 4 {
		t.Fatalf("fixture drift: the burst asked %d: %+v", st.Created, st)
	}
	stmts := r.counter.Statements()
	t.Logf("STATEMENTS: keeper tick, one type short by 4: %d, the first %d to the first ask's birth\n%s",
		got, testutil.FirstBegin(stmts), testutil.StatementBreakdown(stmts))
	if n := testutil.FirstBegin(stmts); n != keeperTickBurstToFirstAsk {
		t.Errorf("the burst to the first ask = %d statements, want %d", n, keeperTickBurstToFirstAsk)
	}
	for what, one := range map[string]string{"slot state": "group-slot-state", "children's types": "up(child, id, parent_id, depth)",
		"the group's properties": "FROM node_properties WHERE node_id=$1 ORDER BY key"} {
		n := 0
		for _, s := range stmts {
			if strings.Contains(s, one) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("the group's %s read %d times over four asks, want once", what, n)
		}
	}
}

// TestKanbanCost_SurplusArm: what a tick costs with the arm's precondition true. K's 8
// positions all held, 5 of them big above its want, totes short by 1 and sourceable.
func TestKanbanCost_SurplusArm(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 2, 2, 2)
	kids, err := r.db.ListChildNodes(r.k.ID)
	testutil.MustNoErr(t, err, "positions")
	for i, c := range kids[3:] {
		landAt(t, r.db, r.big, c.ID, fmt.Sprintf("KC-OVER-%d", i))
	}
	testutil.MustNoErr(t, r.db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: r.k.ID, BinTypeID: r.totes, Want: 2}), "totes short")
	m := r.eng.KanbanKeeper()

	// One window open (W4): the read says so, and the arm reads nothing to learn it.
	r.counter.Reset()
	m.Tick()
	stmts := r.counter.Statements()
	t.Logf("STATEMENTS: surplus arm, one window open: %d\n%s", len(stmts), testutil.StatementBreakdown(stmts))
	if st := mntState(t, m, "KC-K", "KC-BIG"); !strings.Contains(st.SurplusCause, "last one free") {
		t.Fatalf("fixture drift: %+v", st)
	}
	if len(stmts) != surplusArmNoWindow {
		t.Errorf("the arm with one window open = %d statements, want %d", len(stmts), surplusArmNoWindow)
	}

	// Two open: one move.
	_, err = r.db.Exec(`DELETE FROM bins WHERE label = 'KC-W3-EMPTY'`)
	testutil.MustNoErr(t, err, "free a window")
	r.counter.Reset()
	m.Tick()
	stmts = r.counter.Statements()
	n := testutil.FirstBegin(stmts)
	t.Logf("STATEMENTS: surplus arm, two open: %d, the first %d to the move's birth\n%s", len(stmts), n, testutil.StatementBreakdown(stmts))
	var moves int
	testutil.MustNoErr(t, r.db.QueryRow(`SELECT COUNT(*) FROM orders WHERE edge_uuid LIKE 'core-sp-%'`).Scan(&moves), "moves")
	if moves != 1 {
		t.Fatalf("fixture drift: %d moves, want 1", moves)
	}
	if n != surplusArmToFirstMove {
		t.Errorf("the arm to the move's birth = %d statements, want %d", n, surplusArmToFirstMove)
	}
}

// TestKanbanCost_SnapshotBuild: one bins feed build for the Edge watching the
// unloader's windows, cut out of the watch list's handling: the build runs from
// its read snapshot's BEGIN to the outbox write that sends what it built.
func TestKanbanCost_SnapshotBuild(t *testing.T) {
	t.Parallel()
	t.Run("every cart lit for K", func(t *testing.T) {
		t.Parallel()
		r := newKanbanCostRig(t, 2, 2, 2)
		for bt, want := range map[int64]int{r.small: 3, r.big: 2} {
			testutil.MustNoErr(t, r.db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: r.k.ID, BinTypeID: bt, Want: want}), "K short")
		}
		kanbanCostSnapshot(t, r, snapshotBuild, snapshotBodyBytes)
	})
	t.Run("K whole, nothing short", func(t *testing.T) {
		t.Parallel()
		kanbanCostSnapshot(t, newKanbanCostRig(t, 2, 2, 2), snapshotBuild, snapshotBodyBytesWhole)
	})
}

// kanbanCostSnapshot asserts one build's statements and its body's bytes.
func kanbanCostSnapshot(t *testing.T, r *kanbanCostRig, wantBuild, wantBytes int) {
	t.Helper()
	svc := messaging.NewCoreDataService(r.eng.db, messaging.NewCoreHandler(r.eng.db, nil, "core", "shingo.dispatch", nil),
		service.EpochAnnounce{})
	svc.SetGroupLinesFunc(r.eng.UnloaderGroupLines)
	names := []string{}
	for _, w := range r.windows {
		names = append(names, w.Name)
	}
	digest, err := protocol.BinWatchDigest(names)
	testutil.MustNoErr(t, err, "watch digest")

	r.counter.Reset()
	svc.HandleBinWatch(&protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: "kc-edge"}},
		&protocol.BinWatchList{Digest: digest, Nodes: names})
	stmts := r.counter.Statements()
	from := testutil.FirstBegin(stmts)
	to := len(stmts)
	for i := from; i < len(stmts); i++ {
		if strings.Contains(strings.ToLower(stmts[i]), "into outbox") {
			to = i
			break
		}
	}
	build := stmts[from:to]
	t.Logf("STATEMENTS: the watch list handled whole: %d; the build: %d\n%s", len(stmts), len(build),
		testutil.StatementBreakdown(build))
	if to == len(stmts) {
		t.Fatalf("fixture drift: no snapshot reached the outbox:\n%s", strings.Join(stmts, "\n"))
	}
	if len(build) != wantBuild {
		t.Errorf("snapshot build = %d statements, want %d", len(build), wantBuild)
	}
	var sent []byte
	testutil.MustNoErr(t, r.db.QueryRow(`SELECT payload FROM outbox WHERE msg_type = $1`,
		"data."+protocol.SubjectBinsSnapshot).Scan(&sent), "the snapshot off the outbox")
	var env protocol.Envelope
	testutil.MustNoErr(t, json.Unmarshal(sent, &env), "the envelope")
	var data protocol.Data
	testutil.MustNoErr(t, env.DecodePayload(&data), "the data")
	t.Logf("BYTES: the snapshot to the Edge: %d (the envelope around it: %d)", len(data.Body), len(sent))
	if len(data.Body) != wantBytes {
		t.Errorf("snapshot body = %d bytes, want %d: the Pi is sent nothing new\n%s", len(data.Body), wantBytes, data.Body)
	}
}

// TestKanbanCost_NoWindowDoor: a move at K at its level from a node that is no window
// keeps the cap, and its door costs less than it did.
func TestKanbanCost_NoWindowDoor(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 1, 2, 2)
	line := &nodes.Node{Name: "KC-LINE", Enabled: true}
	testutil.MustNoErr(t, r.db.CreateNode(line), "a line")
	bin := landAt(t, r.db, r.small, line.ID, "KC-LINE-CART")
	r.counter.Reset()
	o, _, lerr := r.eng.dispatcher.Lifecycle().CreateInboundOrder("kc-edge", &protocol.OrderRequest{
		OrderUUID: "kc-line-push", OrderType: dispatch.OrderTypeMove, Quantity: 1, OriginClass: protocol.OriginClassNoDemand,
		SourceNode: line.Name, DeliveryNode: r.k.Name, BinID: bin}, nil)
	if lerr != nil {
		t.Fatalf("intake refused the move: %s", lerr.Detail)
	}
	if !r.inGroup(t, o.DeliveryNode, r.z45) {
		t.Fatalf("fixture drift: the move went to %s, want K's overflow", o.DeliveryNode)
	}
	stmts := r.counter.Statements()
	door := stmts[:testutil.FirstBegin(stmts)]
	t.Logf("STATEMENTS: the door %d\n%s", len(door), testutil.StatementBreakdown(door))
	if len(door) != intakeDoorNoWindow {
		t.Errorf("no-window door = %d statements, want %d", len(door), intakeDoorNoWindow)
	}
}

// TestKanbanCost_CancelReturn: the cancel-return's store of a window's cancelled push.
func TestKanbanCost_CancelReturn(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 1, 2, 2)
	bin, err := r.db.GetBin(r.push)
	testutil.MustNoErr(t, err, "the cart")
	carrier := &orders.Order{SourceIntent: dispatch.SourceIntentLocal, SourceNode: r.windows[1].Name, NamedBinID: &r.push}
	r.counter.Reset()
	slot, why := r.eng.storeInto(r.k, bin, carrier)
	if slot == nil || !r.inGroup(t, slot.Name, r.z45) {
		t.Fatalf("fixture drift: the return took %+v (%s)", slot, why)
	}
	stmts := r.counter.Statements()
	t.Logf("STATEMENTS: the store %d\n%s", len(stmts), testutil.StatementBreakdown(stmts))
	if len(stmts) != cancelReturnDoor {
		t.Errorf("cancel-return door = %d statements, want %d", len(stmts), cancelReturnDoor)
	}
}

// TestKanbanCost_IntakeDoor: an empty off a window, pushed at K with K whole, up to
// its birth in Z45. Z45 short of the type (its residents stand on five positions),
// and all free: the same number, since no read is per child.
func TestKanbanCost_IntakeDoor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		small, big, totes int
		want              int
	}{
		{"Z45 short of the type", 1, 2, 2, intakeDoorToASource},
		{"Z45 all free", 0, 0, 0, intakeDoorToASource},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newKanbanCostRig(t, tc.small, tc.big, tc.totes)
			r.counter.Reset()
			o, _, lerr := r.eng.dispatcher.Lifecycle().CreateInboundOrder("kc-edge", &protocol.OrderRequest{
				OrderUUID: "kc-push", OrderType: dispatch.OrderTypeMove, Quantity: 1,
				OriginClass: protocol.OriginClassNoDemand,
				SourceNode:  r.windows[1].Name, DeliveryNode: r.k.Name, BinID: r.push}, nil)
			if lerr != nil {
				t.Fatalf("intake refused the push: %s", lerr.Detail)
			}
			if !r.inGroup(t, o.DeliveryNode, r.z45) {
				t.Fatalf("fixture drift: the push went to %s, want a position of %s", o.DeliveryNode, r.z45.Name)
			}
			stmts := r.counter.Statements()
			door := stmts[:testutil.FirstBegin(stmts)]
			t.Logf("STATEMENTS: %s: the door %d (with the birth %d)\n%s", tc.name, len(door), len(stmts),
				testutil.StatementBreakdown(door))
			if len(door) != tc.want {
				t.Errorf("intake door = %d statements, want %d", len(door), tc.want)
			}
		})
	}
}

// TestKanbanCost_ScannerParkedPass: a push parked at K, on the pass Z45 takes it. K
// has no source and no overflow on the pass that parks it, so the measured pass is
// the first with anywhere to go. Z45 short of the type, and all free.
func TestKanbanCost_ScannerParkedPass(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		small, big, totes int
		arm, pass         int
	}{
		{"Z45 short of the type", 1, 2, 2, parkedArm, parkedPass},
		{"Z45 all free", 0, 0, 0, parkedArm, parkedPass},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newKanbanCostRig(t, tc.small, tc.big, tc.totes)
			testdb.KanbanOverflow(t, r.db, r.k.ID, "")
			testdb.KanbanSources(t, r.db, r.k.ID)
			push := testdb.CreateOrder(t, r.db, func(o *orders.Order) {
				o.EdgeUUID = "kc-parked"
				o.StationID = "kc-edge"
				o.OrderType = dispatch.OrderTypeMove
				o.SourceIntent = dispatch.SourceIntentLocal
				o.OriginClass = protocol.OriginClassNoDemand
				o.SourceNode = r.windows[1].Name
				o.DeliveryNode = r.k.Name
				o.NamedBinID = &r.push
			})
			testutil.MustNoErr(t, reservations.Acquire(r.db.DB, push.ID, push.ID, r.push, "test"), "the push holds its cart")
			testdb.HoldCart(t, r.db, push.ID, r.push)

			r.eng.fulfillment.RunOnce()
			parked, err := r.db.GetOrder(push.ID)
			testutil.MustNoErr(t, err, "reload the push")
			if parked.Status != protocol.StatusQueued || parked.QueueCause != string(dispatch.CauseNGRPAtLevel) {
				t.Fatalf("fixture drift: the push is %s (%s), want parked at K's level", parked.Status, parked.QueueCause)
			}

			testdb.KanbanSources(t, r.db, r.k.ID, r.z45.ID, r.z32.ID)
			r.counter.Reset()
			r.eng.fulfillment.RunOnce()
			stmts := r.counter.Statements()
			arm := reAim(stmts)
			got, err := r.db.GetOrder(push.ID)
			testutil.MustNoErr(t, err, "reload the push")
			if got.Status != protocol.StatusDispatched || !r.inGroup(t, got.DeliveryNode, r.z45) {
				t.Fatalf("fixture drift: the push is %s to %s (%s), want dispatched into %s",
					got.Status, got.DeliveryNode, got.QueueCause, r.z45.Name)
			}
			t.Logf("STATEMENTS: %s: to the re-aim %d, the pass %d\n%s\nto the re-aim:\n%s", tc.name, arm, len(stmts),
				testutil.StatementBreakdown(stmts), testutil.StatementBreakdown(stmts[:arm]))
			if arm != tc.arm {
				t.Errorf("parked arm = %d statements, want %d", arm, tc.arm)
			}
			if len(stmts) != tc.pass {
				t.Errorf("parked pass = %d statements, want %d", len(stmts), tc.pass)
			}
		})
	}
}
