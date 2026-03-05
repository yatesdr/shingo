package store

import (
	"os"
	"path/filepath"
	"testing"

	"shingocore/config"
)

// testDB creates a temporary SQLite database for testing.
func testDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(&config.DatabaseConfig{
		Driver: "sqlite",
		SQLite: config.SQLiteConfig{Path: dbPath},
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		os.Remove(dbPath)
	})
	return db
}

// --- Dialect tests ---

func TestRebind(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"SELECT * FROM t WHERE a=? AND b=?", "SELECT * FROM t WHERE a=$1 AND b=$2"},
		{"INSERT INTO t (a) VALUES (?)", "INSERT INTO t (a) VALUES ($1)"},
		{"SELECT 1", "SELECT 1"},
	}
	for _, tt := range tests {
		got := Rebind(tt.input)
		if got != tt.want {
			t.Errorf("Rebind(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
