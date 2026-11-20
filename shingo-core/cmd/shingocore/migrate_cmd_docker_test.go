//go:build docker

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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
}
