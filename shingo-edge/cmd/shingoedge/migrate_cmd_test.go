package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/backup"
	"shingoedge/internal/testdb"
	"shingoedge/store"
)

// TestMigrateCheck_ExitsAndSays is the installer's contract: 0 and the two
// versions, 1 for a refusal, 2 for what cannot be read; no byte changed.
// install-edge.sh reads the exit and prints the sentence.
func TestMigrateCheck_ExitsAndSays(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, plant string
		noConfig    bool
		staged      bool
		code        int
		out, errOut string
	}{
		{name: "at this build", code: 0, out: "database v61 -> v61"},
		{name: "at v59", plant: `DELETE FROM schema_migrations WHERE version >= 60`, code: 0, out: "database v59 -> v61"},
		{name: "a hole", plant: `DELETE FROM schema_migrations WHERE version = 30`, code: 1, errOut: "start it once"},
		{name: "a table gone at this build", plant: `DROP TABLE shifts`, code: 1, errOut: "table shifts"},
		{name: "no config", noConfig: true, code: 2, errOut: "is not there"},
		{name: "a staged restore that cannot be read", staged: true, code: 2, errOut: "staged restore"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			path := testdb.Path(t)
			if c.plant != "" {
				db, err := store.OpenMigrated(path)
				testutil.MustNoErr(t, err, "open")
				_, err = db.Exec(c.plant)
				testutil.MustNoErr(t, err, c.plant)
				testutil.MustNoErr(t, db.Close(), "close")
			}
			cfg := filepath.Join(t.TempDir(), "shingoedge.yaml")
			if !c.noConfig {
				testutil.MustNoErr(t, os.WriteFile(cfg, []byte("database_path: "+filepath.ToSlash(path)+"\n"), 0o600), "config")
			}
			if c.staged {
				testutil.MustNoErr(t, backup.StageRestoreArchive(cfg, "key", strings.NewReader("not an archive"), ""), "stage")
			}
			before, err := os.ReadFile(path)
			testutil.MustNoErr(t, err, "read")

			var out, errOut bytes.Buffer
			if code := runMigrate([]string{"-config", cfg, "check"}, &out, &errOut); code != c.code {
				t.Errorf("exit %d, want %d (%s)", code, c.code, errOut.String())
			}
			if !strings.Contains(out.String(), c.out) || !strings.Contains(errOut.String(), c.errOut) {
				t.Errorf("said %q and %q, want %q and %q", out.String(), errOut.String(), c.out, c.errOut)
			}
			after, err := os.ReadFile(path)
			testutil.MustNoErr(t, err, "read after")
			if !bytes.Equal(before, after) {
				t.Error("migrate check changed the database")
			}
		})
	}
}

// TestMigrateCheck_BesideARunningEdge: the installer asks while the service
// still runs, so from another process than the one holding the WAL. The check
// answers from what is committed, the -wal included, and changes no byte of
// the database, its -wal or its -shm.
func TestMigrateCheck_BesideARunningEdge(t *testing.T) {
	const childConfig = "EDGE_TEST_MIGRATE_CHECK_CONFIG"
	if cfg := os.Getenv(childConfig); cfg != "" {
		os.Exit(runMigrate([]string{"-config", cfg, "check"}, os.Stdout, os.Stderr))
	}
	path := testdb.Path(t)
	cfg := filepath.Join(t.TempDir(), "shingoedge.yaml")
	testutil.MustNoErr(t, os.WriteFile(cfg, []byte("database_path: "+filepath.ToSlash(path)+"\n"), 0o600), "config")
	running, err := store.Open(path)
	testutil.MustNoErr(t, err, "the running Edge")
	defer running.Close()

	files := []string{path, path + "-wal", path + "-shm"}
	check := func(wantCode int, want string) {
		t.Helper()
		var before [][]byte
		for _, f := range files {
			b, err := os.ReadFile(f)
			testutil.MustNoErr(t, err, f)
			before = append(before, b)
		}
		child := exec.Command(os.Args[0], "-test.run=^TestMigrateCheck_BesideARunningEdge$")
		child.Env = append(os.Environ(), childConfig+"="+cfg)
		out, err := child.CombinedOutput()
		if code := child.ProcessState.ExitCode(); code != wantCode || !strings.Contains(string(out), want) {
			t.Errorf("exit %d (%v) saying %q, want %d and %q", code, err, out, wantCode, want)
		}
		for i, f := range files {
			b, err := os.ReadFile(f)
			testutil.MustNoErr(t, err, f)
			if !bytes.Equal(before[i], b) {
				t.Errorf("migrate check changed %s", filepath.Base(f))
			}
		}
	}
	_, err = running.Exec(`INSERT INTO shifts (name, shift_number, start_time, end_time) VALUES ('A', 1, '06:00', '14:00')`)
	testutil.MustNoErr(t, err, "a write")
	check(0, "database v61 -> v61")
	// Committed, and in the -wal alone.
	_, err = running.Exec(`DELETE FROM schema_migrations WHERE version = 30`)
	testutil.MustNoErr(t, err, "a hole")
	check(1, "missing v30")
}
