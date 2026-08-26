//go:build docker

package dispatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSharedFilesCarryNoSchemaMutation is the OpenShared lint guard.
//
// Files converted to testDBShared(t) share ONE database per file: every test
// in the file operates on the same rows. Two shapes break that contract and
// are easy to reintroduce by accident:
//
//   - DDL mutation (ALTER TABLE / RENAME / DROP COLUMN): renames leak to
//     sibling tests, and a heal that doesn't run on failure leaves the file's
//     database broken for every later test.
//   - Closing the DB mid-test (db.Close / db.DB.Close): the shared handle is
//     file-wide; one close kills every sibling's connection.
//
// This test fails the package loudly when either appears in a file that calls
// testDBShared. It runs under the docker tag with the rest of the package —
// the same place a sibling would fail, but with a message that names the
// contract instead of a duplicate-key error three tests later.
//
// The do-not-share list itself (FIFO-order files, global-unscoped asserts,
// fixed-world fixtures) is a REVIEW concern, not lintable shape: those files
// simply keep calling testDB(t), and nothing here distinguishes them.
func TestSharedFilesCarryNoSchemaMutation(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob test files: %v (files=%d) — cwd is %s", err, len(files), mustGetwd(t))
	}
	for _, file := range files {
		if file == "shared_open_shared_test.go" {
			continue // this guard names the patterns it forbids; don't match itself
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if !strings.Contains(string(body), "testDBShared(t)") {
			continue // not a shared file — per-test DBs may do anything
		}
		checkSharedFile(t, file, string(body))
	}
}

func checkSharedFile(t *testing.T, file, body string) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.Contains(line, "ALTER TABLE"),
			strings.Contains(line, "RENAME COLUMN"),
			strings.Contains(line, "RENAME TO"),
			strings.Contains(line, "DROP COLUMN"):
			if isGoString(line) {
				t.Errorf("%s: schema mutation in a testDBShared file (line %q) — "+
					"a shared-DB file cannot heal DDL for its siblings; revert this file to testDB(t)",
					file, strings.TrimSpace(line))
			}
		case strings.Contains(line, ".Close()"):
			if strings.Contains(line, "db.Close()") || strings.Contains(line, "db.DB.Close()") {
				t.Errorf("%s: DB close in a testDBShared file (line %q) — "+
					"the handle is shared by every test in the file; use testDB(t) if the test needs to close",
					file, strings.TrimSpace(line))
			}
		}
	}
}

// isGoString reports whether the mutation keywords sit inside a Go string
// literal (an actual SQL statement) rather than in a comment, where a mention
// like "// no ALTER TABLE here" is documentation, not mutation.
func isGoString(line string) bool {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "//") {
		return false
	}
	return strings.Contains(trimmed, "`") || strings.Contains(trimmed, `"`)
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		return "(unknown)"
	}
	return wd
}
