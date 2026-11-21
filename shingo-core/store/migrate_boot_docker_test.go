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
	if err == nil || !strings.Contains(err.Error(), "objects in schema public but no schema_migrations: restore the whole dump") {
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

// TestMigrate_RefusesAnyObjectWithoutSchemaMigrations: the partial-restore
// refusal is not about tables only. A public schema holding a function or a
// sequence and no schema_migrations was read as new, and the baseline then
// failed on "already exists" at every boot; it is refused, writing nothing.
// An extension's objects are not Core's and not a restore: a database holding
// only pgcrypto is born.
func TestMigrate_RefusesAnyObjectWithoutSchemaMigrations(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, sql string }{
		{"a function the baseline makes", `CREATE FUNCTION public.bin_types_family_of_one() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`},
		{"a sequence the baseline makes", `CREATE SEQUENCE public.orders_id_seq`},
	} {
		cfg := testdb.OpenEmpty(t)
		db := openRaw(t, cfg)
		if _, err := db.Exec(c.sql); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		_, err := store.Open(cfg)
		if err == nil || !strings.Contains(err.Error(), "no schema_migrations: restore the whole dump") {
			t.Errorf("boot on %s without schema_migrations: err %v, want the partial-restore refusal", c.name, err)
		}
		if n := publicTables(t, db.DB); n != 0 {
			t.Errorf("the refused boot on %s left %d tables in public: a refusal writes nothing", c.name, n)
		}
	}

	cfg := testdb.OpenEmpty(t)
	if _, err := openRaw(t, cfg).Exec(`CREATE EXTENSION pgcrypto`); err != nil {
		t.Fatalf("install pgcrypto in public: %v", err)
	}
	db, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("boot on a database holding only an extension: %v; an extension is not a partial restore", err)
	}
	db.Close()
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

// TestV185_TheJSONGuardNeedsNoTempPrivilege: v185's audit_log move reads
// new_value through a guard function it makes inside its transaction. Made in
// pg_temp it needed the TEMP privilege, and a Core role without TEMP failed
// v185, up and down, on every boot. It is made in schema public and dropped
// before the commit: here a role that holds no TEMP, may CREATE in public and
// owns every table there (it births the database) runs down 185 and the boot
// that brings v185 back, and afterwards no v185_jsonb is left in any schema.
func TestV185_TheJSONGuardNeedsNoTempPrivilege(t *testing.T) {
	t.Parallel()
	cfg := testdb.OpenEmpty(t)
	su := openRaw(t, cfg)

	tx, err := su.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(store.V185JSONGuardOnForTest); err != nil {
		tx.Rollback()
		t.Fatalf("make the JSON guard: %v", err)
	}
	var where string
	err = tx.QueryRow(`SELECT n.nspname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE p.proname = 'v185_jsonb'`).Scan(&where)
	tx.Rollback()
	if err != nil || where != "public" {
		t.Fatalf("the JSON guard is made in schema %q (err %v), want public", where, err)
	}

	role := "notemp_" + cfg.Postgres.Database
	for _, q := range []string{
		`CREATE ROLE ` + role + ` LOGIN PASSWORD 'notemp' NOSUPERUSER NOCREATEDB NOCREATEROLE`,
		`GRANT CREATE ON SCHEMA public TO ` + role,
		`REVOKE TEMPORARY ON DATABASE ` + cfg.Postgres.Database + ` FROM PUBLIC`,
	} {
		if _, err := su.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		su.Exec(`DROP OWNED BY ` + role)
		su.Exec(`DROP ROLE ` + role)
	})
	var temp bool
	if err := su.QueryRow(`SELECT has_database_privilege($1, current_database(), 'TEMP')`, role).Scan(&temp); err != nil || temp {
		t.Fatalf("the role holds TEMP %v (err %v): this test would pass whichever schema the guard is made in", temp, err)
	}
	asRole := *cfg
	asRole.Postgres.User, asRole.Postgres.Password = role, "notemp"

	born, err := store.Open(&asRole)
	if err != nil {
		t.Fatalf("birth on the baseline as a role without TEMP: %v", err)
	}
	born.Close()
	if err := testdb.MigrateDownThrough(&asRole, 185); err != nil {
		t.Fatalf("migrate down 185 as a role without TEMP: %v", err)
	}
	up, err := store.Open(&asRole)
	if err != nil {
		t.Fatalf("v185 up as a role without TEMP: %v", err)
	}
	up.Close()
	var head, guards int
	if err := su.QueryRow(`SELECT (SELECT max(version) FROM schema_migrations), (SELECT count(*) FROM pg_proc WHERE proname = 'v185_jsonb')`).Scan(&head, &guards); err != nil {
		t.Fatal(err)
	}
	if want := store.LatestMigrationVersion(); head != want || guards != 0 {
		t.Errorf("after down and up as a role without TEMP: head v%d, %d v185_jsonb functions left; want v%d and none", head, guards, want)
	}
}

// TestV185_PartitionIndexRenameTakesAnyChild: down and up rename the indexes
// of cell_part_events' partitions after the column they cover. Two children
// the rename failed on, every boot: one carrying an expression-only index (no
// column to be named for, so the catalog read scanned a NULL), and one whose
// name needs quoting (mixed case, which an unquoted ALTER INDEX folds). Both
// come through down 185 and the boot that brings v185 back: each child's
// column indexes are named for the column of the moment, and the expression
// index keeps the name it was given.
func TestV185_PartitionIndexRenameTakesAnyChild(t *testing.T) {
	t.Parallel()
	raw, cfg := testdb.OpenWithConfig(t)
	for _, q := range []string{
		`CREATE TABLE cell_part_events_2031_06 PARTITION OF cell_part_events FOR VALUES FROM ('2031-06-01') TO ('2031-07-01')`,
		`CREATE INDEX zz_cpe_expr_idx ON cell_part_events_2031_06 ((lower(station_uid)))`,
		`CREATE TABLE "cell_part_events_Q3" PARTITION OF cell_part_events FOR VALUES FROM ('2031-07-01') TO ('2031-10-01')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	check := func(stage, col, gone string) {
		t.Helper()
		rows, err := raw.Query(`SELECT tablename, indexname FROM pg_indexes
			WHERE schemaname = 'public' AND tablename IN ('cell_part_events_2031_06', 'cell_part_events_Q3') ORDER BY indexname`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		named := map[string]int{}
		expr := false
		for rows.Next() {
			var table, index string
			if err := rows.Scan(&table, &index); err != nil {
				t.Fatal(err)
			}
			switch {
			case index == "zz_cpe_expr_idx":
				expr = true
			case strings.Contains(index, "_"+gone+"_"):
				t.Errorf("%s: %s's index %s is named for %s", stage, table, index, gone)
			case strings.Contains(index, "_"+col+"_"):
				named[table]++
			}
		}
		if !expr {
			t.Errorf("%s: the expression index zz_cpe_expr_idx is gone or renamed", stage)
		}
		if named["cell_part_events_2031_06"] == 0 || named["cell_part_events_Q3"] == 0 {
			t.Errorf("%s: indexes named for %s per child %v, want some on each: the check had nothing to read", stage, col, named)
		}
	}
	check("at v185", "station_uid", "cell_id")
	if err := testdb.MigrateDownThrough(cfg, 185); err != nil {
		t.Fatalf("migrate down 185 with these partitions: %v", err)
	}
	check("after down", "cell_id", "station_uid")
	db, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("v185 up with these partitions: %v", err)
	}
	db.Close()
	check("after up", "station_uid", "cell_id")
}

// TestV185_AHeldReaderStopsUpAndDown: up and down each take ACCESS EXCLUSIVE
// on every table v185 touches before anything else (v185Begin), so a client
// holding a read on one stops them on the 3 s lock_timeout (55P03) with
// nothing changed, and the next try goes through. audit_log is the table this
// can see: its only change is a row move, whose UPDATE a reader's ACCESS SHARE
// does not block, so only the lock step stops v185 there.
func TestV185_AHeldReaderStopsUpAndDown(t *testing.T) {
	t.Parallel()
	raw, cfg := testdb.OpenWithConfig(t)
	held := func() func() {
		t.Helper()
		tx, err := raw.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`LOCK TABLE audit_log IN ACCESS SHARE MODE`); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		return func() { tx.Rollback() }
	}

	release := held()
	err := testdb.MigrateDownThrough(cfg, 185)
	release()
	if err == nil || !strings.Contains(err.Error(), "55P03") {
		t.Fatalf("down 185 beside a reader of audit_log: err %v, want the lock timeout (55P03)", err)
	}
	if !schema.ColumnExists(raw, "orders", "station_uid") {
		t.Fatal("the down stopped by the lock changed the schema: orders.station_uid is gone")
	}
	if err := testdb.MigrateDownThrough(cfg, 185); err != nil {
		t.Fatalf("down 185 with the reader gone: %v", err)
	}

	release = held()
	_, err = store.Open(cfg)
	release()
	if err == nil || !strings.Contains(err.Error(), "55P03") {
		t.Fatalf("v185 up beside a reader of audit_log: err %v, want the lock timeout (55P03)", err)
	}
	if !schema.ColumnExists(raw, "orders", "station_id") {
		t.Fatal("the up stopped by the lock changed the schema: orders.station_id is gone")
	}
	db, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("v185 up with the reader gone: %v", err)
	}
	db.Close()
}
