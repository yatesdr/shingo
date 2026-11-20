package migrate

import (
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"
)

// TestLatestVersion covers the pure list arithmetic: empty list reads 0
// (nothing to apply), an ordered list reads its last element, and a list
// that is not ascending still reports the max so a mis-ordered append
// cannot silently round the answer down.
func TestLatestVersion(t *testing.T) {
	if got := LatestVersion(nil); got != 0 {
		t.Errorf("nil list: LatestVersion = %d, want 0", got)
	}
	if got := LatestVersion([]Migration{}); got != 0 {
		t.Errorf("empty list: LatestVersion = %d, want 0", got)
	}
	if got := LatestVersion([]Migration{{Version: 1}, {Version: 2}, {Version: 3}}); got != 3 {
		t.Errorf("ordered list: LatestVersion = %d, want 3", got)
	}
	if got := LatestVersion([]Migration{{Version: 5}, {Version: 2}}); got != 5 {
		t.Errorf("unsorted list: LatestVersion = %d, want 5 (max, so a mis-ordered append cannot round down)", got)
	}
}

// TestCheckOrder: an ascending list passes (gaps are allowed); a duplicate or
// an entry below its predecessor fails, naming the entry.
func TestCheckOrder(t *testing.T) {
	for _, ok := range [][]Migration{nil, {{Version: 1}}, {{Version: 1}, {Version: 2}, {Version: 5}}} {
		if err := CheckOrder(ok); err != nil {
			t.Errorf("CheckOrder(%v) = %v, want nil", ok, err)
		}
	}
	for _, bad := range [][]Migration{
		{{Version: 1}, {Version: 2, Name: "dup"}, {Version: 2, Name: "dup"}},
		{{Version: 3}, {Version: 2, Name: "below"}},
	} {
		if err := CheckOrder(bad); err == nil {
			t.Errorf("CheckOrder(%v) = nil, want an error", bad)
		}
	}
}

// TestDialectSQLMatchesTheExtraction pins the constant SQL to the exact
// literals the original Core runner sent, because Core's docker snapshot
// gates diff DDL bytes, not intent: the Postgres CREATE text here must be
// byte-identical to what shingo-core/store/migrations.go sent before this
// package existed (it was extracted verbatim; see that file's history).
// The SQLite texts are the same statements with ? placeholders and a
// SQLite-legal applied_at default — and byte-identical to the CREATE
// appended to shingo-edge/store/schema/sqlite_ddl.go, which the Edge
// snapshot gate (TestSchemaSnapshotIsCurrent) pins from the other side.
func TestDialectSQLMatchesTheExtraction(t *testing.T) {
	if createTableSQL[Postgres] != `CREATE TABLE IF NOT EXISTS schema_migrations (
	version INTEGER PRIMARY KEY,
	applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)` {
		t.Errorf("postgres createTableSQL drifted from the extraction:\n got %q", createTableSQL[Postgres])
	}
	if createTableSQL[SQLite] != `CREATE TABLE IF NOT EXISTS schema_migrations (
	version INTEGER PRIMARY KEY,
	applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
)` {
		t.Errorf("sqlite createTableSQL drifted from the pinned text:\n got %q", createTableSQL[SQLite])
	}
	for d, ph := range map[Dialect]string{Postgres: "$1", SQLite: "?"} {
		for name, got := range map[string]string{
			"exists": existsSQL[d],
			"delete": deleteSQL[d],
			"insert": insertSQL[d],
		} {
			want := "version = " + ph
			if name == "insert" {
				want = "VALUES (" + ph + ")"
			}
			if !hasSubstring(got, want) {
				t.Errorf("dialect %v %sSQL = %q, want placeholder %q", d, name, got, ph)
			}
			other := "?"
			if ph == "?" {
				other = "$1"
			}
			if hasSubstring(got, other) {
				t.Errorf("dialect %v %sSQL = %q carries the OTHER dialect's placeholder %q", d, name, got, other)
			}
		}
	}
}

func hasSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// recordingDriver is a database/sql driver that answers every statement Run
// sends to a brand-new database and records the text of each one. The module
// is stdlib-only, so a real Postgres or SQLite is out of reach here; the text
// a dialect sends is what this package owns, and that is what it checks.
type recordingDriver struct {
	mu   sync.Mutex
	sent []string
}

func (d *recordingDriver) Open(string) (driver.Conn, error) { return &recordingConn{d: d}, nil }

func (d *recordingDriver) record(q string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent = append(d.sent, q)
}

type recordingConn struct{ d *recordingDriver }

func (c *recordingConn) Prepare(q string) (driver.Stmt, error) {
	return &recordingStmt{c: c, q: q}, nil
}
func (c *recordingConn) Close() error              { return nil }
func (c *recordingConn) Begin() (driver.Tx, error) { return recordingTx{}, nil }

type recordingTx struct{}

func (recordingTx) Commit() error   { return nil }
func (recordingTx) Rollback() error { return nil }

type recordingStmt struct {
	c *recordingConn
	q string
}

func (s *recordingStmt) Close() error  { return nil }
func (s *recordingStmt) NumInput() int { return -1 }
func (s *recordingStmt) Exec([]driver.Value) (driver.Result, error) {
	s.c.d.record(s.q)
	return driver.RowsAffected(1), nil
}

// Query answers the COUNT(*) of an empty schema_migrations with 0 and every
// EXISTS probe with false: a database that has never been migrated.
func (s *recordingStmt) Query([]driver.Value) (driver.Rows, error) {
	s.c.d.record(s.q)
	var v driver.Value = int64(0)
	if strings.Contains(s.q, "EXISTS") {
		v = false
	}
	return &oneRow{v: v}, nil
}

type oneRow struct {
	v    driver.Value
	done bool
}

func (r *oneRow) Columns() []string { return []string{"v"} }
func (r *oneRow) Close() error      { return nil }
func (r *oneRow) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.v
	return nil
}

// TestRun_SeedsTheBaselineWithTheDialectsPlaceholder: a chain adopted
// mid-life (baseline > 0) seeds its baseline row on an empty table, and that
// insert is the dialect's own text. It used to be a literal `?` for every
// dialect, which Postgres rejects as a syntax error, so a Postgres chain with
// a baseline could never boot. Red if seedBaseline sends anything but
// insertSQL[dialect].
func TestRun_SeedsTheBaselineWithTheDialectsPlaceholder(t *testing.T) {
	for _, tc := range []struct {
		d    Dialect
		name string
	}{{Postgres, "postgres"}, {SQLite, "sqlite"}} {
		rec := &recordingDriver{}
		drv := "migrate-recording-" + tc.name
		sql.Register(drv, rec)
		db, err := sql.Open(drv, "")
		if err != nil {
			t.Fatalf("%s: open: %v", tc.name, err)
		}
		if err := Run(db, tc.d, 7, nil); err != nil {
			t.Fatalf("%s: Run: %v", tc.name, err)
		}
		db.Close()

		var seeded []string
		for _, q := range rec.sent {
			if strings.HasPrefix(q, "INSERT INTO schema_migrations") {
				seeded = append(seeded, q)
			}
		}
		if len(seeded) != 1 || seeded[0] != insertSQL[tc.d] {
			t.Errorf("%s: the baseline seed sent %q, want exactly [%q] (the dialect's insertSQL)",
				tc.name, seeded, insertSQL[tc.d])
		}
	}
}
