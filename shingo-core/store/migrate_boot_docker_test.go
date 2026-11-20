//go:build docker

package store_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"shingocore/config"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/schema"
)

// publicTables counts the tables in schema public, the fact store.migrate
// reads to tell a new database from a restored one.
func publicTables(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')`).Scan(&n); err != nil {
		t.Fatalf("count public tables: %v", err)
	}
	return n
}

// openRaw opens cfg without migrating, closed at cleanup.
func openRaw(t *testing.T, cfg *config.DatabaseConfig) *store.DB {
	t.Helper()
	db, err := store.OpenWithoutMigrate(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestBaseline_AFailureLateInItLeavesNoTable pins the baseline's atomicity:
// it is one statement, so a failure at its very end (a division by zero after
// the last statement) rolls every table back, and the next boot
// finds an empty database and births it whole. A baseline applied statement by
// statement would leave the schema behind with no schema_migrations, which
// store.migrate refuses as a partial restore, every boot after.
func TestBaseline_AFailureLateInItLeavesNoTable(t *testing.T) {
	t.Parallel()
	cfg := testdb.OpenEmpty(t)
	db := openRaw(t, cfg)
	if !strings.Contains(schema.Baseline, "CREATE TABLE public.orders") {
		t.Fatal("the embedded baseline creates no orders table: there is nothing to roll back, and the check below would pass on nothing")
	}
	if err := store.ApplyBaselineForTest(db, schema.Baseline+"\nSELECT 1/0;\n"); err == nil {
		t.Fatal("the baseline with a failing last statement applied without error")
	}
	if n := publicTables(t, db.DB); n != 0 {
		t.Fatalf("a baseline that failed at its last statement left %d tables in public: it is not one transaction", n)
	}
	reborn, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("the boot after the failed baseline: %v", err)
	}
	defer reborn.Close()
	var head int
	if err := reborn.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&head); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if head != store.LatestMigrationVersion() {
		t.Fatalf("the reborn database records v%d, want v%d", head, store.LatestMigrationVersion())
	}
}

// TestBaseline_PoolKeepsCoresSessionCaps: after a database is born on the
// baseline, every connection in the pool still carries Core's session caps,
// statement_timeout 30s and lock_timeout 3s (store.dsn). A pg_dump preamble
// turns both off with SET; one left in the baseline would outlive it on the
// connection that ran it, and that connection serves the plant afterwards.
func TestBaseline_PoolKeepsCoresSessionCaps(t *testing.T) {
	t.Parallel()
	cfg := testdb.OpenEmpty(t)
	db, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("open an empty database: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	open := db.Stats().OpenConnections
	if open == 0 {
		t.Fatal("the pool holds no connection after the boot: the connection that ran the baseline is not here to check")
	}
	var conns []*sql.Conn
	for range open {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("take a pool connection: %v", err)
		}
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i, c := range conns {
		var stmt, lock string
		if err := c.QueryRowContext(ctx, `SELECT current_setting('statement_timeout'), current_setting('lock_timeout')`).Scan(&stmt, &lock); err != nil {
			t.Fatalf("read connection %d's settings: %v", i+1, err)
		}
		if stmt != "30s" || lock != "3s" {
			t.Errorf("pool connection %d of %d has statement_timeout=%s lock_timeout=%s, want 30s and 3s: "+
				"the baseline changed a session setting", i+1, open, stmt, lock)
		}
	}
}

// TestMigrate_RefusesTablesWithoutSchemaMigrations: a database with tables and
// no schema_migrations is a partial restore. Core refuses it and writes
// nothing, rather than guess its shape.
func TestMigrate_RefusesTablesWithoutSchemaMigrations(t *testing.T) {
	t.Parallel()
	cfg := testdb.OpenEmpty(t)
	db := openRaw(t, cfg)
	if _, err := db.Exec(`CREATE TABLE public.orders (id bigint)`); err != nil {
		t.Fatalf("create a lone table: %v", err)
	}
	_, err := store.Open(cfg)
	if err == nil || !strings.Contains(err.Error(), "Core tables present but no schema_migrations: restore the whole dump") {
		t.Fatalf("boot on tables without schema_migrations: err %v, want the partial-restore refusal", err)
	}
	if n := publicTables(t, db.DB); n != 1 {
		t.Fatalf("the refused boot left %d tables in public, want the 1 it found: a refusal writes nothing", n)
	}
	var sm bool
	if err := db.QueryRow(`SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&sm); err != nil || sm {
		t.Fatalf("after the refusal schema_migrations exists=%v (err %v): a refusal writes nothing", sm, err)
	}
}

// TestMigrate_RefusesAHoleNamingItsVersions: a database missing a version of
// the retired chain is refused by name, with the release that applies it, and
// the refusal writes nothing. Holes, not MAX(version): this database still
// records v184.
func TestMigrate_RefusesAHoleNamingItsVersions(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	count := func() int {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
			t.Fatalf("count schema_migrations: %v", err)
		}
		return n
	}
	for _, c := range []struct{ drop, want string }{
		{`DELETE FROM schema_migrations WHERE version = 72`, "missing v72 of v1–v184"},
		{`DELETE FROM schema_migrations WHERE version BETWEEN 179 AND 184`, "missing v72, v179–v184 of v1–v184"},
	} {
		if _, err := db.Exec(c.drop); err != nil {
			t.Fatalf("%s: %v", c.drop, err)
		}
		before := count()
		_, err := store.Open(cfg)
		if err == nil || !strings.Contains(err.Error(), c.want) ||
			!strings.Contains(err.Error(), "start "+store.RetiredChainRelease+" on this database first") {
			t.Fatalf("boot after %s: err %v, want the refusal naming %q and %s", c.drop, err, c.want, store.RetiredChainRelease)
		}
		if after := count(); after != before {
			t.Fatalf("the refused boot changed schema_migrations from %d rows to %d: a refusal writes nothing", before, after)
		}
	}
}

// TestResetDatabase_ThenBootTwice: --reset-db leaves schema public with
// nothing in it (no relation, function or type), the user that ran it able to
// create in it and PUBLIC holding USAGE, and the next boot brings the database
// up again, as does the one after. The tables-only reset this replaced left
// the schema's function and its free-standing sequence behind, and a boot
// that births the database on the baseline fails on "already exists" after it.
func TestResetDatabase_ThenBootTwice(t *testing.T) {
	t.Parallel()
	resetThenBootTwice(t, store.ResetDatabase)
}

// TestResetDatabase_ObjectFallbackThenBootTwice: the reset where Postgres refuses
// Core's user the DROP SCHEMA (14 and older: the bootstrap superuser owns
// public) drops the schema's objects instead, and leaves the same nothing the
// next boot needs. Run here on the fallback directly, since this server lets
// the schema drop through.
func TestResetDatabase_ObjectFallbackThenBootTwice(t *testing.T) {
	t.Parallel()
	resetThenBootTwice(t, store.ResetPublicObjectsForTest)
}

// resetThenBootTwice boots an empty database, resets it with reset, checks the
// schema holds nothing and still grants what Core needs, boots it twice, and
// does all of that twice.
func resetThenBootTwice(t *testing.T, reset func(*config.DatabaseConfig) error) {
	t.Helper()
	cfg := testdb.OpenEmpty(t)
	for round := 1; round <= 2; round++ {
		db, err := store.Open(cfg)
		if err != nil {
			t.Fatalf("round %d: boot before the reset: %v", round, err)
		}
		db.Close()
		if err := reset(cfg); err != nil {
			t.Fatalf("round %d: reset: %v", round, err)
		}
		raw, err := store.OpenWithoutMigrate(cfg)
		if err != nil {
			t.Fatalf("round %d: open after the reset: %v", round, err)
		}
		var objects int
		var publicUsage, canCreate bool
		err = raw.QueryRow(`SELECT
			(SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public') +
			(SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'public') +
			(SELECT count(*) FROM pg_type ty JOIN pg_namespace n ON n.oid = ty.typnamespace WHERE n.nspname = 'public'),
			EXISTS (SELECT 1 FROM pg_namespace n, aclexplode(n.nspacl) a
			         WHERE n.nspname = 'public' AND a.grantee = 0 AND a.privilege_type = 'USAGE'),
			has_schema_privilege(current_user, 'public', 'CREATE')`).
			Scan(&objects, &publicUsage, &canCreate)
		raw.Close()
		if err != nil {
			t.Fatalf("round %d: read schema public after the reset: %v", round, err)
		}
		if objects != 0 {
			t.Errorf("round %d: the reset left %d objects in schema public; the next boot's baseline fails on the first", round, objects)
		}
		if !publicUsage || !canCreate {
			t.Errorf("round %d: schema public after the reset: PUBLIC has USAGE %v, the resetting user may CREATE in it %v; want both", round, publicUsage, canCreate)
		}
		for boot := 1; boot <= 2; boot++ {
			db, err := store.Open(cfg)
			if err != nil {
				t.Fatalf("round %d: boot %d after the reset: %v", round, boot, err)
			}
			n := publicTables(t, db.DB)
			db.Close()
			if n == 0 {
				t.Fatalf("round %d: boot %d after the reset left no table", round, boot)
			}
		}
	}
}
