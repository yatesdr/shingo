//go:build docker

package testdb

import (
	"database/sql"
	"reflect"
	"testing"

	"shingocore/store"
)

// TestFreshAndTemplateCloneRecordTheSameMigrations pins that the two ways a
// Core database comes into being end on the same schema_migrations rows:
//
//   - FRESH: an empty database opened through store.Open, the full production
//     migrate path. This is what a new plant gets.
//   - CLONE: a copy of the test template, then store.Open again. This is what
//     every docker test runs against, and what a restarted plant looks like
//     (migrations already recorded; the runner skips them, verify hooks run).
//
// Lane J moved Core's runner into protocol/migrate as a pure move. A runner
// that recorded versions differently on first apply than on a re-open (a
// skipped row, a self-heal that deletes and does not re-insert, a baseline
// seeded where Core passes 0) shows up here as two different lists. Both lists
// must also end at store.LatestMigrationVersion().
//
// It lives in package testdb, beside OpenEmpty and the template it compares.
func TestFreshAndTemplateCloneRecordTheSameMigrations(t *testing.T) {
	// OpenWithConfig brings the server and template up (or skips without Docker).
	_, cloneCfg := OpenWithConfig(t)

	reopened, err := store.Open(cloneCfg)
	if err != nil {
		t.Fatalf("re-open the template clone through the migrate path: %v", err)
	}
	defer reopened.Close()
	cloneRows := migrationVersions(t, reopened.DB)

	fresh, err := store.Open(OpenEmpty(t))
	if err != nil {
		t.Fatalf("open + migrate an empty database: %v", err)
	}
	defer fresh.Close()
	freshRows := migrationVersions(t, fresh.DB)

	if !reflect.DeepEqual(freshRows, cloneRows) {
		t.Fatalf("schema_migrations differ:\n fresh (%d rows): %v\n clone (%d rows): %v",
			len(freshRows), freshRows, len(cloneRows), cloneRows)
	}
	want := store.LatestMigrationVersion()
	if n := len(freshRows); n == 0 || freshRows[n-1] != want {
		t.Fatalf("schema_migrations ends at %v, want head v%d", freshRows[len(freshRows)-1:], want)
	}
	t.Logf("fresh and clone both record %d versions, head v%d", len(freshRows), want)
}

func migrationVersions(t *testing.T, db *sql.DB) []int {
	t.Helper()
	rows, err := db.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan schema_migrations: %v", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema_migrations: %v", err)
	}
	return out
}
