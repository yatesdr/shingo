package orders

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
	"shingo/protocol"
	"shingo/protocol/testutil"
)

// list_active_window_test.go — the pin on the visibility window the Edge's
// orders history screen actually enforces.
//
// WHY THIS TEST EXISTS. protocol.IsOperatorVisible used to claim it decided
// what "appears on operator-facing HMI surfaces (edge ListActive, …)" — and
// nothing called it. ListActive and ListActiveByProcess share one window
// (operatorWindowSQL):
//
//	NOT IN ('cancelled','skipped') — never shown, terminal and unactionable
//	AND (NOT IN ('confirmed','failed') OR created_at > -7 days)
//
// so the truth is a WINDOW, not a status set: confirmed and failed orders
// disappear at 7 days, active statuses (faulted among them) never age out, and
// cancelled/skipped never appear at all. Until 2026-09-28 faulted aged out with
// confirmed and failed, which hid a non-terminal order the operator still had
// to act on (fact census B4). The dead predicate described a simpler
// rule (a pure status set with no time arm) that no surface enforced. It was
// deleted 2026-09-26 (see protocol/status.go); this test pins the real
// behaviour so the window cannot drift the way the comment did.
//
// The boundary is a strict inequality: exactly 7 days old is HIDDEN
// (`> datetime('now','-7 days')`); anything younger is shown.
//
// VERIFIED RED BY: flipping '-7 days' to '-6 days' in ListActive — every
// 7-day row flipped its expectation (7d and 7d+1h became visible; the
// 7d−1h rows stayed visible so the flip is visible in the diff, not a
// vacuous all-flip).
//
// A real SQLite database, not a source grep: the window is a SQL comparison
// against a SQL-computed instant, and what this pins is what the query
// RETURNS at the boundary, not what its text looks like. The DDL is the
// narrow slice ListActive reads (the reconciliation package's
// staleness_bound_test.go established the pattern): the select/join needs
// orders plus the three nullable LEFT JOIN lookups, and a NULL
// process_node_id satisfies all three joins.

// openWindowDB creates a fresh SQLite DB with just enough of the orders shape
// for ListActive's select + joins.
func openWindowDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "window.db"))
	testutil.MustNoErr(t, err, "open sqlite")
	t.Cleanup(func() { db.Close() })

	_, err = db.Exec(`
CREATE TABLE orders (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    uuid            TEXT NOT NULL UNIQUE,
    order_type      TEXT NOT NULL DEFAULT 'move',
    status          TEXT NOT NULL DEFAULT 'pending',
    process_node_id INTEGER,
    retrieve_empty  INTEGER NOT NULL DEFAULT 1,
    quantity        INTEGER NOT NULL DEFAULT 0,
    delivery_node   TEXT NOT NULL DEFAULT '',
    staging_node    TEXT NOT NULL DEFAULT '',
    source_node     TEXT NOT NULL DEFAULT '',
    load_type       TEXT NOT NULL DEFAULT '',
    waybill_id      TEXT,
    external_ref    TEXT,
    final_count     INTEGER,
    count_confirmed INTEGER NOT NULL DEFAULT 0,
    eta             TEXT,
    auto_confirm    INTEGER NOT NULL DEFAULT 0,
    staged_expire_at TEXT,
    bin_id          INTEGER,
    payload_code    TEXT NOT NULL DEFAULT '',
    payload_desc    TEXT NOT NULL DEFAULT '',
    sibling_order_id INTEGER,
    queue_reason    TEXT NOT NULL DEFAULT '',
    queue_code      TEXT NOT NULL DEFAULT '',
    authored_by     TEXT NOT NULL DEFAULT 'edge',
    origin_id       TEXT NOT NULL DEFAULT '',
    origin_class    TEXT NOT NULL DEFAULT '',
    fault_since     TEXT,
    fault_deadline  TEXT,
    fault_notice_after_s INTEGER NOT NULL DEFAULT 0,
    fault_ref       TEXT,
    departed_at     TEXT,
    cell_left_at    TEXT,
    station_wait    INTEGER,
    wait_kind       TEXT NOT NULL DEFAULT '',
    release_facts   TEXT,
    release_intent  TEXT NOT NULL DEFAULT '',
    release_held    TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now')),
    steps_json      TEXT
);
CREATE TABLE process_nodes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id INTEGER,
    operator_station_id INTEGER,
    core_node_name TEXT NOT NULL,
    name TEXT NOT NULL
);
CREATE TABLE operator_stations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL
);
CREATE TABLE processes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL
);`)
	testutil.MustNoErr(t, err, "create schema")
	return db
}

// seedOrder inserts an order row with an explicit status and created_at, and
// returns its uuid.
func seedOrder(t *testing.T, db *sql.DB, status string, createdAt time.Time) string {
	t.Helper()
	uuid := fmt.Sprintf("win-%s-%d", status, createdAt.UnixNano())
	_, err := db.Exec(`INSERT INTO orders (uuid, status, created_at) VALUES (?, ?, ?)`,
		uuid, status, createdAt.UTC().Format("2006-01-02 15:04:05"))
	testutil.MustNoErr(t, err, "seed order "+uuid)
	return uuid
}

// visible is the set of uuids ListActive returns.
func visible(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	got, err := ListActive(db)
	testutil.MustNoErr(t, err, "ListActive")
	set := make(map[string]bool, len(got))
	for i := range got {
		set[got[i].UUID] = true
	}
	return set
}

func TestListActiveWindow(t *testing.T) {
	db := openWindowDB(t)
	now := time.Now().UTC()

	// Ages chosen so each row sits unambiguously on its side of the 7-day
	// line under SQLite's second-resolution datetime arithmetic: 6d and
	// 7d−1h are inside the window, 7d, 7d+1h and 8d are outside.
	cases := []struct {
		status      protocol.Status
		age         time.Duration
		wantVisible bool
	}{
		// The 7-day statuses. Window members age out at exactly 7 days.
		{protocol.StatusConfirmed, 6 * 24 * time.Hour, true},
		{protocol.StatusConfirmed, 7*24*time.Hour - time.Hour, true},
		{protocol.StatusConfirmed, 7 * 24 * time.Hour, false}, // strict >
		{protocol.StatusConfirmed, 7*24*time.Hour + time.Hour, false},
		{protocol.StatusConfirmed, 8 * 24 * time.Hour, false},
		{protocol.StatusFailed, 6 * 24 * time.Hour, true},
		{protocol.StatusFailed, 7*24*time.Hour - time.Hour, true},
		{protocol.StatusFailed, 7 * 24 * time.Hour, false},
		{protocol.StatusFailed, 8 * 24 * time.Hour, false},
		// Active statuses never age out — including ancient ones. Faulted is
		// one: a non-terminal grace state the operator still has to act on.
		{protocol.StatusFaulted, 6 * 24 * time.Hour, true},
		{protocol.StatusFaulted, 7 * 24 * time.Hour, true},
		{protocol.StatusFaulted, 8 * 24 * time.Hour, true},
		{protocol.StatusQueued, 8 * 24 * time.Hour, true},
		{protocol.StatusInTransit, 8 * 24 * time.Hour, true},
		{protocol.StatusStaged, 8 * 24 * time.Hour, true},
		{protocol.StatusSourcing, 8 * 24 * time.Hour, true},
		{protocol.StatusPending, 8 * 24 * time.Hour, true},
		// Never shown, at any age — terminal with nothing for the operator.
		{protocol.StatusCancelled, 6 * 24 * time.Hour, false},
		{protocol.StatusCancelled, 8 * 24 * time.Hour, false},
		{protocol.StatusSkipped, 6 * 24 * time.Hour, false},
		{protocol.StatusSkipped, 8 * 24 * time.Hour, false},
	}

	for _, c := range cases {
		uuid := seedOrder(t, db, string(c.status), now.Add(-c.age))
		set := visible(t, db)
		if got := set[uuid]; got != c.wantVisible {
			t.Errorf("%s order created %v ago: ListActive shows it=%v, want %v",
				c.status, c.age, got, c.wantVisible)
		}
		// Remove the row so each case is judged alone against the window.
		_, err := db.Exec(`DELETE FROM orders WHERE uuid = ?`, uuid)
		testutil.MustNoErr(t, err, "cleanup "+uuid)
	}
}
