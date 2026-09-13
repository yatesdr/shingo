package store

import (
	"path/filepath"
	"testing"
)

// The counter is the instrument every "constant number of queries" pin leans
// on, so it gets its own check: a known number of statements, sent every way
// the application sends them, must come back as that number. An instrument
// that under-counted would let a per-style loop pass as constant.
func TestOpenCounting_CountsEveryStatementPath(t *testing.T) {
	t.Parallel()
	db, counter, err := OpenCounting(filepath.Join(t.TempDir(), "count.db"))
	if err != nil {
		t.Fatalf("OpenCounting: %v", err)
	}
	defer db.Close()

	if got := counter.Count(); got != 0 {
		t.Fatalf("count after open = %d, want 0 — migrate and verifySchema must not leak into the caller's count", got)
	}

	// 1: direct Exec.
	if _, err := db.Exec(`CREATE TABLE probe (n INTEGER)`); err != nil {
		t.Fatalf("exec: %v", err)
	}
	// 2, 3: direct Exec with args, twice.
	for i := 0; i < 2; i++ {
		if _, err := db.Exec(`INSERT INTO probe (n) VALUES (?)`, i); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	// 4: QueryRow.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM probe`).Scan(&n); err != nil {
		t.Fatalf("query row: %v", err)
	}
	// 5: Query with rows.
	rows, err := db.Query(`SELECT n FROM probe`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	rows.Close()
	// 6, 7: a prepared statement executed twice.
	stmt, err := db.Prepare(`INSERT INTO probe (n) VALUES (?)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := stmt.Exec(10 + i); err != nil {
			t.Fatalf("stmt exec: %v", err)
		}
	}
	stmt.Close()
	// 8, 9: two statements inside one transaction (BEGIN/COMMIT are the
	// driver's own and are deliberately not counted).
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO probe (n) VALUES (?)`, 20); err != nil {
		t.Fatalf("tx exec: %v", err)
	}
	var inTx int
	if err := tx.QueryRow(`SELECT count(*) FROM probe`).Scan(&inTx); err != nil {
		t.Fatalf("tx query: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if got := counter.Count(); got != 9 {
		t.Errorf("count = %d, want 9 (1 DDL + 2 inserts + 1 QueryRow + 1 Query + 2 stmt execs + 2 in-tx)", got)
	}

	counter.Reset()
	if got := counter.Count(); got != 0 {
		t.Errorf("count after Reset = %d, want 0", got)
	}
}
