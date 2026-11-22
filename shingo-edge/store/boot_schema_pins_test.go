package store

import (
	"database/sql"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// What an Edge's start does to the file it opens: to its schema, to its rows,
// and which files it takes. Each test goes through Open, the door a start uses.

// rawDB opens path with no migration and no pragma: the instrument these tests
// plant a file with and read one back through.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	testutil.MustNoErr(t, err, "raw open")
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func execAll(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		_, err := db.Exec(s)
		testutil.MustNoErr(t, err, s)
	}
}

// bornDB is the path of a closed database this build bore.
func bornDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "edge.db")
	db, err := Open(path)
	testutil.MustNoErr(t, err, "bear the database")
	testutil.MustNoErr(t, db.Close(), "close")
	return path
}

// startEdge opens path as a start does and closes it.
func startEdge(t *testing.T, path string) error {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		return err
	}
	return db.Close()
}

// queryLines is a query's rows, one string each, in the query's order, a
// row's values separated by one space.
func queryLines(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(query, args...)
	testutil.MustNoErr(t, err, query)
	defer rows.Close()
	cols, err := rows.Columns()
	testutil.MustNoErr(t, err, "columns")
	var out []string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		testutil.MustNoErr(t, rows.Scan(ptrs...), "scan")
		words := make([]string, len(cols))
		for i, v := range vals {
			if words[i] = v.String; !v.Valid {
				words[i] = "NULL"
			}
		}
		out = append(out, strings.Join(words, " "))
	}
	testutil.MustNoErr(t, rows.Err(), "rows")
	return out
}

// fileState is every row of every table and the stored schema text, keyed by
// object: what "this start changed nothing" is asserted over.
func fileState(t *testing.T, path string) map[string]string {
	t.Helper()
	db := rawDB(t, path)
	out := map[string]string{
		"sqlite_master": strings.Join(queryLines(t, db, `SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`), "\n"),
	}
	for _, name := range queryLines(t, db, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`) {
		lines := queryLines(t, db, `SELECT * FROM "`+name+`"`)
		slices.Sort(lines)
		out[name] = strings.Join(lines, "\n")
	}
	testutil.MustNoErr(t, db.Close(), "close")
	return out
}

// TestBoot_ClearsPointersAtEndedOrders pins the one data step a start keeps: a
// position's active_order_id and staged_order_id are cleared where they name an
// order that has ended, and nowhere else. The sequential backfill and the board
// read both pointers as they stand.
func TestBoot_ClearsPointersAtEndedOrders(t *testing.T) {
	t.Parallel()
	path := bornDB(t)
	raw := rawDB(t, path)
	ended := map[protocol.Status]bool{
		protocol.StatusConfirmed: true, protocol.StatusCancelled: true,
		protocol.StatusFailed: true, protocol.StatusSkipped: true,
	}
	want := map[int]string{}
	for i, st := range protocol.AllStatuses() {
		id := i + 1
		execAll(t, raw,
			fmt.Sprintf(`INSERT INTO orders (id, uuid, order_type, status) VALUES (%d, 'SWEEP-%d', 'retrieve', '%s')`, id, id, st),
			fmt.Sprintf(`INSERT INTO process_node_runtime_states (process_node_id, active_order_id, staged_order_id, remaining_uop_cached)
				VALUES (%d, %d, NULL, 7), (%d, NULL, %d, 9)`, id, id, id+100, id))
		pointer := fmt.Sprint(id)
		if ended[st] {
			pointer = "0"
		}
		want[id] = pointer + "/0/7"
		want[id+100] = "0/" + pointer + "/9"
	}
	// A pointer at an order this file does not hold is not this step's.
	execAll(t, raw, `INSERT INTO process_node_runtime_states (process_node_id, active_order_id, staged_order_id, remaining_uop_cached)
		VALUES (999, 9001, 9002, 3)`)
	want[999] = "9001/9002/3"
	testutil.MustNoErr(t, raw.Close(), "close")

	testutil.MustNoErr(t, startEdge(t, path), "start")

	rows, err := rawDB(t, path).Query(`SELECT process_node_id, COALESCE(active_order_id, 0), COALESCE(staged_order_id, 0),
		remaining_uop_cached FROM process_node_runtime_states`)
	testutil.MustNoErr(t, err, "read the positions")
	defer rows.Close()
	got := map[int]string{}
	for rows.Next() {
		var node, active, staged, uop int
		testutil.MustNoErr(t, rows.Scan(&node, &active, &staged, &uop), "scan")
		got[node] = fmt.Sprintf("%d/%d/%d", active, staged, uop)
	}
	testutil.MustNoErr(t, rows.Err(), "rows")
	if !maps.Equal(got, want) {
		t.Errorf("active/staged/count by position after a start:\n got  %v\n want %v", got, want)
	}
}

// TestBoot_ASecondStartChangesNothing: on a file this build bore, with rows in
// it and no pointer at an ended order, a start writes no row and moves no
// object. A value a person set is still there after the next start, and
// nothing dropped comes back.
func TestBoot_ASecondStartChangesNothing(t *testing.T) {
	t.Parallel()
	path := bornDB(t)
	raw := rawDB(t, path)
	execAll(t, raw,
		`INSERT INTO processes (id, name) VALUES (1, 'STILL-PROC')`,
		`INSERT INTO styles (id, process_id, name) VALUES (1, 1, 'STILL-STYLE')`,
		`INSERT INTO style_node_claims (id, style_id, core_node_name, role, swap_mode, payload_code, keep_staged_node, paired_core_node)
			VALUES (7, 1, 'STILL-NODE', 'produce', 'sequential', 'SYN-STILL', 'STILL-SPOT', 'STILL-BACK')`,
		`INSERT INTO orders (id, uuid, order_type, status, queue_reason) VALUES (1, 'STILL-1', 'retrieve', 'queued', 'waiting')`,
		`INSERT INTO process_nodes (id, process_id, core_node_name, code, name) VALUES (1, 1, 'STILL-NODE', 'still', 'STILL-NODE')`,
		`INSERT INTO process_node_runtime_states (process_node_id, active_order_id) VALUES (1, 1)`)
	testutil.MustNoErr(t, raw.Close(), "close")

	before := fileState(t, path)
	testutil.MustNoErr(t, startEdge(t, path), "start")
	after := fileState(t, path)
	for _, k := range slices.Sorted(maps.Keys(before)) {
		if before[k] != after[k] {
			t.Errorf("a start changed %s:\nbefore:\n%s\nafter:\n%s", k, before[k], after[k])
		}
	}
	if len(after) != len(before) {
		t.Errorf("a start left %d objects, found %d", len(after), len(before))
	}
}

// TestBoot_TheDataStepsAStartRuns: a start rewrites no row but the pointers
// above. Each case is the smallest file on which the retired chain rewrote one
// at every start.
func TestBoot_TheDataStepsAStartRuns(t *testing.T) {
	t.Parallel()
	const claim = `INSERT INTO style_node_claims (style_id, core_node_name, swap_mode, role, payload_code, allowed_payload_codes) VALUES `
	for _, c := range []struct {
		name  string
		seed  []string
		query string
		want  string
	}{
		{"styles and no process: none is invented",
			[]string{`INSERT INTO styles (name, deleted_at) VALUES ('GONE-STYLE', datetime('now'))`},
			`SELECT COUNT(*), COALESCE(MAX(name), '-') FROM processes`,
			"0 -"},
		{"a legacy staging column leaves the staging a person set",
			[]string{claim + `(1, 'LEG-1', 'two_robot', 'consume', '', '')`,
				`UPDATE style_node_claims SET inbound_staging = 'EDITED', staging_node = 'OLD'`},
			`SELECT inbound_staging FROM style_node_claims`,
			"EDITED"},
		{"a legacy source column leaves a source a person cleared",
			[]string{claim + `(1, 'LEG-2', 'two_robot', 'consume', '', '')`,
				`UPDATE style_node_claims SET inbound_source = '', inbound_source_node = 'OLD'`},
			`SELECT inbound_source FROM style_node_claims`,
			""},
		{"a bin_loader role is left",
			[]string{claim + `(1, 'LEG-3', 'two_robot', 'bin_loader', '', '')`},
			`SELECT role, swap_mode FROM style_node_claims`,
			"bin_loader two_robot"},
		{"a manual_swap claim's allowed parts are left",
			[]string{claim + `(1, 'LEG-4', 'manual_swap', 'produce', 'SYN-LEG', '')`},
			`SELECT allowed_payload_codes FROM style_node_claims`,
			""},
		{"the retired bucket delta's bookkeeping is left",
			[]string{`INSERT INTO inventory_delta_seq (scope_kind, scope_key, epoch) VALUES ('bucket', 'LEG-5', 0)`},
			`SELECT COUNT(*) FROM inventory_delta_seq`,
			"1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			path := bornDB(t)
			raw := rawDB(t, path)
			execAll(t, raw, c.seed...)
			testutil.MustNoErr(t, raw.Close(), "close")
			testutil.MustNoErr(t, startEdge(t, path), "start")
			if got := strings.Join(queryLines(t, rawDB(t, path), c.query), "; "); got != c.want {
				t.Errorf("after a start: %q, want %q", got, c.want)
			}
		})
	}
}

// TestClaimQuarantine_MirrorsEveryClaimColumn: the archive holds a column for
// every column of style_node_claims, so QuarantineLoaderClaims, which copies
// the live column list by name, can always archive a row in full. A migration
// that adds a claim column adds the mirror column too.
func TestClaimQuarantine_MirrorsEveryClaimColumn(t *testing.T) {
	t.Parallel()
	db := rawDB(t, bornDB(t))
	const columns = `SELECT name FROM pragma_table_info(?)`
	live := queryLines(t, db, columns, "style_node_claims")
	mirror := queryLines(t, db, columns, ClaimQuarantineTable)
	if len(live) < 30 {
		t.Fatalf("read %d claim columns: the check has no input", len(live))
	}
	for _, name := range live {
		if !slices.Contains(mirror, name) {
			t.Errorf("%s has no %s: add the mirror column in the migration that added the claim column", ClaimQuarantineTable, name)
		}
	}
}
