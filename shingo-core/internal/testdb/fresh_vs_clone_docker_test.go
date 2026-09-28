//go:build docker

package testdb

import (
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"testing"

	"shingocore/config"
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
// It lives in package testdb, not store, because making an EMPTY database
// needs the unexported admin connection; every exported opener clones the
// template.
func TestFreshAndTemplateCloneRecordTheSameMigrations(t *testing.T) {
	// OpenWithConfig brings the server and template up (or skips without Docker).
	_, cloneCfg := OpenWithConfig(t)

	reopened, err := store.Open(cloneCfg)
	if err != nil {
		t.Fatalf("re-open the template clone through the migrate path: %v", err)
	}
	defer reopened.Close()
	cloneRows := migrationVersions(t, reopened.DB)

	freshName := fmt.Sprintf("test_fresh_p%d", os.Getpid())
	admin, err := adminConn()
	if err != nil {
		t.Fatalf("admin connection: %v", err)
	}
	defer admin.Close()
	_, _ = admin.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %s", freshName))
	if _, err := admin.Exec(fmt.Sprintf("CREATE DATABASE %s", freshName)); err != nil {
		t.Fatalf("create empty database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, freshName)
		_, _ = admin.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %s", freshName))
	})
	fresh, err := store.Open(&config.DatabaseConfig{Postgres: config.PostgresConfig{
		Host: containerHost, Port: containerPort, Database: freshName,
		User: "test", Password: "test", SSLMode: "disable",
	}})
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
