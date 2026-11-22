package store

import (
	"path/filepath"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// What the running Edge takes from the shape of its schema: a constraint, a
// default, an index's predicate, a column a read scans. Each holds however the
// database was born.

// TestPositions_PutBackSurvivesARestart: taking a position off retires its row
// and putting the same node back makes a new one, on no screen or on the same
// screen. A restart leaves both as they were.
func TestPositions_PutBackSurvivesARestart(t *testing.T) {
	t.Parallel()
	for _, sameScreen := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "edge.db")
		db, err := Open(path)
		testutil.MustNoErr(t, err, "open")
		pid, err := db.CreateProcess("BACK-PROC", "", "", "", false)
		testutil.MustNoErr(t, err, "process")
		screen, err := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "BACK-SCREEN", Enabled: true})
		testutil.MustNoErr(t, err, "screen")
		position := func(on *int64) int64 {
			id, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, OperatorStationID: on,
				CoreNodeName: "BACK-W1", Name: "BACK-W1", Sequence: 1, Enabled: true})
			testutil.MustNoErr(t, err, "position")
			_, err = db.EnsureProcessNodeRuntime(id)
			testutil.MustNoErr(t, err, "runtime")
			return id
		}
		old := position(&screen)
		testutil.MustNoErr(t, db.DeleteProcessNode(old), "take it off")
		var on *int64
		if sameScreen {
			on = &screen
		}
		back := position(on)
		testutil.MustNoErr(t, db.Close(), "close")

		db, err = Open(path)
		testutil.MustNoErr(t, err, "restart")
		var liveBack, retiredOld, rows int
		testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*),
			COALESCE(SUM(id = ? AND deleted_at IS NULL), 0), COALESCE(SUM(id = ? AND deleted_at IS NOT NULL), 0)
			FROM process_nodes WHERE core_node_name = 'BACK-W1'`, back, old).Scan(&rows, &liveBack, &retiredOld), "read back")
		if rows != 2 || liveBack != 1 || retiredOld != 1 {
			t.Errorf("same screen %t: %d rows, live new %d, retired old %d after a restart; want 2, 1, 1",
				sameScreen, rows, liveBack, retiredOld)
		}
		testutil.MustNoErr(t, db.Close(), "close")
	}
}

// TestPositions_OneLiveRowPerNode: the database holds one live position per
// node in a process. A retired row of the node and any number of positions
// naming no Core node do not collide.
func TestPositions_OneLiveRowPerNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, err := db.CreateProcess("ONE-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "process")
	insert := func(node, code string, retired any) error {
		_, err := db.Exec(`INSERT INTO process_nodes (process_id, core_node_name, code, name, deleted_at) VALUES (?, ?, ?, ?, ?)`,
			pid, node, code, code, retired)
		return err
	}
	testutil.MustNoErr(t, insert("ONE-N1", "a", nil), "the first")
	if insert("ONE-N1", "b", nil) == nil {
		t.Error("a second live position of one node in one process was accepted")
	}
	testutil.MustNoErr(t, insert("ONE-N1", "c", "2026-11-01 00:00:00"), "a retired row beside the live one")
	testutil.MustNoErr(t, insert("", "d", nil), "a position naming no node")
	testutil.MustNoErr(t, insert("", "e", nil), "a second position naming no node")
}

// TestPiles_OneRowPerNodePartAndState: a pile's identity is (node, part, state).
func TestPiles_OneRowPerNodePartAndState(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	insert := func(state string) error {
		_, err := db.Exec(`INSERT INTO node_lineside_bucket (node_id, payload_code, qty, state) VALUES (1, 'SYN-PILE', 3, ?)`, state)
		return err
	}
	testutil.MustNoErr(t, insert("active"), "active")
	testutil.MustNoErr(t, insert("stranded"), "stranded beside it")
	if insert("stranded") == nil {
		t.Error("a second stranded pile of one part at one node was accepted")
	}
}

// TestClaimDefaults_AHandWrittenInsertArmsNothing: an INSERT that names neither
// column lands with Auto reorder off and Robot 1 refilling, and one that names
// no swap mode is refused.
func TestClaimDefaults_AHandWrittenInsertArmsNothing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, sid := seedProcessStyle(t, db, "DEF-PROC", "DEF-STYLE")
	_, err := db.Exec(`INSERT INTO style_node_claims (style_id, core_node_name, swap_mode) VALUES (?, 'DEF-N1', 'two_robot')`, sid)
	testutil.MustNoErr(t, err, "hand-written claim")
	var auto int
	var refill string
	testutil.MustNoErr(t, db.QueryRow(`SELECT auto_reorder, refill_by FROM style_node_claims`).Scan(&auto, &refill), "read back")
	if auto != 0 || refill != "clearing_robot" {
		t.Errorf("auto_reorder %d, refill_by %q; want 0 and clearing_robot", auto, refill)
	}
	if _, err := db.Exec(`INSERT INTO style_node_claims (style_id, core_node_name) VALUES (?, 'DEF-N2')`, sid); err == nil {
		t.Error("a claim naming no swap mode was accepted")
	}
}

// TestOrders_ARowStatingNoWantReadsNone: an order row written without the want
// reads as wanting no height.
func TestOrders_ARowStatingNoWantReadsNone(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, err := db.Exec(`INSERT INTO orders (uuid, order_type, status) VALUES ('WANT-NONE', 'retrieve', 'pending')`)
	testutil.MustNoErr(t, err, "order")
	o, err := db.GetOrderByUUID("WANT-NONE")
	testutil.MustNoErr(t, err, "read")
	if o.Want != (protocol.Height{}) {
		t.Errorf("want = %v, want none", o.Want)
	}
}

// TestCoreLoaderEdge_CachedAndKeptOnAScreen: the cache keeps the Edge Core
// names for a loader, a screen made for a loader names it, and an edit of the
// screen keeps that name.
func TestCoreLoaderEdge_CachedAndKeptOnAScreen(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testutil.MustNoErr(t, db.ReplaceCoreLoaders([]protocol.LoaderInfo{{Name: "NAMED Loader", LoaderKey: "loader:NAMED",
		Role: "produce", Layout: "shared_window", Replenishment: "operator", EdgeStationUID: "stn-a", EdgeDisplayName: "EDGE A",
		Positions: []protocol.LoaderPosition{{CoreNodeName: "NAMED-W1"}}}}), "cache a loader")
	l, err := db.GetCoreLoader("loader:NAMED")
	if err != nil || l == nil || l.EdgeStationUID != "stn-a" || l.EdgeDisplayName != "EDGE A" {
		t.Fatalf("cached loader = %+v, %v; want it naming stn-a, EDGE A", l, err)
	}
	pid, err := db.CreateProcess("NAMED-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "process")
	sid, err := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "MADE", Enabled: true, CoreLoaderKey: "loader:NAMED"})
	testutil.MustNoErr(t, err, "screen")
	testutil.MustNoErr(t, db.UpdateOperatorStation(sid, stations.Input{ProcessID: pid, Name: "RENAMED", Enabled: true}), "edit")
	st, err := db.GetOperatorStation(sid)
	testutil.MustNoErr(t, err, "read")
	if st.CoreLoaderKey != "loader:NAMED" || st.Name != "RENAMED" {
		t.Errorf("screen = %+v; want it renamed and still naming loader:NAMED", *st)
	}
}

// TestLoaderWindow_TheMarkOnlySets: MarkProcessNodesLoaderWindow marks the
// rows named and clears none.
func TestLoaderWindow_TheMarkOnlySets(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, err := db.CreateProcess("MARK-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "process")
	var ids []int64
	for i, name := range []string{"MARK-A", "MARK-B"} {
		id, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: name, Code: name, Name: name, Sequence: i + 1, Enabled: true})
		testutil.MustNoErr(t, err, name)
		ids = append(ids, id)
	}
	testutil.MustNoErr(t, db.MarkProcessNodesLoaderWindow(ids[:1]), "mark")
	testutil.MustNoErr(t, db.MarkProcessNodesLoaderWindow(nil), "mark none")
	nodes, err := db.ListProcessNodesByProcess(pid)
	testutil.MustNoErr(t, err, "list")
	got := map[string]bool{}
	for _, n := range nodes {
		got[n.CoreNodeName] = n.LoaderWindow
	}
	if !got["MARK-A"] || got["MARK-B"] {
		t.Errorf("marks = %v, want MARK-A marked and MARK-B not", got)
	}
}

// TestSchema_EveryBornDatabaseCanBindALanding: a v0.19.0 Core sends a landing
// only to a station registered at schema 20 or above, and the bind names
// placed_seq. A born database reports at least that version and has it.
func TestSchema_EveryBornDatabaseCanBindALanding(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	const v019LandingSchema = 20
	if v := db.AppliedSchemaVersion(); v < v019LandingSchema {
		t.Errorf("a born database reports v%d, below a v0.19.0 Core's landings (v%d)", v, v019LandingSchema)
	}
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('process_node_runtime_states')
		WHERE name = 'placed_seq'`).Scan(&n), "column")
	if n != 1 {
		t.Error("process_node_runtime_states has no placed_seq")
	}
}

// TestSchema_NoForeignKeyNamesAMissingTable: every REFERENCES clause of a born
// database names one of its tables.
func TestSchema_NoForeignKeyNamesAMissingTable(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	dangling := queryLines(t, db.DB, `SELECT m.name, f."table" FROM sqlite_master m, pragma_foreign_key_list(m.name) f
		WHERE m.type = 'table' AND f."table" NOT IN (SELECT name FROM sqlite_master WHERE type = 'table')`)
	if len(dangling) > 0 {
		t.Errorf("foreign keys naming a table that is not there: %v", dangling)
	}
}
