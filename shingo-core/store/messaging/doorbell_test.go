package messaging

import (
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// doorbell_test.go — every Core enqueue rings the outbox drainer.
//
// The drainer wakes on Notify and otherwise waits out its interval (5 s by
// default). Core has seven enqueue sites; none of them rings the drainer
// itself. They all reach EnqueueOutbox, which rings on a written row, or its
// twin EnqueueOutboxReturningID (an ask's answer, whose row id is kept for a
// replay), which rings the same way; so the sites are pinned here as two
// facts: both INSERTs ring, and there are only those two, in this file.

type fakeExecer struct{ err error }

func (f fakeExecer) Exec(string, ...any) (sql.Result, error) { return nil, f.err }

func countRings(t *testing.T) *int {
	t.Helper()
	n := 0
	fn := func() { n++ }
	SetEnqueueNotifier(fn)
	t.Cleanup(func() { SetEnqueueNotifier(nil) })
	return &n
}

func TestDoorbell_EnqueueRingsOncePerRow(t *testing.T) {
	rings := countRings(t)
	if err := EnqueueOutbox(fakeExecer{}, "shingo.dispatch", []byte("x"), "order.ack", "line-1"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if *rings != 1 {
		t.Fatalf("rings = %d after one enqueue, want 1", *rings)
	}
	if err := EnqueueOutbox(fakeExecer{err: errors.New("insert failed")}, "shingo.dispatch", []byte("x"), "order.ack", "line-1"); err == nil {
		t.Fatal("enqueue swallowed the insert error")
	}
	if *rings != 1 {
		t.Fatalf("rings = %d after a failed insert, want still 1: no row, nothing to drain", *rings)
	}
}

// outboxInsert matches an INSERT into the outbox table on a line of Go source;
// comment lines are skipped.
var outboxInsert = regexp.MustCompile(`(?i)insert\s+into\s+outbox\b`)

// TestDoorbell_OneInsertIntoOutbox pins that no Core path writes an outbox row
// except through EnqueueOutbox and EnqueueOutboxReturningID. A third INSERT
// would be a site the doorbell does not cover: its messages would wait out the
// drain interval.
func TestDoorbell_OneInsertIntoOutbox(t *testing.T) {
	root := filepath.Join("..", "..")
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(src), "\n") {
			if outboxInsert.MatchString(line) && !strings.HasPrefix(strings.TrimSpace(line), "//") {
				found = append(found, filepath.ToSlash(path))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	want := "../../store/messaging/messaging.go"
	if len(found) != 2 || found[0] != want || found[1] != want {
		t.Fatalf("INSERT INTO outbox found at %v, want exactly twice in %s (EnqueueOutbox, EnqueueOutboxReturningID): any other writer skips the doorbell", found, want)
	}
}
