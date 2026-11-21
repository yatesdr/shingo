//go:build docker

package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/config"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// configFor writes a config file naming cfg's database and returns its path.
func configFor(t *testing.T, cfg *config.DatabaseConfig) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "shingocore.yaml")
	pg := cfg.Postgres
	body := fmt.Sprintf("database:\n  postgres:\n    host: %s\n    port: %d\n    database: %s\n    user: %s\n    password: %s\n    sslmode: disable\n",
		pg.Host, pg.Port, pg.Database, pg.User, pg.Password)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runMigrate(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// catalog is what "writes nothing" is held to: every relation in public, and
// schema_migrations' rows when it exists.
func catalog(t *testing.T, cfg *config.DatabaseConfig) string {
	t.Helper()
	db, err := store.OpenWithoutMigrate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rels, rows string
	if err := db.QueryRow(`SELECT COALESCE(string_agg(c.relname || ':' || c.relkind::text, ',' ORDER BY c.relname), '')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public'`).Scan(&rels); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rels, "schema_migrations:r") {
		if err := db.QueryRow(`SELECT string_agg(version::text, ',' ORDER BY version) FROM schema_migrations`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
	}
	return rels + "|" + rows
}

// checkLineRe is check's exit-0 stdout. Its reader is install-core.sh, which
// parses both numbers to print the rollback recipe (one `migrate down` per
// version from <target> down to <cur>+1): change the line and the installer
// with it.
var checkLineRe = regexp.MustCompile(`^database v(\d+) -> v(\d+)\n$`)

// TestMigrateCheck_TheInstallersLine pins `shingocore migrate check`'s exit 0
// line for each database Core would start on, by its exact format: an empty
// database (born on the baseline: v0), one at the version before this
// build's, and one at it. Each check writes nothing, an empty database's
// included (no schema_migrations is created). install-core.sh reads the line.
func TestMigrateCheck_TheInstallersLine(t *testing.T) {
	target := store.LatestMigrationVersion()

	empty := testdb.OpenEmpty(t)
	_, migratedCfg := testdb.OpenWithConfig(t)
	_, beforeCfg := testdb.OpenWithConfig(t)
	if err := store.MigrateDown(beforeCfg, target); err != nil {
		t.Fatalf("down %d to make the database before this build's: %v", target, err)
	}

	for _, c := range []struct {
		name string
		cfg  *config.DatabaseConfig
		cur  int
	}{
		{"an empty database", empty, 0},
		{"a database at the version before", beforeCfg, target - 1},
		{"a database at this build's version", migratedCfg, target},
	} {
		was := catalog(t, c.cfg)
		code, stdout, stderr := run(t, "-config", configFor(t, c.cfg), "check")
		if code != 0 || stderr != "" {
			t.Errorf("%s: exit %d stderr %q, want 0 and nothing", c.name, code, stderr)
		}
		m := checkLineRe.FindStringSubmatch(stdout)
		if m == nil {
			t.Errorf("%s: stdout %q is not one line `database v<cur> -> v<target>`", c.name, stdout)
			continue
		}
		cur, err := strconv.Atoi(m[1])
		testutil.MustNoErr(t, err, c.name+": the check's current version")
		if cur != c.cur {
			t.Errorf("%s: cur v%d, want v%d", c.name, cur, c.cur)
		}
		tgt, err := strconv.Atoi(m[2])
		testutil.MustNoErr(t, err, c.name+": the check's target version")
		if tgt != target {
			t.Errorf("%s: target v%d, want v%d", c.name, tgt, target)
		}
		if now := catalog(t, c.cfg); now != was {
			t.Errorf("%s: check changed the catalog:\n before %s\n after  %s", c.name, was, now)
		}
	}
}

// TestMigrateCheck_ARefusalIsExit1AndAnUnreadableDatabaseExit2: check prints
// migrate's refusal on stderr and exits 1, writing nothing; a database it
// cannot reach exits 2 with the error.
func TestMigrateCheck_ARefusalIsExit1AndAnUnreadableDatabaseExit2(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 72`); err != nil {
		t.Fatal(err)
	}
	was := catalog(t, cfg)
	code, stdout, stderr := run(t, "-config", configFor(t, cfg), "check")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "schema_migrations is missing v72") {
		t.Errorf("a hole at v72: exit %d stdout %q stderr %q, want exit 1 and the refusal on stderr", code, stdout, stderr)
	}
	if now := catalog(t, cfg); now != was {
		t.Errorf("the refused check changed the catalog")
	}

	tables := testdb.OpenEmpty(t)
	raw, err := sql.Open("pgx", fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=disable",
		tables.Postgres.Host, tables.Postgres.Port, tables.Postgres.Database, tables.Postgres.User, tables.Postgres.Password))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TABLE public.orders (id bigint)`); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run(t, "-config", configFor(t, tables), "check"); code != 1 || !strings.Contains(stderr, "restore the whole dump") {
		t.Errorf("tables without schema_migrations: exit %d stderr %q, want exit 1 and the refusal", code, stderr)
	}

	gone := *cfg
	gone.Postgres.Port = 1
	if code, stdout, stderr := run(t, "-config", configFor(t, &gone), "check"); code != 2 || stdout != "" || stderr == "" {
		t.Errorf("an unreachable database: exit %d stdout %q stderr %q, want exit 2 and the error", code, stdout, stderr)
	}
}

// TestMigrateDown_RunsOnlyTheNewest: `migrate down N` exits 0 when N is the
// newest version recorded and this build carries its down step, and 1 with
// the reason otherwise: once it has run, the newest is the one before.
func TestMigrateDown_RunsOnlyTheNewest(t *testing.T) {
	_, cfg := testdb.OpenWithConfig(t)
	target := store.LatestMigrationVersion()
	path := configFor(t, cfg)
	if code, _, stderr := run(t, "-config", path, "down", strconv.Itoa(target-1)); code != 1 || stderr == "" {
		t.Errorf("down v%d on a database at v%d: exit %d stderr %q, want 1 and the reason", target-1, target, code, stderr)
	}
	if code, stdout, stderr := run(t, "-config", path, "down", strconv.Itoa(target)); code != 0 || stderr != "" || stdout == "" {
		t.Fatalf("down v%d: exit %d stdout %q stderr %q, want 0", target, code, stdout, stderr)
	}
	if code, _, stderr := run(t, "-config", path, "down", strconv.Itoa(target)); code != 1 || !strings.Contains(stderr, "newest version recorded is") {
		t.Errorf("down v%d again: exit %d stderr %q, want 1 naming the newest", target, code, stderr)
	}
	code, stdout, _ := run(t, "-config", path, "check")
	if code != 0 || stdout != fmt.Sprintf("database v%d -> v%d\n", target-1, target) {
		t.Errorf("check after down: exit %d stdout %q", code, stdout)
	}
}
