//go:build docker

package messaging

import (
	"reflect"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// groupsFixture adds to binsFixture an unloader: a consume loader with windows
// SMN_A2, SMN_A1 and UNWATCHED-W (in that order), its carts going to group
// KD-1. UNWATCHED-W holds a cart and is on no Edge's list.
type groupsFixture struct {
	binsFixture
	group     *nodes.Node
	unwatched int64 // the cart at UNWATCHED-W
	tote      int64 // bin type id
	db        *store.DB
	counter   *store.QueryCounter
	svc       *CoreDataService
	calls     []lineCall
}

// lineCall is what the build handed the line function.
type lineCall struct {
	group   string
	levels  int
	windows []protocol.NodeBinsRow
}

func newGroupsFixture(t *testing.T, levelled bool) *groupsFixture {
	t.Helper()
	db, cfg := testdb.OpenWithConfig(t)
	f := &groupsFixture{binsFixture: newBinsFixture(t, db), db: db}
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT id FROM bin_types WHERE code = 'TOTE'`).Scan(&f.tote), "tote type")
	grpID, err := nodes.CreateGroup(db.DB, "KD-1")
	testutil.MustNoErr(t, err, "group")
	f.group, err = db.GetNode(grpID)
	testutil.MustNoErr(t, err, "read group")
	w := &nodes.Node{Name: "UNWATCHED-W", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(w), "unwatched window")
	cart := &bins.Bin{BinTypeID: f.tote, Label: "TOTE-3", NodeID: &w.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(cart), "cart at the unwatched window")
	f.unwatched = cart.ID

	id, err := db.CreateLoader(loaders.Loader{Name: "UL-1", Role: loaders.RoleConsume,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator, OutboundDest: "KD-1"})
	testutil.MustNoErr(t, err, "loader")
	for i, win := range []int64{f.a2, f.a1, w.ID} {
		testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: win, SortOrder: i}), "window")
	}
	if levelled {
		testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{GroupNodeID: grpID, BinTypeID: f.tote, Want: 3}), "level")
	}

	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open counting db")
	t.Cleanup(func() { cdb.Close() })
	f.counter = counter
	f.svc = NewCoreDataService(cdb, &feedsPinResponder{}, service.EpochAnnounce{})
	f.svc.SetGroupLinesFunc(func(g *nodes.Node, levels []store.MaintainLevel, windows []protocol.NodeBinsRow) (*protocol.NodeBinsGroup, error) {
		f.calls = append(f.calls, lineCall{group: g.Name, levels: len(levels), windows: windows})
		if len(levels) == 0 {
			return nil, nil
		}
		return &protocol.NodeBinsGroup{Group: g.Name, Wants: []protocol.NodeBinsWant{{BinType: "TOTE", Want: 3}},
			Assignments: []protocol.NodeBinsAssignment{{Node: "SMN_A2", BinType: "TOTE"}}}, nil
	})
	return f
}

// A levelled group of a consume loader with a watched window gets its line in
// the snapshot. The line function is handed the group, its levels and every
// window of the loader in the plant's order: a watched window named as the
// Edge watches it, one it does not watch by the node's own name, each with the
// bin standing there. The build is its read snapshot's five statements plus the
// group's levels; the windows' bins ride the one bins read.
func TestBuildNodeBins_ALevelledGroupsLine(t *testing.T) {
	t.Parallel()
	f := newGroupsFixture(t, true)
	f.svc.HandleBinWatch(feedsPinEnv("edge.a"), binsWatchList(t, binsFixtureNames...))
	f.calls = nil

	f.counter.Reset()
	snap, _, err := f.svc.buildNodeBins("edge.a")
	testutil.MustNoErr(t, err, "build")
	if got := f.counter.Count(); got != 6 {
		t.Errorf("one build with one levelled group = %d statements (the line function's own excluded), want 6:\n%v",
			got, f.counter.Statements())
	}
	rows := f.wantRows()
	// A window row is the bin's row alone: the count of bins at the node and the
	// inbound orders are the snapshot row's, not the line's.
	window := func(r protocol.NodeBinsRow) protocol.NodeBinsRow { r.Bins = 0; return r }
	wantWindows := []protocol.NodeBinsRow{window(rows[3]), window(rows[2]),
		{Node: "UNWATCHED-W", State: protocol.BinsStateOccupied, Occupied: true, BinID: f.unwatched, BinLabel: "TOTE-3",
			BinTypeCode: "TOTE", Epoch: 1}}
	if len(f.calls) != 1 || f.calls[0].group != "KD-1" || f.calls[0].levels != 1 ||
		!reflect.DeepEqual(f.calls[0].windows, wantWindows) {
		t.Errorf("line function called with %+v\nwant KD-1, 1 level, windows\n %+v", f.calls, wantWindows)
	}
	wantGroups := []protocol.NodeBinsGroup{{Group: "KD-1", Wants: []protocol.NodeBinsWant{{BinType: "TOTE", Want: 3}},
		Assignments: []protocol.NodeBinsAssignment{{Node: "SMN_A2", BinType: "TOTE"}}}}
	if !reflect.DeepEqual(snap.Groups, wantGroups) {
		t.Errorf("groups = %+v, want %+v", snap.Groups, wantGroups)
	}
	if !reflect.DeepEqual(snap.Rows, rows) {
		t.Errorf("rows moved with the group: %+v", snap.Rows)
	}
	noGroups := *snap
	noGroups.Groups = []protocol.NodeBinsGroup{}
	d, err := protocol.NodeBinsDigest(noGroups)
	testutil.MustNoErr(t, err, "digest without the group line")
	if d == snap.Digest {
		t.Error("the group line is not in the digest")
	}

	// A cart moving at the unwatched window pushes the Edge: its line reads it.
	if got := f.svc.bins.watchingBin(0, []int64{*mustBin(t, f.db, f.unwatched).NodeID}); !reflect.DeepEqual(got, []string{"edge.a"}) {
		t.Errorf("a bin event at the unwatched window marks %v, want [edge.a]", got)
	}
}

func mustBin(t *testing.T, db *store.DB, id int64) *bins.Bin {
	t.Helper()
	b, err := db.GetBin(id)
	testutil.MustNoErr(t, err, "read bin")
	return b
}

// A group with no levels has no line and costs no statement: the build is its
// read snapshot's five statements and the line function is never called. A
// level saved on the group afterwards (BinsLevelChanged, the level save's hook)
// brings its line in on the next build, at one more statement.
func TestBuildNodeBins_NoLevelsNoGroupNoStatement(t *testing.T) {
	t.Parallel()
	f := newGroupsFixture(t, false)
	f.svc.HandleBinWatch(feedsPinEnv("edge.a"), binsWatchList(t, binsFixtureNames...))

	f.counter.Reset()
	snap, _, err := f.svc.buildNodeBins("edge.a")
	testutil.MustNoErr(t, err, "build")
	if got := f.counter.Count(); got != 5 {
		t.Errorf("one build with no levels = %d statements, want 5:\n%v", got, f.counter.Statements())
	}
	if len(snap.Groups) != 0 || len(f.calls) != 0 {
		t.Errorf("no levels: groups %+v, line calls %+v; want neither", snap.Groups, f.calls)
	}
	if b := renderTwiceCore(t, snap); !reflect.DeepEqual(snap.Groups, []protocol.NodeBinsGroup{}) {
		t.Errorf("groups sent as %s, want []", b)
	}

	testutil.MustNoErr(t, f.db.SetMaintainLevel(store.MaintainLevel{GroupNodeID: f.group.ID, BinTypeID: f.tote, Want: 3}), "level")
	f.svc.bins.delay = 20 * time.Millisecond
	f.svc.BinsLevelChanged(f.group.ID)
	settle(t, f.svc)
	f.calls = nil
	f.counter.Reset()
	snap, _, err = f.svc.buildNodeBins("edge.a")
	testutil.MustNoErr(t, err, "build")
	if len(snap.Groups) != 1 || len(f.calls) != 1 {
		t.Errorf("after a level save: groups %+v, line calls %d; want the line", snap.Groups, len(f.calls))
	}
	if got := f.counter.Count(); got != 6 {
		t.Errorf("after a level save: %d statements, want 6", got)
	}

	// Its levels removed, the group is read once more, found empty, and not
	// read again.
	_, err = f.db.DB.Exec(`DELETE FROM node_maintain_levels WHERE group_node_id = $1`, f.group.ID)
	testutil.MustNoErr(t, err, "remove level")
	for i, want := range []int64{6, 5} {
		f.counter.Reset()
		snap, _, err = f.svc.buildNodeBins("edge.a")
		testutil.MustNoErr(t, err, "build")
		if got := f.counter.Count(); got != want || len(snap.Groups) != 0 {
			t.Errorf("build %d after the levels went: %d statements, %d groups; want %d and none", i, got, len(snap.Groups), want)
		}
	}
}
