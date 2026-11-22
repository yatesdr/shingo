package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shingo/protocol/testutil"
)

// dirBytes is every file beside path, by name: what "a refusal changes
// nothing" is asserted over, sidecars included.
func dirBytes(t *testing.T, path string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	testutil.MustNoErr(t, err, "read the directory")
	out := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(filepath.Dir(path), e.Name()))
		testutil.MustNoErr(t, err, e.Name())
		out[e.Name()] = string(b)
	}
	return out
}

// mustRefuse starts an Edge on path and requires the refusal, saying each of
// want, with every file beside the database as it was.
func mustRefuse(t *testing.T, path string, want ...string) {
	t.Helper()
	before := dirBytes(t, path)
	err := startEdge(t, path)
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("start = %v, want a refusal", err)
	}
	for _, w := range want {
		if !strings.Contains(refusal.Error(), w) {
			t.Errorf("the refusal %q does not say %q", refusal.Error(), w)
		}
	}
	after := dirBytes(t, path)
	if len(after) != len(before) {
		t.Errorf("a refusal left %d files, found %d", len(after), len(before))
	}
	for name, b := range before {
		if after[name] != b {
			t.Errorf("a refusal changed %s", name)
		}
	}
}

// TestBoot_TheFilesAStartTakes is what a start does with a file that is not
// this build's whole chain: refuses it by name and changes nothing (no byte,
// no journal-mode conversion, no startup step), or, for a table gone from a
// whole file, fails by name and creates nothing.
func TestBoot_TheFilesAStartTakes(t *testing.T) {
	t.Parallel()
	const pass = "install v0.19.0, start it once, then this"
	for _, c := range []struct {
		name  string
		plant string
		want  []string
	}{
		{"a hole", `DELETE FROM schema_migrations WHERE version = 30`, []string{"missing v30 of v1–v59", pass}},
		{"a head below the last", `DELETE FROM schema_migrations WHERE version > 40`, []string{"v41–v59", pass}},
		{"an empty version table", `DELETE FROM schema_migrations`, []string{"missing v1–v59", pass}},
		{"tables with no version table", `DROP TABLE schema_migrations`, []string{"no schema_migrations", "v1–v59", pass}},
		{"v1 gone, which v0.19.0 cannot refill", `DELETE FROM schema_migrations WHERE version = 1`,
			[]string{"missing v1 of", "v0.19.0 cannot refill v1", "restore a backup"}},
		{"a newer database", `INSERT INTO schema_migrations (version) VALUES (999)`, []string{"v999", "this build knows v"}},
	} {
		for _, journal := range []string{"WAL", "DELETE"} {
			t.Run(c.name+", "+journal, func(t *testing.T) {
				t.Parallel()
				path := bornDB(t)
				raw := rawDB(t, path)
				execAll(t, raw,
					`INSERT INTO orders (id, uuid, order_type, status) VALUES (1, 'REFUSED-1', 'retrieve', 'confirmed')`,
					`INSERT INTO process_node_runtime_states (process_node_id, active_order_id) VALUES (1, 1)`,
					c.plant,
					`PRAGMA journal_mode = `+journal)
				testutil.MustNoErr(t, raw.Close(), "close")
				mustRefuse(t, path, c.want...)
			})
		}
	}
	t.Run("a hand-dropped table", func(t *testing.T) {
		t.Parallel()
		path := bornDB(t)
		raw := rawDB(t, path)
		execAll(t, raw, `DROP TABLE shifts`)
		testutil.MustNoErr(t, raw.Close(), "close")
		err := startEdge(t, path)
		if err == nil || !strings.Contains(err.Error(), "table shifts") {
			t.Fatalf("start = %v, want a failure naming the table", err)
		}
		if n := queryLines(t, rawDB(t, path), `SELECT COUNT(*) FROM sqlite_master WHERE name = 'shifts'`); n[0] != "0" {
			t.Error("the start created shifts again, empty")
		}
	})
}

// TestBoot_ARefusalLeavesACrashedFileAlone: a killed Edge's -wal and -shm are
// part of "nothing": a refusal checkpoints neither.
func TestBoot_ARefusalLeavesACrashedFileAlone(t *testing.T) {
	t.Parallel()
	writing := bornDB(t)
	writer := rawDB(t, writing)
	execAll(t, writer, `PRAGMA journal_mode = WAL`, `DELETE FROM schema_migrations WHERE version = 30`)
	path := filepath.Join(t.TempDir(), "edge.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		image, err := os.ReadFile(writing + suffix)
		testutil.MustNoErr(t, err, "the crash image's "+suffix)
		testutil.MustNoErr(t, os.WriteFile(path+suffix, image, 0o600), "place it")
	}
	mustRefuse(t, path, "missing v30")
}

// TestBoot_AHotJournalIsRolledBack: a killed writer's rollback journal does
// not stop the next start (a read-only connection cannot roll one back).
func TestBoot_AHotJournalIsRolledBack(t *testing.T) {
	t.Parallel()
	writing := bornDB(t)
	writer := rawDB(t, writing)
	execAll(t, writer, `PRAGMA journal_mode = DELETE`, `PRAGMA cache_size = 1`, `BEGIN`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 300)
			INSERT INTO outbox (topic, payload, msg_type) SELECT 'shingo.orders', zeroblob(2000), 'crash.pin' FROM n`)
	path := filepath.Join(t.TempDir(), "edge.db")
	for _, suffix := range []string{"", "-journal"} {
		image, err := os.ReadFile(writing + suffix)
		testutil.MustNoErr(t, err, "the crash image's "+suffix)
		testutil.MustNoErr(t, os.WriteFile(path+suffix, image, 0o600), "place it")
	}
	testutil.MustNoErr(t, startEdge(t, path), "start over a hot journal")
	if n := queryLines(t, rawDB(t, path), `SELECT COUNT(*) FROM outbox`); n[0] != "0" {
		t.Errorf("outbox holds %s rows of a transaction that never committed", n[0])
	}
}

// TestBorn_RecordsEveryVersion: an empty file is born on the baseline with
// every version through this build's, and nothing else in it.
func TestBorn_RecordsEveryVersion(t *testing.T) {
	t.Parallel()
	db := rawDB(t, bornDB(t))
	versions := queryLines(t, db, `SELECT version FROM schema_migrations ORDER BY version`)
	if len(versions) != latestVersion() {
		t.Fatalf("a born database records %d versions, want v1–v%d", len(versions), latestVersion())
	}
	for _, table := range queryLines(t, db, `SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name <> 'schema_migrations'`) {
		if n := queryLines(t, db, `SELECT COUNT(*) FROM "`+table+`"`); n[0] != "0" {
			t.Errorf("a born %s holds %s rows", table, n[0])
		}
	}
}
