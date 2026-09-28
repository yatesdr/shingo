package migrate

import "testing"

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
