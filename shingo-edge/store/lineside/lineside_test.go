package lineside

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
	"shingo/protocol/testutil"
)

// openTestDB creates a fresh SQLite DB, runs just enough of the edge
// schema to satisfy node_lineside_bucket, and seeds two processes and their
// nodes. Nodes 100 and 101 are process 1; node 102 is process 1 too and
// carries the same core name as node 100 (two local nodes, one place at
// Core); node 200 is process 2.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "lineside.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_foreign_keys=on")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ddl := `
CREATE TABLE processes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT ''
);
CREATE TABLE process_nodes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    core_node_name TEXT NOT NULL,
    code TEXT NOT NULL,
    name TEXT NOT NULL
);
CREATE TABLE node_lineside_bucket (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id      INTEGER NOT NULL REFERENCES process_nodes(id) ON DELETE CASCADE,
    payload_code TEXT NOT NULL,
    qty          INTEGER NOT NULL DEFAULT 0,
    state        TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'stranded')),
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (node_id, payload_code, state)
);

INSERT INTO processes (id, name) VALUES (1, 'Line 1'), (2, 'Line 2');
INSERT INTO process_nodes (id, process_id, core_node_name, code, name)
    VALUES (100, 1, 'ALN_002', 'aln-002', 'ALN_002'),
           (101, 1, 'ALN_003', 'aln-003', 'ALN_003'),
           (102, 1, 'ALN_002', 'aln-002b', 'ALN_002 B'),
           (200, 2, 'ALN_900', 'aln-900', 'ALN_900');
`
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("seed schema: %v", err)
	}
	return db
}

// pile reads one row by (node, payload, state); ok is false when there is none.
func pile(t *testing.T, db *sql.DB, nodeID int64, payload, state string) (qty int, ok bool) {
	t.Helper()
	err := db.QueryRow(`SELECT qty FROM node_lineside_bucket WHERE node_id=? AND payload_code=? AND state=?`,
		nodeID, payload, state).Scan(&qty)
	if err == sql.ErrNoRows {
		return 0, false
	}
	testutil.MustNoErr(t, err, "read pile")
	return qty, true
}

func seedStranded(t *testing.T, db *sql.DB, nodeID int64, payload string, qty int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO node_lineside_bucket (node_id, payload_code, qty, state) VALUES (?, ?, ?, ?)`,
		nodeID, payload, qty, StateStranded)
	testutil.MustNoErr(t, err, "seed stranded pile")
}

func TestCaptureCreatesActiveBucket(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	qty, err := Capture(db, 100, "P-500", 60)
	testutil.MustNoErr(t, err, "Capture")
	if qty != 60 {
		t.Fatalf("Capture returned qty %d, want 60", qty)
	}
	if got, ok := pile(t, db, 100, "P-500", StateActive); !ok || got != 60 {
		t.Fatalf("active pile = %d (present %v), want 60", got, ok)
	}
}

func TestCaptureZeroIsNoop(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	qty, err := Capture(db, 100, "P-500", 0)
	testutil.MustNoErr(t, err, "Capture 0")
	if qty != 0 {
		t.Fatalf("Capture 0 returned %d, want 0", qty)
	}
	var count int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM node_lineside_bucket`).Scan(&count), "count")
	if count != 0 {
		t.Fatalf("expected no rows inserted, got %d", count)
	}
}

func TestCaptureMergesWithExistingActive(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	_, err := Capture(db, 100, "P-500", 60)
	testutil.MustNoErr(t, err, "first Capture")
	qty, err := Capture(db, 100, "P-500", 25)
	testutil.MustNoErr(t, err, "merge Capture")
	if qty != 85 {
		t.Fatalf("merged qty = %d, want 85", qty)
	}
	var count int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM node_lineside_bucket`).Scan(&count), "count")
	if count != 1 {
		t.Fatalf("expected 1 row after merge, got %d", count)
	}
}

// A capture never reads or writes a stranded row: a part stranded at an earlier
// cutover stays stranded with its qty, and the pull is a new active pile beside
// it.
// Replaces TestCaptureReactivatesInactive, which flipped under change #2 (a
// stranded pile never revives).
func TestCapture_NeverTouchesAStrandedPile(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	seedStranded(t, db, 100, "P-500", 30)

	qty, err := Capture(db, 100, "P-500", 4)
	testutil.MustNoErr(t, err, "Capture")
	if qty != 4 {
		t.Errorf("Capture returned %d, want 4: the new active pile, not 34", qty)
	}
	if got, _ := pile(t, db, 100, "P-500", StateStranded); got != 30 {
		t.Errorf("stranded pile = %d, want 30 untouched", got)
	}
	if got, _ := pile(t, db, 100, "P-500", StateActive); got != 4 {
		t.Errorf("active pile = %d, want 4", got)
	}
}

func TestDrainDecrementsBucketFirst(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	_, err := Capture(db, 100, "P-500", 60)
	testutil.MustNoErr(t, err, "Capture")

	drained, err := Drain(db, 100, "P-500", 15)
	testutil.MustNoErr(t, err, "Drain 15")
	if drained != 15 {
		t.Fatalf("drained=%d, want 15", drained)
	}
	if got, _ := pile(t, db, 100, "P-500", StateActive); got != 45 {
		t.Fatalf("qty after drain=%d, want 45", got)
	}
}

func TestDrainCarriesRemainderWhenBucketEmpty(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	_, err := Capture(db, 100, "P-500", 10)
	testutil.MustNoErr(t, err, "Capture")

	drained, err := Drain(db, 100, "P-500", 25)
	testutil.MustNoErr(t, err, "Drain 25")
	if drained != 10 {
		t.Fatalf("drained=%d, want 10 (the pile's qty)", drained)
	}
	if _, ok := pile(t, db, 100, "P-500", StateActive); ok {
		t.Fatal("the pile was not deleted when it reached zero")
	}
}

func TestDrainWithNoBucketReturnsZero(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	drained, err := Drain(db, 100, "P-500", 5)
	testutil.MustNoErr(t, err, "Drain empty")
	if drained != 0 {
		t.Fatalf("drained=%d, want 0 on no match", drained)
	}
}

func TestDrainZeroIsNoop(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	_, err := Capture(db, 100, "P-500", 10)
	testutil.MustNoErr(t, err, "Capture")
	drained, err := Drain(db, 100, "P-500", 0)
	testutil.MustNoErr(t, err, "Drain 0")
	if drained != 0 {
		t.Fatalf("drained=%d, want 0", drained)
	}
	if got, _ := pile(t, db, 100, "P-500", StateActive); got != 10 {
		t.Fatalf("qty=%d, want 10 unchanged", got)
	}
}

// A stranded pile never drains: the tick flows through to the bin.
// Replaces TestDrainAcrossStyleCutover, whose style half went with change #1;
// the changeover-window drain it pinned is pinned end to end in
// engine/lineside_bucket_pins_test.go.
func TestDrain_NeverDrainsAStrandedPile(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	seedStranded(t, db, 100, "P-500", 30)

	drained, err := Drain(db, 100, "P-500", 5)
	testutil.MustNoErr(t, err, "Drain")
	if drained != 0 {
		t.Errorf("drained=%d from a stranded pile, want 0", drained)
	}
	if got, _ := pile(t, db, 100, "P-500", StateStranded); got != 30 {
		t.Errorf("stranded pile = %d, want 30 untouched", got)
	}
}

func TestListForNodeActiveFirst(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	seedStranded(t, db, 100, "P-500", 60)
	_, err := Capture(db, 100, "P-600", 40)
	testutil.MustNoErr(t, err, "Capture")

	list, err := ListForNode(db, 100)
	testutil.MustNoErr(t, err, "ListForNode")
	if len(list) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(list))
	}
	if list[0].State != StateActive || list[1].State != StateStranded {
		t.Fatalf("order = %s, %s; want active first", list[0].State, list[1].State)
	}
}

// One pile per (node, payload, state), enforced by the schema.
// Was TestUniqueActivePerNodeStylePart; the style left the key under change #1.
func TestUniquePerNodePayloadState(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	_, err := Capture(db, 100, "P-500", 60)
	testutil.MustNoErr(t, err, "Capture")
	if _, err := db.Exec(`INSERT INTO node_lineside_bucket (node_id, payload_code, qty, state) VALUES (100, 'P-500', 5, 'active')`); err == nil {
		t.Error("a second active P-500 pile at node 100 was accepted")
	}
	seedStranded(t, db, 100, "P-500", 5) // the other state is a different row
	if _, err := db.Exec(`INSERT INTO node_lineside_bucket (node_id, payload_code, qty, state) VALUES (100, 'P-500', 5, 'inactive')`); err == nil {
		t.Error("state 'inactive' was accepted; the CHECK allows active and stranded only")
	}
}

// THE CUTOVER. Every active pile at any node of the process folds into its
// (node, payload) stranded row, summed into one already there, and the active
// row goes. A node with no part change (101) is stranded too; another
// process's pile (200) is not.
func TestStrandProcess_FoldsEveryActivePileIntoItsStrandedRow(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	for _, c := range []struct {
		node int64
		part string
		qty  int
	}{{100, "P-500", 10}, {101, "P-700", 8}, {200, "P-900", 3}} {
		_, err := Capture(db, c.node, c.part, c.qty)
		testutil.MustNoErr(t, err, "Capture")
	}
	seedStranded(t, db, 100, "P-500", 5) // from an earlier cutover

	stranded, err := StrandProcess(db, 1)
	testutil.MustNoErr(t, err, "StrandProcess")

	if len(stranded) != 2 {
		t.Fatalf("stranded = %+v, want the two piles of process 1", stranded)
	}
	if got, _ := pile(t, db, 100, "P-500", StateStranded); got != 15 {
		t.Errorf("node 100 stranded P-500 = %d, want 15 (5 + 10)", got)
	}
	if got, _ := pile(t, db, 101, "P-700", StateStranded); got != 8 {
		t.Errorf("node 101 stranded P-700 = %d, want 8", got)
	}
	for _, n := range []struct {
		node int64
		part string
	}{{100, "P-500"}, {101, "P-700"}} {
		if _, ok := pile(t, db, n.node, n.part, StateActive); ok {
			t.Errorf("node %d still has an active %s pile after the strand", n.node, n.part)
		}
	}
	if got, ok := pile(t, db, 200, "P-900", StateActive); !ok || got != 3 {
		t.Errorf("process 2's pile = %d (present %v), want active 3: another process's cutover", got, ok)
	}
	for _, s := range stranded {
		if s.NodeID == 100 && (s.CoreNodeName != "ALN_002" || s.Qty != 10) {
			t.Errorf("stranded record %+v, want core ALN_002 and the 10 folded (not the row's 15)", s)
		}
	}
}

// Two local nodes with one core name are one place at Core: the level is the
// sum over both.
func TestLevel_SumsNodesSharingACoreName(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	_, err := Capture(db, 100, "P-500", 10)
	testutil.MustNoErr(t, err, "Capture 100")
	_, err = Capture(db, 102, "P-500", 7)
	testutil.MustNoErr(t, err, "Capture 102")
	seedStranded(t, db, 100, "P-500", 40)

	got, err := Level(db, "ALN_002", "P-500", StateActive)
	testutil.MustNoErr(t, err, "Level")
	if got != 17 {
		t.Errorf("active level = %d, want 17 (10 + 7)", got)
	}
	got, err = Level(db, "ALN_002", "P-500", StateStranded)
	testutil.MustNoErr(t, err, "Level stranded")
	if got != 40 {
		t.Errorf("stranded level = %d, want 40", got)
	}
	got, err = Level(db, "ALN_002", "P-999", StateActive)
	testutil.MustNoErr(t, err, "Level of a payload with no row")
	if got != 0 {
		t.Errorf("level of a payload with no row = %d, want 0", got)
	}
}

// ListKeys names every row once, with its core name; ListKeysForProcess only
// the process's.
func TestListKeys_NamesEveryRow(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	_, err := Capture(db, 100, "P-500", 10)
	testutil.MustNoErr(t, err, "Capture")
	seedStranded(t, db, 101, "P-700", 2)
	_, err = Capture(db, 200, "P-900", 3)
	testutil.MustNoErr(t, err, "Capture")

	all, err := ListKeys(db)
	testutil.MustNoErr(t, err, "ListKeys")
	want := []Key{
		{NodeID: 100, CoreNodeName: "ALN_002", PayloadCode: "P-500", State: StateActive},
		{NodeID: 101, CoreNodeName: "ALN_003", PayloadCode: "P-700", State: StateStranded},
		{NodeID: 200, CoreNodeName: "ALN_900", PayloadCode: "P-900", State: StateActive},
	}
	if len(all) != len(want) {
		t.Fatalf("ListKeys = %+v, want %+v", all, want)
	}
	for i := range want {
		if all[i] != want[i] {
			t.Errorf("ListKeys[%d] = %+v, want %+v", i, all[i], want[i])
		}
	}
	proc, err := ListKeysForProcess(db, 1)
	testutil.MustNoErr(t, err, "ListKeysForProcess")
	if len(proc) != 2 {
		t.Errorf("ListKeysForProcess(1) = %+v, want the two process-1 rows", proc)
	}
}
