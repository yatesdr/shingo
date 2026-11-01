//go:build docker

package store_test

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/plantclaims"
)

// kanban_counts_docker_test.go — the batched reads the keeper's tick and a snapshot
// build make are the single reads they replaced, on a plant with every population:
// the residents CountEmptyBinsOfTypeInGroup counts and each kind it does not, the
// comings ListTypedInboundToGroup lists and each kind it does not, an episode's
// asks and its oldest parked one.

func TestKanbanCounts_TheBatchedReadsAgreeWithTheSingleReads(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := db.Exec(q, args...)
		testutil.MustNoErr(t, err, q)
	}
	typ := func(code string, totes any) int64 {
		t.Helper()
		var id int64
		testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code, totes_per_layer) VALUES ($1, $2) RETURNING id`,
			code, totes).Scan(&id), "type "+code)
		return id
	}
	tote, kd, other := typ("KCT-TOTE", 8), typ("KCT-KD", nil), typ("KCT-OTHER", nil)
	var bare int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code, bare_of) VALUES ('KCT-BARE', $1) RETURNING id`, kd).Scan(&bare), "bare type")

	group := func(name string, slots int) (*nodes.Node, []*nodes.Node) {
		t.Helper()
		id := testdb.MarkedGroup(t, db, name)
		var kids []*nodes.Node
		for i := 0; i < slots; i++ {
			n := &nodes.Node{Name: name + "-S" + string(rune('A'+i)), Enabled: true, ParentID: id}
			testutil.MustNoErr(t, db.CreateNode(n), "slot")
			kids = append(kids, n)
		}
		g, err := db.GetNode(*id)
		testutil.MustNoErr(t, err, "group")
		return g, kids
	}
	k1, s1 := group("KCT-K1", 12)
	k2, s2 := group("KCT-K2", 3)
	bin := func(bt int64, label string, node, totes any) int64 {
		t.Helper()
		var id int64
		testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bins (bin_type_id, label, node_id, status, totes_on_cart)
			VALUES ($1,$2,$3,'available',$4) RETURNING id`, bt, label, node, totes).Scan(&id), "bin "+label)
		return id
	}
	levels := []store.MaintainLevel{
		{GroupNodeID: k1.ID, BinTypeID: tote, Want: 9}, {GroupNodeID: k1.ID, BinTypeID: kd, Want: 9},
		{GroupNodeID: k2.ID, BinTypeID: kd, Want: 1},
	}
	for _, l := range levels {
		testutil.MustNoErr(t, db.SetMaintainLevel(l), "level")
	}

	// Residents the count takes.
	bin(tote, "KCT-R24", s1[0].ID, 24)
	bin(tote, "KCT-RNIL", s1[1].ID, nil)
	bin(tote, "KCT-R26", s1[2].ID, 26)
	bin(kd, "KCT-RKD", s1[3].ID, nil)
	bin(kd, "KCT-R2KD", s2[0].ID, nil)
	// Standing in the kanban, and no resident at any height said: tops & bottoms.
	bin(tote, "KCT-RTB", s1[4].ID, 0)
	// Each thing the empty WHERE leaves out.
	exec(`UPDATE bins SET payload_code = 'PART-A' WHERE id = $1`, bin(kd, "KCT-LOADED", s1[5].ID, nil))
	locked := bin(kd, "KCT-LOCKED", s1[6].ID, nil)
	exec(`UPDATE bins SET locked = true, locked_by = 'test' WHERE id = $1`, locked)
	exec(`UPDATE bins SET status = 'staged' WHERE id = $1`, bin(kd, "KCT-STAGED", s1[7].ID, nil))
	exec(`UPDATE bins SET quality_hold = true WHERE id = $1`, bin(kd, "KCT-HELD", s1[8].ID, nil))
	bin(bare, "KCT-BARE-CART", s1[9].ID, nil)
	bin(other, "KCT-UNDECLARED", s1[10].ID, nil)
	claimed := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID, o.DeliveryNode, o.OrderType = "kct-claim", "ELSEWHERE", "move" })
	cb := bin(kd, "KCT-CLAIMED", s1[11].ID, nil)
	testdb.ReserveBin(t, db, claimed.ID, cb)
	// A disabled position, a cell's own position, a live loader's window, and a
	// lane's slot under the kanban (the walk's depth).
	off := &nodes.Node{Name: "KCT-K1-OFF", Enabled: false, ParentID: &k1.ID}
	testutil.MustNoErr(t, db.CreateNode(off), "disabled")
	bin(kd, "KCT-AT-OFF", off.ID, nil)
	cell := &nodes.Node{Name: "KCT-K1-CELL", Enabled: true, ParentID: &k1.ID}
	testutil.MustNoErr(t, db.CreateNode(cell), "cell")
	bin(kd, "KCT-AT-CELL", cell.ID, nil)
	testutil.MustNoErr(t, plantclaims.ReplaceProcess(db.DB, "KCT-PROC",
		[]plantclaims.StyleRow{{ProcessID: "KCT-PROC", StyleID: "S", ConfigGen: 1, IsActive: true}},
		[]plantclaims.ClaimRow{{ProcessID: "KCT-PROC", StyleID: "S", CoreNodeName: cell.Name, Role: protocol.ClaimRoleProduce,
			SwapMode: protocol.SwapMode("press_index"), PayloadCode: "PART-A"}}, 0), "claim of the cell")
	win := &nodes.Node{Name: "KCT-K1-WIN", Enabled: true, ParentID: &k1.ID}
	testutil.MustNoErr(t, db.CreateNode(win), "window")
	bin(kd, "KCT-AT-WIN", win.ID, nil)
	lid, err := db.CreateLoader(loaders.Loader{Name: "KCT-UNL", Role: loaders.RoleConsume,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator, OutboundDest: k1.Name})
	testutil.MustNoErr(t, err, "loader")
	testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: lid, PositionNodeID: win.ID}), "home")
	// A window of a pulled-directly stage 2 standing in the group is the group's.
	direct := &nodes.Node{Name: "KCT-K1-DIRECT", Enabled: true, ParentID: &k1.ID}
	testutil.MustNoErr(t, db.CreateNode(direct), "direct window")
	bin(kd, "KCT-AT-DIRECT", direct.ID, nil)
	did, err := db.CreateLoader(loaders.Loader{Name: "KCT-S2", Role: loaders.RoleConsume,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator})
	testutil.MustNoErr(t, err, "stage 2")
	exec(`UPDATE bin_loaders SET pulled_directly = true WHERE id = $1`, did)
	testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: did, PositionNodeID: direct.ID}), "stage 2 home")
	lane, err := db.AddLane(k1.ID, "KCT-K1-LANE")
	testutil.MustNoErr(t, err, "lane")
	depth := 1
	laneSlot := &nodes.Node{Name: "KCT-K1-LANE-1", Enabled: true, ParentID: &lane, Depth: &depth}
	testutil.MustNoErr(t, db.CreateNode(laneSlot), "lane slot")
	bin(kd, "KCT-IN-LANE", laneSlot.ID, nil)

	// Comings, every shape: a plain order's cart, a named one, one still naming the
	// group, a row that sets down elsewhere than the order's delivery, a named cart
	// that also has a row (counted once, by its row), tops & bottoms.
	order := func(uuid, delivery string, setup func(o *orders.Order)) *orders.Order {
		t.Helper()
		o := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID, o.DeliveryNode, o.OrderType = uuid, delivery, "move"
			if setup != nil {
				setup(o)
			}
		})
		if c := testdb.OnlyCart(t, o); c != nil {
			testdb.ReserveBin(t, db, o.ID, *c)
		}
		return o
	}
	c1 := bin(tote, "KCT-C1", nil, 40)
	order("kct-plain", s1[0].Name, func(o *orders.Order) { o.HoldFromBirth(c1) })
	c2 := bin(tote, "KCT-C2", nil, nil)
	order("kct-named", k1.Name, func(o *orders.Order) { o.NamedBinID = &c2 })
	c3 := bin(kd, "KCT-C3", nil, nil)
	order("kct-group", k1.Name, func(o *orders.Order) { o.HoldFromBirth(c3) })
	c4 := bin(kd, "KCT-C4", nil, nil)
	elsewhere := order("kct-junction", "ELSEWHERE", func(o *orders.Order) {
		o.StepsJSON = `[{"action":"pickup","node":"PICKUP"},{"action":"dropoff","node":"` + s1[1].Name + `"},{"action":"dropoff","node":"ELSEWHERE"}]`
	})
	testdb.SeedCartRow(t, db, elsewhere.ID, c4, 0, 1)
	c5 := bin(kd, "KCT-C5", nil, nil)
	order("kct-both", s2[1].Name, func(o *orders.Order) { o.NamedBinID = &c5; o.HoldFromBirth(c5) })
	c6 := bin(tote, "KCT-C6-TB", nil, 0)
	order("kct-tb", s1[0].Name, func(o *orders.Order) { o.HoldFromBirth(c6) })
	// Not coming: loaded, a terminal order, another type, the keeper's own asks.
	c7 := bin(kd, "KCT-C7", nil, nil)
	exec(`UPDATE bins SET payload_code = 'PART-A' WHERE id = $1`, c7)
	order("kct-loaded", s1[0].Name, func(o *orders.Order) { o.HoldFromBirth(c7) })
	c8 := bin(kd, "KCT-C8", nil, nil)
	done := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.DeliveryNode, o.OrderType, o.NamedBinID = "kct-done", s1[0].Name, "move", &c8
	})
	exec(`UPDATE orders SET status = $1 WHERE id = $2`, string(protocol.StatusConfirmed), done.ID)
	c9 := bin(other, "KCT-C9", nil, nil)
	order("kct-other", s1[0].Name, func(o *orders.Order) { o.HoldFromBirth(c9) })

	const mnt = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeee02"
	exec(`INSERT INTO demand_origins (origin_id, revision, episode_key, kind, opened_at) VALUES ($1, 1, $2, $3, NOW())`,
		mnt, protocol.MaintainEpisodeKey(k1.Name, "KCT-KD"), protocol.EpisodeKindMaintain)
	c10 := bin(kd, "KCT-C10", nil, nil)
	own := order("kct-own", s1[1].Name, func(o *orders.Order) { o.HoldFromBirth(c10); o.OriginID = mnt })
	// The episode's asks: a live root (own), a live leg that is no more demand and is
	// the longest parked, a parked root, and a terminal one.
	long := time.Now().UTC().Add(-3 * time.Hour)
	leg := order("kct-leg", s1[2].Name, func(o *orders.Order) { o.OriginID = mnt; o.ParentOrderID = &own.ID })
	parkedRoot := order("kct-parked", s1[2].Name, func(o *orders.Order) { o.OriginID = mnt })
	exec(`UPDATE orders SET queue_cause = 'leg-cause', queue_reason = 'the leg waits', created_at = $1 WHERE id = $2`, long, leg.ID)
	exec(`UPDATE orders SET queue_cause = 'root-cause', queue_reason = 'the root waits' WHERE id = $1`, parkedRoot.ID)
	gone := order("kct-gone", s1[2].Name, func(o *orders.Order) { o.OriginID = mnt })
	exec(`UPDATE orders SET status = $1, queue_cause = 'stale', created_at = $2 WHERE id = $3`,
		string(protocol.StatusCancelled), long.Add(-time.Hour), gone.ID)

	counts, err := db.ReadKanbanCounts(nil)
	testutil.MustNoErr(t, err, "the batched counts")
	scoped, err := db.ReadKanbanCounts([]int64{k2.ID})
	testutil.MustNoErr(t, err, "the batched counts of one kanban")
	open, err := db.ListOpenKeeperEpisodes()
	testutil.MustNoErr(t, err, "the episodes")
	if len(open) != 1 || open[0].OriginID != mnt {
		t.Fatalf("open episodes = %+v, want the one", open)
	}
	asked, err := db.CountLiveRootsByOrigin(mnt)
	testutil.MustNoErr(t, err, "the single asked count")
	if open[0].Asked != asked || asked != 2 {
		t.Errorf("asked = %d, single read %d; want the two live roots", open[0].Asked, asked)
	}
	if open[0].OldestCause != "leg-cause" || open[0].OldestReason != "the leg waits" || open[0].OldestAt == nil ||
		!open[0].OldestAt.Equal(long.Truncate(time.Microsecond)) {
		t.Errorf("oldest parked = %q %q %v, want the leg's, ahead of the terminal one", open[0].OldestCause, open[0].OldestReason, open[0].OldestAt)
	}

	binIDs := func(in []orders.InboundCarrier) []int64 {
		out := []int64{}
		for _, c := range in {
			out = append(out, c.BinID)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	for _, c := range []struct {
		g                *nodes.Node
		code             string
		bt               int64
		resident, coming int
	}{{k1, "KCT-TOTE", tote, 3, 2}, {k1, "KCT-KD", kd, 3, 2}, {k2, "KCT-KD", kd, 1, 1}} {
		lv := store.MaintainLevel{GroupNodeID: c.g.ID, BinTypeID: c.bt, BinTypeCode: c.code, Want: 9}
		single, err := db.CountLevel(c.g, lv, store.DemandOrigin{})
		testutil.MustNoErr(t, err, "the single read")
		got := counts.Level(lv, 0)
		if got.Resident != single.Resident || got.Coming != single.Coming ||
			!reflect.DeepEqual(binIDs(got.Inbound), binIDs(single.Inbound)) {
			t.Errorf("%s/%s batched %+v, single %+v", c.g.Name, c.code, got, single)
		}
		// The count is the fixture's, so a population lost from both is caught too.
		if got.Resident != c.resident || got.Coming != c.coming {
			t.Errorf("%s/%s resident %d coming %d; want %d and %d", c.g.Name, c.code, got.Resident, got.Coming, c.resident, c.coming)
		}
		byTotes, err := db.CountEmptyBinsOfTypeInGroupByTotes(c.code, c.g.ID)
		testutil.MustNoErr(t, err, "the single census")
		wants := []store.MaintainLayerWant{}
		totes := 8
		for layers := 0; layers <= 5; layers++ {
			wants = append(wants, store.MaintainLayerWant{BinTypeID: c.bt, Layers: layers, Want: 1, TotesPerLayer: &totes})
		}
		tl, _ := counts.TypeLine(lv, 0, wants)
		for _, h := range tl.Heights {
			want := byTotes[h.Layers*totes]
			for _, in := range single.Inbound {
				if in.TotesOnCart != nil && *in.TotesOnCart == h.Layers*totes {
					want++
				}
			}
			if h.Have != want {
				t.Errorf("%s/%s at %d layers: have %d, single reads %d", c.g.Name, c.code, h.Layers, h.Have, want)
			}
		}
	}
	lvK2 := store.MaintainLevel{GroupNodeID: k2.ID, BinTypeID: kd, BinTypeCode: "KCT-KD", Want: 1}
	if got := scoped.Level(lvK2, 0); got.Resident != 1 || got.Coming != 1 {
		t.Errorf("scoped to K2: %+v, want its one and one", got)
	}
	lvK1 := store.MaintainLevel{GroupNodeID: k1.ID, BinTypeID: kd, BinTypeCode: "KCT-KD", Want: 1}
	if got := scoped.Level(lvK1, 0); got.Resident != 0 || got.Coming != 0 {
		t.Errorf("a read scoped to K2 answered for K1: %+v", got)
	}
}

// TestEffectiveTypesOfChildren_AgreesWithTheSingleRead: one statement answers what
// GetEffectiveBinTypes answers per child, in each of the modes: all, specific with
// and without rows, inheriting its own rows, the group's, the ancestor's, nothing.
func TestEffectiveTypesOfChildren_AgreesWithTheSingleRead(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	var a, b, c int64
	for code, id := range map[string]*int64{"ETC-A": &a, "ETC-B": &b, "ETC-C": &c} {
		testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code) VALUES ($1) RETURNING id`, code).Scan(id), "type")
	}
	root := testdb.MarkedGroup(t, db, "ETC-ROOT")
	testutil.MustNoErr(t, db.SetNodeBinTypes(*root, []int64{c}), "the ancestor's types")
	inner := &nodes.Node{Name: "ETC-INNER", Enabled: true, ParentID: root, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(inner), "inner")
	bare := testdb.MarkedGroup(t, db, "ETC-BARE")
	child := func(parent *int64, name, mode string, types ...int64) *nodes.Node {
		t.Helper()
		n := &nodes.Node{Name: name, Enabled: true, ParentID: parent}
		testutil.MustNoErr(t, db.CreateNode(n), "child")
		if mode != "" {
			testutil.MustNoErr(t, db.SetNodeProperty(n.ID, nodes.PropBinTypeMode, mode), "mode")
		}
		if len(types) > 0 {
			testutil.MustNoErr(t, db.SetNodeBinTypes(n.ID, types), "types")
		}
		return n
	}
	kids := []*nodes.Node{
		child(&inner.ID, "ETC-ALL", "all", a),
		child(&inner.ID, "ETC-SPECIFIC", "specific", a, b),
		child(&inner.ID, "ETC-SPECIFIC-NONE", "specific"),
		child(&inner.ID, "ETC-INHERIT-OWN", "inherit", b),
		child(&inner.ID, "ETC-INHERIT-UP", ""),
		child(&inner.ID, "ETC-DEFAULT-OWN", "", a),
	}
	got, err := bins.EffectiveTypesOfChildren(db.DB, inner.ID)
	testutil.MustNoErr(t, err, "the batched read")
	code := func(in []*bins.BinType) []string {
		out := []string{}
		for _, bt := range in {
			out = append(out, bt.Code)
		}
		return out
	}
	want := map[string][]string{"ETC-ALL": {}, "ETC-SPECIFIC": {"ETC-A", "ETC-B"}, "ETC-SPECIFIC-NONE": {},
		"ETC-INHERIT-OWN": {"ETC-B"}, "ETC-INHERIT-UP": {"ETC-C"}, "ETC-DEFAULT-OWN": {"ETC-A"}}
	for _, k := range kids {
		single, err := db.GetEffectiveBinTypes(k.ID)
		testutil.MustNoErr(t, err, "the single read")
		if !reflect.DeepEqual(code(got[k.ID]), code(single)) || !reflect.DeepEqual(code(single), want[k.Name]) {
			t.Errorf("%s: batched %v, single %v, want %v", k.Name, code(got[k.ID]), code(single), want[k.Name])
		}
	}
	// A group with no rows up its chain: nothing declared, every child unrestricted.
	free := child(bare, "ETC-FREE", "")
	none, err := bins.EffectiveTypesOfChildren(db.DB, *bare)
	testutil.MustNoErr(t, err, "the batched read, nothing declared")
	if len(none[free.ID]) != 0 {
		t.Errorf("an undeclared chain answered %v", code(none[free.ID]))
	}
}

// KanbanAskReads answers the slot chooser's per-child questions from one read, and
// each answer is the single read's: bins, live orders, a stranger's row. An ask noted
// moves the next answer on with no statement.
func TestKanbanAskReads_AnswerAsTheSingleReads(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	g := testdb.MarkedGroup(t, db, "KAR")
	var kids []*nodes.Node
	for i := 0; i < 4; i++ {
		n := &nodes.Node{Name: "KAR-S" + string(rune('A'+i)), Enabled: true, ParentID: g}
		testutil.MustNoErr(t, db.CreateNode(n), "child")
		kids = append(kids, n)
	}
	var bt int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code) VALUES ('KAR-T') RETURNING id`).Scan(&bt), "type")
	var bin int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bins (bin_type_id, label, node_id, status) VALUES ($1,'KAR-B',$2,'available') RETURNING id`,
		bt, kids[0].ID).Scan(&bin), "bin")
	// Pending and live, at the child: a pre-dispatch order names it.
	pending := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID, o.DeliveryNode, o.OrderType = "kar-pend", kids[1].Name, "move" })
	holder := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID, o.DeliveryNode, o.OrderType = "kar-hold", "ELSEWHERE", "move" })
	testutil.MustNoErr(t, db.ReserveGroupRoom(kids[2].ID, holder.ID), "a stranger's slot row")

	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "counting db")
	t.Cleanup(func() { cdb.Close() })
	reads := cdb.KanbanAskReads(*g, nil, map[string]int{"KAR-T": 7})
	check := func() {
		t.Helper()
		for _, k := range kids {
			nb, _ := reads.CountBinsByNode(k.ID)
			sb, err := db.CountBinsByNode(k.ID)
			testutil.MustNoErr(t, err, "single bins")
			nl, _ := reads.CountActiveOrdersByDeliveryNodeExcluding(k.Name, pending.ID)
			sl, err := db.CountActiveOrdersByDeliveryNodeExcluding(k.Name, pending.ID)
			testutil.MustNoErr(t, err, "single live")
			nl2, _ := reads.CountActiveOrdersByDeliveryNodeExcluding(k.Name, 0)
			sl2, err := db.CountActiveOrdersByDeliveryNodeExcluding(k.Name, 0)
			testutil.MustNoErr(t, err, "single live, excluding nobody")
			ns, _ := reads.SlotSpokenForByStranger(k.ID, pending.ID)
			ss, err := db.SlotSpokenForByStranger(k.ID, pending.ID)
			testutil.MustNoErr(t, err, "single spoken")
			if nb != sb || nl != sl || nl2 != sl2 || ns != ss {
				t.Errorf("%s: read says bins %d, live %d/%d, spoken %v; single says %d, %d/%d, %v", k.Name, nb, nl, nl2, ns, sb, sl, sl2, ss)
			}
		}
		if n, _ := reads.CountEmptyBinsOfTypeInGroup("KAR-T", *g, domain.Height{}); n != 7 {
			t.Errorf("the cap's count = %d, want the tick's 7", n)
		}
	}
	counter.Reset()
	check()
	if n := countOf(counter.Statements(), "group-slot-state"); n != 1 {
		t.Errorf("the slot state read %d times, want once", n)
	}
	reads.NoteAsked(kids[3].ID, 4242)
	if n, _ := reads.CountActiveOrdersByDeliveryNodeExcluding(kids[3].Name, 0); n != 1 {
		t.Errorf("after an ask is noted the child reads %d live, want 1", n)
	}
	if n, _ := reads.CountActiveOrdersByDeliveryNodeExcluding(kids[3].Name, 4242); n != 0 {
		t.Errorf("the asker's own ask counted against it: %d", n)
	}
}

func countOf(stmts []string, sub string) int {
	n := 0
	for _, s := range stmts {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}
