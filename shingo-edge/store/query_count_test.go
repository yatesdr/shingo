package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The counter is the instrument every "constant number of queries" pin leans
// on, so it gets its own check: a known number of statements, sent every way
// the application sends them, must come back as that number. An instrument
// that under-counted would let a per-style loop pass as constant.
func TestOpenCounting_CountsEveryStatementPath(t *testing.T) {
	t.Parallel()
	db, counter, err := OpenCounting(filepath.Join(t.TempDir(), "count.db"))
	if err != nil {
		t.Fatalf("OpenCounting: %v", err)
	}
	defer db.Close()

	if got := counter.Count(); got != 0 {
		t.Fatalf("count after open = %d, want 0 — migrate and verifySchema must not leak into the caller's count", got)
	}

	// 1: direct Exec.
	if _, err := db.Exec(`CREATE TABLE probe (n INTEGER)`); err != nil {
		t.Fatalf("exec: %v", err)
	}
	// 2, 3: direct Exec with args, twice.
	for i := 0; i < 2; i++ {
		if _, err := db.Exec(`INSERT INTO probe (n) VALUES (?)`, i); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	// 4: QueryRow.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM probe`).Scan(&n); err != nil {
		t.Fatalf("query row: %v", err)
	}
	// 5: Query with rows.
	rows, err := db.Query(`SELECT n FROM probe`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	rows.Close()
	// 6, 7: a prepared statement executed twice.
	stmt, err := db.Prepare(`INSERT INTO probe (n) VALUES (?)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := stmt.Exec(10 + i); err != nil {
			t.Fatalf("stmt exec: %v", err)
		}
	}
	stmt.Close()
	// 8, 9: two statements inside one transaction (BEGIN/COMMIT are the
	// driver's own and are deliberately not counted).
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO probe (n) VALUES (?)`, 20); err != nil {
		t.Fatalf("tx exec: %v", err)
	}
	var inTx int
	if err := tx.QueryRow(`SELECT count(*) FROM probe`).Scan(&inTx); err != nil {
		t.Fatalf("tx query: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if got := counter.Count(); got != 9 {
		t.Errorf("count = %d, want 9 (1 DDL + 2 inserts + 1 QueryRow + 1 Query + 2 stmt execs + 2 in-tx)", got)
	}

	counter.Reset()
	if got := counter.Count(); got != 0 {
		t.Errorf("count after Reset = %d, want 0", got)
	}
}

// A migrated copy is the database a fresh Open makes, and a migrate() pass over
// it changes nothing: not the schema, not a row, not a pragma. Two things stand
// on this. The test pools (coverageDB here, internal/testdb.Open) hand out
// copies of a template instead of running the chain per test, which is only
// honest if a copy is what Open would have made. And a counting store opened on
// such a copy can skip the chain without counting against a different
// database: OpenCounting runs migrate() over the copy again, OpenMigrated does
// not, so the two must leave the same file.
//
// The one masked column is schema_migrations.applied_at, CURRENT_TIMESTAMP at
// the time the chain ran: the template's is older than a fresh Open's. It is
// masked only between the fresh file and the copy; the two copies, made from
// one template, must agree on it too.
func TestMigratedCopy_IsAFreshOpenAndAMigratePassLeavesItUnchanged(t *testing.T) {
	t.Parallel()
	fresh := filepath.Join(t.TempDir(), "fresh.db")
	db, err := Open(fresh)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db.Close()

	migrated := coveragePath(t)
	if db, err = OpenMigrated(migrated); err != nil {
		t.Fatalf("OpenMigrated: %v", err)
	}
	db.Close()

	counted := coveragePath(t)
	cdb, _, err := OpenCounting(counted)
	if err != nil {
		t.Fatalf("OpenCounting: %v", err)
	}
	cdb.Close()

	want := databaseDump(t, migrated, nil)
	// The dump must have had something to compare: the schema and the rows a
	// fresh database is born with.
	for _, must := range []string{"table orders ON orders", "rows schema_migrations="} {
		if !strings.Contains(want, must) {
			t.Fatalf("the dump of a migrated copy has no %q; it compared nothing", must)
		}
	}
	if got := databaseDump(t, counted, nil); got != want {
		t.Errorf("a migrate() pass over a migrated copy changed it, so a counting store that skips the pass "+
			"counts against a different database.\n%s", firstDumpDiff(want, got))
	}
	masked := map[string]bool{"schema_migrations.applied_at": true}
	if got, copyDump := databaseDump(t, fresh, masked), databaseDump(t, migrated, masked); got != copyDump {
		t.Errorf("a template copy is not the database a fresh Open makes.\n%s", firstDumpDiff(got, copyDump))
	}
}

// databaseDump is schemaDump plus the file's user_version and application_id
// and every row of every table, sqlite_sequence included, each table's rows in
// the order of all their columns. A column named in masked ("table.column")
// prints as <masked>.
func databaseDump(t *testing.T, path string, masked map[string]bool) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(schemaDump(t, path))
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open for dump: %v", err)
	}
	defer raw.Close()
	for _, p := range []string{"user_version", "application_id"} {
		var v int64
		if err := raw.QueryRow("PRAGMA " + p).Scan(&v); err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		fmt.Fprintf(&b, "pragma %s=%d\n", p, v)
	}
	var tables []string
	rows, err := raw.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	rows.Close()
	for _, table := range tables {
		b.WriteString(tableRowsDump(t, raw, table, masked))
	}
	return b.String()
}

// tableRowsDump prints one table's rows for databaseDump.
func tableRowsDump(t *testing.T, raw *sql.DB, table string, masked map[string]bool) string {
	t.Helper()
	quoted := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
	rows, err := raw.Query(`SELECT * FROM ` + quoted)
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	cols, err := rows.Columns()
	rows.Close()
	if err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	order := make([]string, len(cols))
	for i := range cols {
		order[i] = strconv.Itoa(i + 1)
	}
	rows, err = raw.Query(`SELECT * FROM ` + quoted + ` ORDER BY ` + strings.Join(order, ", "))
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		cells := make([]string, len(cols))
		for i, v := range vals {
			cells[i] = cols[i] + "=" + dumpValue(v)
			if masked[table+"."+cols[i]] {
				cells[i] = cols[i] + "=<masked>"
			}
		}
		lines = append(lines, "row "+table+": "+strings.Join(cells, " | "))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	return fmt.Sprintf("rows %s=%d\n", table, len(lines)) + strings.Join(append(lines, ""), "\n")
}

// dumpValue prints a scanned SQLite value with its Go type, so a 1 and a "1"
// differ.
func dumpValue(v any) string {
	switch v := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return fmt.Sprintf("x'%x'", v)
	case time.Time:
		return "time:" + v.UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprintf("%T:%v", v, v)
	}
}

// schemaDump is every sqlite_master row (type, name, table, CREATE text) in a
// fixed order. SQLite rewrites a table's stored CREATE text on ADD and DROP
// COLUMN, so a column coming back changes this dump. (internal/schemadump
// cannot be used here: it imports this package.)
func schemaDump(t *testing.T, path string) string {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open for dump: %v", err)
	}
	defer raw.Close()
	rows, err := raw.Query(`SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master
		ORDER BY type, name`)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var typ, name, tbl, text string
		if err := rows.Scan(&typ, &name, &tbl, &text); err != nil {
			t.Fatalf("dump scan: %v", err)
		}
		b.WriteString(typ + " " + name + " ON " + tbl + "\n" + text + "\n\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("dump rows: %v", err)
	}
	return b.String()
}

// firstDumpDiff is the first line at which two dumps differ.
func firstDumpDiff(a, b string) string {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(la) || i < len(lb); i++ {
		var x, y string
		if i < len(la) {
			x = la[i]
		}
		if i < len(lb) {
			y = lb[i]
		}
		if x != y {
			return "first difference at line " + strconv.Itoa(i+1) + ":\n  first: " + x + "\n  second: " + y
		}
	}
	return "(no line difference)"
}
