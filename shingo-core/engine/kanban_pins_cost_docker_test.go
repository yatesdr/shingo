//go:build docker

package engine

import (
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
// CHARACTERIZATION PINS. Each today* const is the number measured at this tree,
// asserted exactly so a read added or removed is a decision. The comment on each
// names the settled target; the lane that lands the target changes the const.
const (
	// todayKeeperTickFixed + S is one steady tick, nothing short: the node list,
	// the open episodes, one maintain_enabled read per enabled synthetic node (S),
	// and per kept group (K, K2) its station, its levels and two counts per level.
	// Settled: 4 whatever S is.
	todayKeeperTickFixed = 16
	// todaySnapshotBuild is one bins feed build for the Edge watching the
	// windows: the read snapshot (5), K's levels, then UnloaderGroupLines (open
	// episodes, types in play, two counts per level). Settled: about 9.
	todaySnapshotBuild = 16
	// todayIntakeDoorShort is the intake door up to the order's birth, for an
	// empty pushed at K at its level, into Z45 short of that type (three of its
	// eight positions free). Settled: about 9-11.
	todayIntakeDoorShort = 45
	// todayIntakeDoorAllFree is the same door with all eight of Z45's positions
	// free. Settled: about 9-11, the same number as the short case.
	todayIntakeDoorAllFree = 60
	// todayParkedArmShort is a scanner pass with one push parked at K's level that
	// Z45 takes (three of its eight positions free), from the pass's start to the
	// re-aim: the order list and read, K's gate, the overflow's gate, the reserve's
	// resolve. Settled: about 8.
	todayParkedArmShort = 84
	// todayParkedArmAllFree is the same arm with all eight of Z45's positions free.
	// Settled: about 8, the same number as the short case.
	todayParkedArmAllFree = 104
	// todayParkedPassShort is that whole pass: the arm, then the acquire's lane
	// admission and the hand to the fleet. The brief's settled number covers the
	// arm alone; this tail (62) has none yet.
	todayParkedPassShort = 146
	// todayParkedPassAllFree is that whole pass with all eight positions free.
	todayParkedPassAllFree = 166
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

// kanbanCostFlat is a flat group of n positions, its carrier types open as a
// level save leaves them (service.ensureExplicitBinTypeMode).
func kanbanCostFlat(t *testing.T, db *store.DB, name string, n int) (*nodes.Node, []*nodes.Node) {
	t.Helper()
	id, err := nodes.CreateGroup(db.DB, name)
	testutil.MustNoErr(t, err, "group "+name)
	testdb.MarkGroup(t, db, id)
	testutil.MustNoErr(t, db.SetNodeProperty(id, nodes.PropBinTypeMode, nodes.BinTypeModeAll), "bin type mode")
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
		testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{GroupNodeID: g.ID, BinTypeID: bt, Want: want}), "level")
	}
	stand := func(bt int64, at []*nodes.Node, label string) []*nodes.Node {
		for i, n := range at {
			landAt(t, db, bt, n.ID, fmt.Sprintf("%s-%d", label, i+1))
		}
		return at
	}
	keeper := func(g *nodes.Node, supports ...string) {
		testutil.MustNoErr(t, db.SetNodeProperty(g.ID, nodes.PropMaintainEnabled, "on"), "enable")
		testutil.MustNoErr(t, db.SetNodeProperty(g.ID, nodes.PropMaintenanceStation, "kc-edge"), "station")
		var ids []int64
		for _, name := range supports {
			n := &nodes.Node{Name: name, Enabled: true}
			testutil.MustNoErr(t, db.CreateNode(n), "supported position")
			ids = append(ids, n.ID)
		}
		testutil.MustNoErr(t, nodes.SetMaintainSupports(db.DB, g.ID, ids), "supports")
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

	// Z45 and Z32 declare levels and run no keeper: the flag is K's and K2's.
	r.z45, kids = kanbanCostFlat(t, db, "KC-Z45", 8)
	level(r.z45, r.small, 2)
	level(r.z45, r.big, 2)
	level(r.z45, r.totes, 2)
	stand(r.small, kids[:z45Small], "KC-Z45-S")
	stand(r.big, kids[z45Small:z45Small+z45Big], "KC-Z45-B")
	stand(r.totes, kids[z45Small+z45Big:z45Small+z45Big+z45Totes], "KC-Z45-T")
	testutil.MustNoErr(t, db.SetNodeProperty(r.k.ID, nodes.PropOverflowDestination, r.z45.Name), "overflow")

	r.z32, kids = kanbanCostFlat(t, db, "KC-Z32", 8)
	level(r.z32, r.t32, 6)
	stand(r.t32, kids[:6], "KC-Z32")

	// Plain markets: lanes, no level, each lane one more synthetic node for the
	// tick's per-node read.
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

// syntheticCount is S: the enabled synthetic nodes the tick asks one question of.
func (r *kanbanCostRig) syntheticCount(t *testing.T) int64 {
	t.Helper()
	var s int64
	testutil.MustNoErr(t, r.db.QueryRow(`SELECT count(*) FROM nodes WHERE is_synthetic AND enabled`).Scan(&s), "S")
	return s
}

// inGroup reports whether a node name is a position of group g.
func (r *kanbanCostRig) inGroup(t *testing.T, name string, g *nodes.Node) bool {
	t.Helper()
	n, err := r.db.GetNodeByDotName(name)
	testutil.MustNoErr(t, err, "read "+name)
	return n != nil && n.ParentID != nil && *n.ParentID == g.ID
}

// firstBegin is the index of the first transaction's BEGIN in stmts, len(stmts)
// with none.
func firstBegin(stmts []string) int {
	for i, s := range stmts {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "begin") {
			return i
		}
	}
	return len(stmts)
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

// TestKanbanCost_KeeperTick: one steady tick. Asserted with Z45 and Z32 running
// no keeper; logged with them flagged on as well, the reading the round-4 census
// assumed for "≈22+S".
func TestKanbanCost_KeeperTick(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 2, 2, 2)
	s := r.syntheticCount(t)
	m := r.eng.Maintainer()

	r.counter.Reset()
	m.Tick()
	n := r.counter.Count()
	t.Logf("STATEMENTS: keeper tick, steady, S=%d: %d (fixed %d + S)\n%s", s, n, n-s,
		testutil.StatementBreakdown(r.counter.Statements()))
	for _, st := range m.Snapshot() {
		if st.Gap > 0 || st.Created != 0 || st.OriginID != "" {
			t.Fatalf("fixture drift: the tick is not steady: %+v", st)
		}
	}
	if len(m.Snapshot()) != 5 {
		t.Fatalf("fixture drift: %d lines kept, want K's 4 and K2's 1: %+v", len(m.Snapshot()), m.Snapshot())
	}
	if n != todayKeeperTickFixed+s {
		t.Errorf("keeper tick = %d statements, want %d + S (%d)", n, todayKeeperTickFixed, s)
	}

	for _, g := range []*nodes.Node{r.z45, r.z32} {
		testutil.MustNoErr(t, r.db.SetNodeProperty(g.ID, nodes.PropMaintainEnabled, "on"), "enable")
		testutil.MustNoErr(t, r.db.SetNodeProperty(g.ID, nodes.PropMaintenanceStation, "kc-edge"), "station")
	}
	r.counter.Reset()
	m.Tick()
	t.Logf("STATEMENTS: the same tick with Z45 and Z32 flagged on: %d (fixed %d + S)", r.counter.Count(), r.counter.Count()-s)
}

// TestKanbanCost_SnapshotBuild: one bins feed build for the Edge watching the
// unloader's windows, cut out of the watch list's handling: the build runs from
// its read snapshot's BEGIN to the outbox write that sends what it built.
func TestKanbanCost_SnapshotBuild(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 2, 2, 2)
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
	from := firstBegin(stmts)
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
	if len(build) != todaySnapshotBuild {
		t.Errorf("snapshot build = %d statements, want %d", len(build), todaySnapshotBuild)
	}
}

// TestKanbanCost_IntakeDoor: an empty off a window, pushed at K at its level,
// up to its birth in Z45: the reference to the node and the bin, K's refusal, the
// overflow's name, and Z45's resolve. Z45 short of the type (its residents stand
// on five positions), and all free.
func TestKanbanCost_IntakeDoor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		small, big, totes int
		want              int
	}{
		{"Z45 short of the type", 1, 2, 2, todayIntakeDoorShort},
		{"Z45 all free", 0, 0, 0, todayIntakeDoorAllFree},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newKanbanCostRig(t, tc.small, tc.big, tc.totes)
			r.counter.Reset()
			o, _, lerr := r.eng.dispatcher.Lifecycle().CreateInboundOrder("kc-edge", &protocol.OrderRequest{
				OrderUUID: "kc-push", OrderType: dispatch.OrderTypeMove, Quantity: 1,
				SourceNode: r.windows[1].Name, DeliveryNode: r.k.Name, BinID: r.push}, nil)
			if lerr != nil {
				t.Fatalf("intake refused the push: %s", lerr.Detail)
			}
			if !r.inGroup(t, o.DeliveryNode, r.z45) {
				t.Fatalf("fixture drift: the push went to %s, want a position of %s", o.DeliveryNode, r.z45.Name)
			}
			stmts := r.counter.Statements()
			door := stmts[:firstBegin(stmts)]
			t.Logf("STATEMENTS: %s: the door %d (with the birth %d)\n%s", tc.name, len(door), len(stmts),
				testutil.StatementBreakdown(door))
			if len(door) != tc.want {
				t.Errorf("intake door = %d statements, want %d", len(door), tc.want)
			}
		})
	}
}

// TestKanbanCost_ScannerParkedPass: a push parked at K's level, on the pass Z45
// takes it. K names no overflow on the pass that parks it, so the measured pass
// is the first to ask. Z45 short of the type, and all free.
func TestKanbanCost_ScannerParkedPass(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		small, big, totes int
		arm, pass         int
	}{
		{"Z45 short of the type", 1, 2, 2, todayParkedArmShort, todayParkedPassShort},
		{"Z45 all free", 0, 0, 0, todayParkedArmAllFree, todayParkedPassAllFree},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newKanbanCostRig(t, tc.small, tc.big, tc.totes)
			testutil.MustNoErr(t, r.db.SetNodeProperty(r.k.ID, nodes.PropOverflowDestination, ""), "no overflow yet")
			push := testdb.CreateOrder(t, r.db, func(o *orders.Order) {
				o.EdgeUUID = "kc-parked"
				o.StationID = "kc-edge"
				o.OrderType = dispatch.OrderTypeMove
				o.SourceNode = r.windows[1].Name
				o.DeliveryNode = r.k.Name
			})
			testutil.MustNoErr(t, reservations.Acquire(r.db.DB, push.ID, push.ID, r.push, "test"), "the push holds its cart")
			testdb.HoldCart(t, r.db, push.ID, r.push)

			r.eng.fulfillment.RunOnce()
			parked, err := r.db.GetOrder(push.ID)
			testutil.MustNoErr(t, err, "reload the push")
			if parked.Status != protocol.StatusQueued || parked.QueueCause != string(dispatch.CauseNGRPAtLevel) {
				t.Fatalf("fixture drift: the push is %s (%s), want parked at K's level", parked.Status, parked.QueueCause)
			}

			testutil.MustNoErr(t, r.db.SetNodeProperty(r.k.ID, nodes.PropOverflowDestination, r.z45.Name), "overflow")
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
