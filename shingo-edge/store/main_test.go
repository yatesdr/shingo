package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain pre-warms the modernc.org/sqlite library's global one-time
// initialization (specifically _sqlite3MutexInit) on the main goroutine
// before any t.Parallel() test starts. Without this, two parallel
// tests in this package can race each other on the library's global
// static-variable writes — see modernc.org/sqlite/lib/sqlite_linux_amd64.go:16636.
// The race surfaced under `go test -race` post-2026-05 once parallel
// scheduling became fast enough to consistently trip the unlocked
// init path; it is not a race in our code.
//
// The warm-up is a single Open + Close against a throwaway file under
// the OS temp dir, which forces the library through the init path
// exactly once on a single goroutine. Subsequent Opens from parallel
// tests see the init complete and skip the racy code path.
func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "shingoedge-store-warmup-*")
	if err == nil {
		dbPath := filepath.Join(tmpDir, "warmup.db")
		if db, openErr := Open(dbPath); openErr == nil {
			_ = db.Close()
		}
		_ = os.RemoveAll(tmpDir)
	}
	os.Exit(m.Run())
}
