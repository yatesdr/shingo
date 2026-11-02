//go:build docker

package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/dispatch/binresolver"
	"shingocore/domain"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/kanban"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/plantclaims"
)

// kanban_effective_want_docker_test.go — the keeper, the cap and the gate hold a
// kanban to want = base + cover x running, and a kanban with no supports runs no
// keeper.

// coverPart makes a part riding one carrier type.
func coverPart(t *testing.T, db *store.DB, code string, carrier int64) {
	t.Helper()
	var id int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO payloads (code) VALUES ($1) RETURNING id`, code).Scan(&id), "part "+code)
	_, err := db.DB.Exec(`INSERT INTO payload_bin_types (payload_id, bin_type_id) VALUES ($1,$2)`, id, carrier)
	testutil.MustNoErr(t, err, "the part's carrier")
}

// coverStyle is one style of a process: its produce claims as (node, part).
type coverStyle struct {
	id     string
	active bool
	claims [][2]string
}

// coverProcess mirrors a process as the plant.claims feed does.
func coverProcess(t *testing.T, db *store.DB, process string, styles ...coverStyle) {
	t.Helper()
	var ss []plantclaims.StyleRow
	var cs []plantclaims.ClaimRow
	for _, st := range styles {
		ss = append(ss, plantclaims.StyleRow{ProcessID: process, StyleID: st.id, ConfigGen: 1, IsActive: st.active})
		for i, c := range st.claims {
			cs = append(cs, plantclaims.ClaimRow{ProcessID: process, StyleID: st.id, CoreNodeName: c[0],
				Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapMode("press_index"), PayloadCode: c[1], Seq: i})
		}
	}
	testutil.MustNoErr(t, plantclaims.ReplaceProcess(db.DB, process, ss, cs, 0), "mirror "+process)
}

// TestKanbanPin_TheKeeperFollowsTheEffectiveWant: base 1 and cover 2 with one process
// running the type ask 3, and 3 is what the episode stores as its threshold. A
// process on several positions with two parts on the type counts once, a type
// nothing runs stays at its base, and a second process adds another cover.
func TestKanbanPin_TheKeeperFollowsTheEffectiveWant(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	grpID, small := mntFixture(t, db, "KEW", 8, "KEW-SMALL", 1)
	var big int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code) VALUES ('KEW-BIG') RETURNING id`).Scan(&big), "big")
	testutil.MustNoErr(t, db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: grpID, BinTypeID: big, Want: 0}), "big's level")
	testdb.KanbanCover(t, db, grpID, 2)
	for _, n := range []string{"KEW-OUT1", "KEW-OUT2", "KEW-OUT3", "KEW-OUT4", "KEW-OUT5"} {
		mntOutsideEmpty(t, db, small, n)
	}
	coverPart(t, db, "KEW-L", small)
	coverPart(t, db, "KEW-R", small)

	second := &nodes.Node{Name: "KEW-SECOND", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(second), "a second supported position")
	weld := &nodes.Node{Name: "KEW-WELD", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(weld), "another process's position")
	support, err := db.GetNodeByDotName("KEW-SUPPORT")
	testutil.MustNoErr(t, err, "the support")
	testutil.MustNoErr(t, nodes.SetKanbanSupports(db.DB, grpID, []int64{support.ID, second.ID, weld.ID}), "supports")
	coverProcess(t, db, "KEW-PRESS", coverStyle{"S", true, [][2]string{{"KEW-SUPPORT", "KEW-L"}, {"KEW-SECOND", "KEW-R"}}})

	m := eng.KanbanKeeper()
	m.Tick()
	st := mntState(t, m, "KEW", "KEW-SMALL")
	if st.Want != 3 || st.Base != 1 || st.Created != 3 {
		t.Fatalf("base 1, cover 2, one process on two positions with two parts: %+v; want 3 of base 1, three asks", st)
	}
	if len(st.RunningProcesses) != 1 || st.RunningProcesses[0] != "KEW-PRESS" {
		t.Errorf("running processes = %v, want the press", st.RunningProcesses)
	}
	opened, err := db.GetDemandOrigin(st.OriginID)
	testutil.MustNoErr(t, err, "the episode")
	if opened.Threshold != 3 {
		t.Errorf("the episode's threshold = %d, want the effective want it opened over, 3", opened.Threshold)
	}
	if bigSt := mntState(t, m, "KEW", "KEW-BIG"); bigSt.Want != 0 || bigSt.Gap != 0 {
		t.Errorf("a type nothing runs: %+v, want its base", bigSt)
	}

	coverProcess(t, db, "KEW-WELD-P", coverStyle{"W", true, [][2]string{{"KEW-WELD", "KEW-L"}}})
	m.Tick()
	if st = mntState(t, m, "KEW", "KEW-SMALL"); st.Want != 5 || st.Created != 2 {
		t.Errorf("a second process on the type: %+v; want 1 + 2x2 = 5, two more asks", st)
	}
}

// TestKanbanPin_CoverFollowsTheTargetAndRevertsWithinATick: a changeover episode
// opened for the process moves the cover to its target's type on the next tick, and
// its close moves it back on the tick after, with no save and no cross-tick state.
func TestKanbanPin_CoverFollowsTheTargetAndRevertsWithinATick(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	grpID, small := mntFixture(t, db, "KET", 8, "KET-SMALL", 1)
	var totes int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code) VALUES ('KET-TOTES') RETURNING id`).Scan(&totes), "totes")
	testutil.MustNoErr(t, db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: grpID, BinTypeID: totes, Want: 1}), "totes' level")
	testdb.KanbanCover(t, db, grpID, 4)
	coverPart(t, db, "KET-SP", small)
	coverPart(t, db, "KET-TP", totes)
	coverProcess(t, db, "KET-PRESS", coverStyle{"SMALL", true, [][2]string{{"KET-SUPPORT", "KET-SP"}}},
		coverStyle{"TOTES", false, [][2]string{{"KET-SUPPORT", "KET-TP"}}})
	mntOutsideEmpty(t, db, totes, "KET-OUT")

	wants := func(what string, wantSmall, wantTotes int) {
		t.Helper()
		m := eng.KanbanKeeper()
		m.Tick()
		s, tt := mntState(t, m, "KET", "KET-SMALL"), mntState(t, m, "KET", "KET-TOTES")
		if s.Want != wantSmall || tt.Want != wantTotes {
			t.Errorf("%s: small wants %d, totes %d; want %d and %d", what, s.Want, tt.Want, wantSmall, wantTotes)
		}
	}
	wants("the active style", 5, 1)

	const co = "c0c0c0c0-0000-4000-8000-000000000001"
	testutil.MustNoErr(t, db.UpsertDemandOrigin(store.DemandOrigin{OriginID: co, Revision: 1, EpisodeKey: "co|" + co,
		Kind: protocol.EpisodeKindChangeover, StationID: "edge.1", ProcessID: "KET-PRESS", CoreNodeName: "KET-SUPPORT",
		PayloadCode: "KET-TP", OpenedAt: time.Now().UTC()}), "open the changeover")
	wants("a changeover opened for totes", 1, 5)

	closed, err := db.CloseDemandOriginByID(co, "cancelled", protocol.ClosedByNotification, time.Now().UTC())
	testutil.MustNoErr(t, err, "close")
	if !closed {
		t.Fatal("the changeover did not close")
	}
	wants("the changeover cancelled", 5, 1)
}

// TestKanbanPin_ALevelZeroRowLiftedByCover: a row at 0 refuses every store while
// nothing runs its type (TestKanbanPin_ALevelZeroRowRefusesEveryStore); with a process
// running it the want is the cover, and the resolver and the gate both admit, up to
// the want.
func TestKanbanPin_ALevelZeroRowLiftedByCover(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	grpID, t32 := mntFixture(t, db, "KEZ", 4, "KEZ-32", 0)
	testdb.KanbanCover(t, db, grpID, 1)
	grp, err := db.GetNode(grpID)
	testutil.MustNoErr(t, err, "the group")
	d := eng.Dispatcher()
	store0 := func() error {
		_, err := d.PlanGroupStoreSlot(grp, "", binresolver.KnownBinType(t32))
		return err
	}
	if err := store0(); !errors.Is(err, kanban.ErrAtLevel) {
		t.Fatalf("nothing running the type: %v, want at its declared level", err)
	}

	coverPart(t, db, "KEZ-P", t32)
	coverProcess(t, db, "KEZ-PRESS", coverStyle{"S", true, [][2]string{{"KEZ-SUPPORT", "KEZ-P"}}})
	if err := store0(); err != nil {
		t.Errorf("a process running the type, cover 1: %v, want room", err)
	}

	dock := &nodes.Node{Name: "KEZ-DOCK", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dock), "the pusher's node")
	cart := landAt(t, db, t32, dock.ID, "KEZ-PUSHED")
	push := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.OrderType, o.SourceNode, o.DeliveryNode = "kez-push", dispatch.OrderTypeMove, dock.Name, grp.Name
		o.HoldFromBirth(cart)
	})
	testdb.ReserveBin(t, db, push.ID, cart)
	if blocked, v := dispatch.CheckDropoffCapacity(db, grp.Name, push.ID); blocked {
		t.Errorf("the gate refused a cart the cover wants: %+v", v)
	}

	// At the want the cap binds again.
	landCarrier(t, db, t32, freePosition(t, db, grpID), "KEZ-LANDED")
	if err := store0(); !errors.Is(err, kanban.ErrAtLevel) {
		t.Errorf("at the effective want: %v, want at its declared level", err)
	}
}

// TestKanban_ANoSupportsKanbanRunsNoKeeper: a kanban with no supports counts and
// shows its shortfall, opens no episode, asks nothing, a station or none. One that
// loses its supports has its open episode withdrawn.
func TestKanban_ANoSupportsKanbanRunsNoKeeper(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	grpID, btID := mntFixture(t, db, "KNS", 3, "KNS-T", 2)
	mntOutsideEmpty(t, db, btID, "KNS-OUT1")
	mntOutsideEmpty(t, db, btID, "KNS-OUT2")
	testutil.MustNoErr(t, nodes.SetKanbanSupports(db.DB, grpID, nil), "no supports")
	landAt(t, db, btID, firstChild(t, db, grpID).ID, "KNS-STANDING")

	noStation := testdb.MarkedGroup(t, db, "KNS-NOSTATION")
	testutil.MustNoErr(t, db.CreateNode(&nodes.Node{Name: "KNS-NOSTATION-P", Enabled: true, ParentID: noStation}), "position")
	testutil.MustNoErr(t, db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: *noStation, BinTypeID: btID, Want: 1}), "level")

	m := eng.KanbanKeeper()
	m.Tick()
	st := mntState(t, m, "KNS", "KNS-T")
	if st.Want != 2 || st.Resident != 1 || st.Gap != 1 || st.Created != 0 || st.OriginID != "" || st.HeldBy == "" {
		t.Errorf("a station and no supports: %+v; want the shortfall of 1 shown, nothing asked or open, and why", st)
	}
	if st = mntState(t, m, "KNS-NOSTATION", "KNS-T"); st.Gap != 1 || st.Created != 0 {
		t.Errorf("no station and no supports: %+v; want the shortfall of 1 shown and nothing asked", st)
	}
	open, err := db.ListOpenEpisodesOfKind(protocol.EpisodeKindKanban)
	testutil.MustNoErr(t, err, "open")
	if len(open) != 0 {
		t.Errorf("a kanban with no supports opened %d episodes", len(open))
	}
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM orders`).Scan(&n), "orders")
	if n != 0 {
		t.Errorf("%d orders asked", n)
	}

	// Supports back: it asks. Taken away with the ask live: the episode withdraws.
	pos := &nodes.Node{Name: "KNS-PRESS", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(pos), "a position")
	testutil.MustNoErr(t, nodes.SetKanbanSupports(db.DB, grpID, []int64{pos.ID}), "supports")
	m.Tick()
	st = mntState(t, m, "KNS", "KNS-T")
	if st.Created != 1 || st.OriginID == "" {
		t.Fatalf("with a support: %+v, want the ask and the episode", st)
	}
	origin := st.OriginID
	testutil.MustNoErr(t, nodes.SetKanbanSupports(db.DB, grpID, nil), "supports gone")
	m.Tick()
	closed, err := db.GetDemandOrigin(origin)
	testutil.MustNoErr(t, err, "the episode")
	if closed.CloseReason != protocol.CloseReasonThresholdRemoved {
		t.Errorf("supports gone: the episode's close is %q, want %q", closed.CloseReason, protocol.CloseReasonThresholdRemoved)
	}
}

// sourcesRig is a keeper short of one type with its source groups around it.
type sourcesRig struct {
	db   *store.DB
	eng  *Engine
	k    int64
	bt   int64
	m    *KanbanKeeper
	code string
}

func newSourcesRig(t *testing.T, name string, want int) *sourcesRig {
	t.Helper()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	k, bt := mntFixture(t, db, name, 4, name+"-T", want)
	return &sourcesRig{db: db, eng: eng, k: k, bt: bt, m: eng.KanbanKeeper(), code: name + "-T"}
}

// zone makes a flat group of positions.
func (r *sourcesRig) zone(t *testing.T, name string, n int) *nodes.Node {
	t.Helper()
	id := testdb.MarkedGroup(t, r.db, name)
	for i := 0; i < n; i++ {
		testutil.MustNoErr(t, r.db.CreateNode(&nodes.Node{Name: name + "-P" + string(rune('A'+i)), Enabled: true, ParentID: id}), "position")
	}
	g, err := r.db.GetNode(*id)
	testutil.MustNoErr(t, err, "zone")
	return g
}

// stand stands an empty of the keeper's type on a free position of a group.
func (r *sourcesRig) stand(t *testing.T, g *nodes.Node, label string) int64 {
	t.Helper()
	return landAt(t, r.db, r.bt, freePosition(t, r.db, g.ID), label)
}

// inGroup reports whether a bin stands on a position of g.
func (r *sourcesRig) inGroup(t *testing.T, b *bins.Bin, g *nodes.Node) bool {
	t.Helper()
	if b.NodeID == nil {
		return false
	}
	n, err := r.db.GetNode(*b.NodeID)
	testutil.MustNoErr(t, err, "the bin's node")
	return n.ParentID != nil && *n.ParentID == g.ID
}

func (r *sourcesRig) sources(t *testing.T, ids ...int64) {
	t.Helper()
	testutil.MustNoErr(t, r.db.SaveKanban(store.KanbanChange{NodeID: r.k, EnsureRow: true, Sources: &ids}, "test"), "sources")
}

// asksFrom ticks and says which bins the tick's asks took, in order of asking.
func (r *sourcesRig) asksFrom(t *testing.T, name string) (map[int64]bool, KanbanLineState) {
	t.Helper()
	r.m.Tick()
	st := mntState(t, r.m, name, r.code)
	out := map[int64]bool{}
	if st.OriginID == "" {
		return out, st
	}
	for _, o := range kanbanAsksOf(t, r.db, st.OriginID) {
		if c := testdb.OnlyCart(t, o); c != nil {
			out[*c] = true
		}
	}
	return out, st
}

// TestKanbanSources_TheKeepersFind: the keeper's ask names no source and its find
// reads the group's sources: rank order ahead of the find's own, exclusive when the
// list is not empty, the plant when it is, and the fence still applied.
func TestKanbanSources_TheKeepersFind(t *testing.T) {
	t.Parallel()

	t.Run("rank order, the second source only when the first is out", func(t *testing.T) {
		t.Parallel()
		r := newSourcesRig(t, "KSR", 3)
		// The second-ranked source has the lower ids and a lane-mouth tie, so
		// only the rank can prefer the first.
		second, first := r.zone(t, "KSR-SECOND", 2), r.zone(t, "KSR-FIRST", 2)
		s1 := r.stand(t, second, "KSR-S1")
		f1 := r.stand(t, first, "KSR-F1")
		f2 := r.stand(t, first, "KSR-F2")
		r.sources(t, first.ID, second.ID)
		got, st := r.asksFrom(t, "KSR")
		if st.Created != 3 || !got[f1] || !got[f2] || !got[s1] {
			t.Fatalf("created %d, bins %v; want all three", st.Created, got)
		}
		asks := kanbanAsksOf(t, r.db, st.OriginID)
		last := asks[len(asks)-1]
		if c := testdb.OnlyCart(t, last); c == nil || *c != s1 {
			t.Errorf("the last ask took %v, want the second source's %d: the first source goes first", c, s1)
		}
	})

	t.Run("exclusive when the list is not empty", func(t *testing.T) {
		t.Parallel()
		r := newSourcesRig(t, "KSX", 1)
		src := r.zone(t, "KSX-SRC", 1)
		mntOutsideEmpty(t, r.db, r.bt, "KSX-ELSEWHERE")
		r.sources(t, src.ID)
		got, st := r.asksFrom(t, "KSX")
		if st.Created != 0 || len(got) != 0 {
			t.Errorf("an empty outside the sources was asked for: %+v", st)
		}
		if !strings.Contains(st.HeldBy, "at any source") {
			t.Errorf("held by %q, want it to say no empty at any source", st.HeldBy)
		}
		if st.OriginID == "" {
			t.Errorf("the short kanban closed its episode: %+v", st)
		}
	})

	t.Run("the plant when the list is empty", func(t *testing.T) {
		t.Parallel()
		r := newSourcesRig(t, "KSE", 1)
		out := mntOutsideEmpty(t, r.db, r.bt, "KSE-ELSEWHERE")
		got, st := r.asksFrom(t, "KSE")
		if st.Created != 1 || !got[out] {
			t.Errorf("no sources: created %d, bins %v; want the plant's %d", st.Created, got, out)
		}
	})

	t.Run("the strict fence still applies to a source", func(t *testing.T) {
		t.Parallel()
		r := newSourcesRig(t, "KSF", 1)
		src := r.zone(t, "KSF-SRC", 1)
		r.stand(t, src, "KSF-STRICT-EMPTY")
		testdb.KanbanStrict(t, r.db, src.ID, true)
		r.sources(t, src.ID)
		if got, st := r.asksFrom(t, "KSF"); st.Created != 0 || len(got) != 0 {
			t.Errorf("a strict source was taken from: %+v", st)
		}
	})

	t.Run("a source's lanes are its own", func(t *testing.T) {
		t.Parallel()
		r := newSourcesRig(t, "KSL", 1)
		market, _, slots := laneGroup(t, r.db, "KSL-SM", 1, 2)
		in := landAt(t, r.db, r.bt, slots[0][1].ID, "KSL-IN-LANE")
		r.sources(t, market.ID)
		if got, st := r.asksFrom(t, "KSL"); st.Created != 1 || !got[in] {
			t.Errorf("created %d, bins %v; want the lane's %d", st.Created, got, in)
		}
	})

	t.Run("the destination's zone does not outrank the sources", func(t *testing.T) {
		t.Parallel()
		r := newSourcesRig(t, "KSZ", 1)
		second, first := r.zone(t, "KSZ-SECOND", 1), r.zone(t, "KSZ-FIRST", 1)
		near := r.stand(t, second, "KSZ-NEAR")
		far := r.stand(t, first, "KSZ-FAR")
		_, err := r.db.DB.Exec(`UPDATE nodes SET zone = 'Z1' WHERE parent_id = $1 OR parent_id = $2`, r.k, second.ID)
		testutil.MustNoErr(t, err, "zones")
		r.sources(t, first.ID, second.ID)
		got, st := r.asksFrom(t, "KSZ")
		if st.Created != 1 || !got[far] || got[near] {
			t.Errorf("created %d, bins %v; want the first source's %d, not the same zone's %d", st.Created, got, far, near)
		}
	})
}

// TestKanbanSources_NoLinesideRaid: a lineside kanban's keeper takes nothing from
// another kanban with supports, whether by the plant-wide scan or a sources entry; a
// kanban with no supports is a source like any other.
func TestKanbanSources_NoLinesideRaid(t *testing.T) {
	t.Parallel()

	lineside := func(t *testing.T, r *sourcesRig, name string) *nodes.Node {
		t.Helper()
		other := r.zone(t, name, 2)
		testutil.MustNoErr(t, r.db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: other.ID, BinTypeID: r.bt, Want: 0}), "its level")
		testdb.KanbanKeeper(t, r.db, other.ID, "test-core")
		r.stand(t, other, name+"-SPARE")
		return other
	}

	t.Run("by the plant-wide scan", func(t *testing.T) {
		t.Parallel()
		r := newSourcesRig(t, "KRP", 1)
		lineside(t, r, "KRP-K2")
		if got, st := r.asksFrom(t, "KRP"); st.Created != 0 || len(got) != 0 || st.HeldBy == "" {
			t.Errorf("the keeper raided a lineside kanban: %+v", st)
		}
	})

	t.Run("by a sources entry", func(t *testing.T) {
		t.Parallel()
		r := newSourcesRig(t, "KRS", 1)
		other := lineside(t, r, "KRS-K2")
		r.sources(t, other.ID)
		got, st := r.asksFrom(t, "KRS")
		if st.Created != 0 || len(got) != 0 || !strings.Contains(st.HeldBy, "at any source") {
			t.Errorf("the keeper raided a named lineside kanban: %+v", st)
		}
	})

	t.Run("a kanban with no supports is a source", func(t *testing.T) {
		t.Parallel()
		r := newSourcesRig(t, "KRZ", 1)
		zone := r.zone(t, "KRZ-Z45", 2)
		testutil.MustNoErr(t, r.db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: zone.ID, BinTypeID: r.bt, Want: 2}), "its level")
		in := r.stand(t, zone, "KRZ-LENT")
		if got, st := r.asksFrom(t, "KRZ"); st.Created != 1 || !got[in] {
			t.Errorf("created %d, bins %v; want Z45's %d by the plant-wide scan", st.Created, got, in)
		}
	})

	t.Run("a press's own pull still reaches its kanban", func(t *testing.T) {
		t.Parallel()
		r := newSourcesRig(t, "KRO", 0)
		other := lineside(t, r, "KRO-K2")
		sup, err := r.db.GetNodeByDotName("KRO-K2-SUPPORT")
		testutil.MustNoErr(t, err, "its support")
		b, err := r.db.FindEmptyBinOfType(r.code, "", sup.ID, bins.EmptyFence{ProcessNode: sup.Name}, domain.Anyone, domain.Height{})
		if err != nil || b == nil || !r.inGroup(t, b, other) {
			t.Errorf("a supported process's own find: %+v, %v; rule (iii) is the keeper's alone", b, err)
		}
	})
}

// TestKanbanForecast_AgreesWithTheFinder: the keeper's one forecast statement answers
// what the finder's pick answers for the same ask (an empty found, or found buried
// that a dig reaches, is something to source; anything else is not), with and without
// sources.
func TestKanbanForecast_AgreesWithTheFinder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, r *sourcesRig) []int64 // the sources
		want  bool
	}{
		{"nothing anywhere", func(*testing.T, *sourcesRig) []int64 { return nil }, false},
		{"an empty on a plain node", func(t *testing.T, r *sourcesRig) []int64 {
			mntOutsideEmpty(t, r.db, r.bt, r.code+"-OUT")
			return nil
		}, true},
		{"only in its own group", func(t *testing.T, r *sourcesRig) []int64 {
			landAt(t, r.db, r.bt, freePosition(t, r.db, r.k), r.code+"-OWN")
			return nil
		}, false},
		{"only in a strict group", func(t *testing.T, r *sourcesRig) []int64 {
			z := r.zone(t, r.code+"-STRICT", 1)
			r.stand(t, z, r.code+"-FENCED")
			testdb.KanbanStrict(t, r.db, z.ID, true)
			return nil
		}, false},
		{"only held by another order", func(t *testing.T, r *sourcesRig) []int64 {
			b := mntOutsideEmpty(t, r.db, r.bt, r.code+"-HELD")
			o := testdb.CreateOrder(t, r.db, func(o *orders.Order) {
				o.EdgeUUID, o.OrderType, o.DeliveryNode = "kfc-held-"+r.code, "move", "ELSEWHERE"
			})
			testdb.ReserveBin(t, r.db, o.ID, b)
			return nil
		}, false},
		{"only buried in a lane", func(t *testing.T, r *sourcesRig) []int64 {
			_, _, slots := laneGroup(t, r.db, r.code+"-M", 1, 2)
			var other int64
			testutil.MustNoErr(t, r.db.QueryRow(`INSERT INTO bin_types (code) VALUES ($1) RETURNING id`, r.code+"-OTHER").Scan(&other), "other type")
			landAt(t, r.db, other, slots[0][0].ID, r.code+"-FRONT")
			landAt(t, r.db, r.bt, slots[0][1].ID, r.code+"-BURIED")
			return nil
		}, true},
		{"at a source", func(t *testing.T, r *sourcesRig) []int64 {
			z := r.zone(t, r.code+"-SRC", 1)
			r.stand(t, z, r.code+"-AT-SRC")
			return []int64{z.ID}
		}, true},
		{"outside the sources", func(t *testing.T, r *sourcesRig) []int64 {
			mntOutsideEmpty(t, r.db, r.bt, r.code+"-OUT")
			return []int64{r.zone(t, r.code+"-SRC", 1).ID}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			name := "KFC" + strings.ReplaceAll(tc.name, " ", "")
			r := newSourcesRig(t, name, 2)
			sources := tc.setup(t, r)
			if len(sources) > 0 {
				r.sources(t, sources...)
			}
			id, err := r.db.OpenCoreEpisode(store.DemandOrigin{OriginID: "fcfcfcfc-0000-4000-8000-000000000001",
				EpisodeKey: protocol.KanbanEpisodeKey(1, 2), Kind: protocol.EpisodeKindKanban,
				StationID: "test-core", CoreNodeName: name, OpenedAt: time.Now().UTC(), Threshold: 2})
			testutil.MustNoErr(t, err, "the episode")

			got, err := r.db.SourceableTypes([]string{r.code}, bins.EmptyFence{OriginGroup: name, Sources: sources})
			testutil.MustNoErr(t, err, "the forecast")
			pick := dispatch.MapFinderOutcome(r.eng.Dispatcher().ForecastPick(dispatch.SourceNeed{
				Intent: dispatch.IntentEmpty, OriginID: id}))
			if picked := pick == dispatch.OutcomeFound || pick == dispatch.OutcomeReshuffle; picked != got[r.code] || picked != tc.want {
				t.Errorf("the finder's pick says %v, the forecast %v; want %v", picked, got[r.code], tc.want)
			}

			r.m.Tick()
			if st := mntState(t, r.m, name, r.code); (st.Created > 0) != tc.want {
				t.Errorf("the keeper asked %d, held by %q; want asked=%v", st.Created, st.HeldBy, tc.want)
			}
		})
	}
}
