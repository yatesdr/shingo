//go:build docker

package store_test

import (
	"database/sql"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
)

// publicTables counts the tables in schema public.
func publicTables(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')`).Scan(&n); err != nil {
		t.Fatalf("count public tables: %v", err)
	}
	return n
}

// TestResetDatabase_ThenBootTwice: --reset-db leaves schema public with
// nothing in it (no relation, function or type), the user that ran it able to
// create in it and PUBLIC holding USAGE, and the next boot brings the database
// up again, as does the one after. The tables-only reset this replaced left
// the schema's function and its free-standing sequence behind, and a boot
// that births the database on the baseline fails on "already exists" after it.
func TestResetDatabase_ThenBootTwice(t *testing.T) {
	t.Parallel()
	cfg := testdb.OpenEmpty(t)
	for round := 1; round <= 2; round++ {
		db, err := store.Open(cfg)
		if err != nil {
			t.Fatalf("round %d: boot before the reset: %v", round, err)
		}
		db.Close()
		if err := store.ResetDatabase(cfg); err != nil {
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
