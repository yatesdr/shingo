package messaging

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"shingo/protocol"
)

// doorbell_test.go — every Edge enqueue rings the outbox drainer.
//
// The drainer wakes on Notify and otherwise waits out its interval (5 s by
// default). The Edge has eight enqueue sites (order sender, UOP deltas,
// lineside levels, demand origins, supply refusals, plant claims, production
// reports, the sim); none rings the drainer itself. They all reach Enqueue or
// EnqueueSnapshot, which ring on a written row, so the eight are pinned here as
// two facts: those two ring, and nothing else writes an outbox row.

func countRings(t *testing.T) *int {
	t.Helper()
	n := 0
	fn := func() { n++ }
	SetEnqueueNotifier(fn)
	t.Cleanup(func() { SetEnqueueNotifier(nil) })
	return &n
}

func TestDoorbell_EnqueueRingsOncePerRow(t *testing.T) {
	db := snapshotTestDB(t)
	rings := countRings(t)
	for i := range 3 {
		if _, err := Enqueue(db, []byte("x"), protocol.SubjectBinUOPDelta); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if *rings != 3 {
		t.Fatalf("rings = %d after three enqueues, want 3", *rings)
	}
}

// TestDoorbell_SnapshotRingsOnceAfterCommit pins the snapshot's one ring for
// the batch. The drainer reads every pending row per pass, so one covers them
// all; a refused snapshot writes nothing and rings nothing.
func TestDoorbell_SnapshotRingsOnceAfterCommit(t *testing.T) {
	db := snapshotTestDB(t)
	rings := 0
	visible := -1
	SetEnqueueNotifier(func() {
		rings++
		// What the drainer would read at the ring: the batch must be committed.
		if err := db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE sent_at IS NULL`).Scan(&visible); err != nil {
			t.Errorf("count at ring: %v", err)
		}
	})
	t.Cleanup(func() { SetEnqueueNotifier(nil) })
	if err := EnqueueSnapshot(db, [][]byte{[]byte("a"), []byte("b"), []byte("c")}, protocol.SubjectPlantClaims); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if visible != 3 {
		t.Fatalf("%d rows visible when the doorbell rang, want 3: a ring before the commit races the drainer", visible)
	}
	if rings != 1 {
		t.Fatalf("rings = %d after a three-row snapshot, want 1", rings)
	}
	if err := EnqueueSnapshot(db, [][]byte{[]byte("d")}, protocol.SubjectBinUOPDelta); err == nil {
		t.Fatal("a sequenced delta was accepted as a snapshot")
	}
	if rings != 1 {
		t.Fatalf("rings = %d after a refused snapshot, want still 1", rings)
	}
}

// outboxInsert matches an INSERT into the outbox table on a line of Go source;
// comment lines are skipped.
var outboxInsert = regexp.MustCompile(`(?i)insert\s+into\s+outbox\b`)

// TestDoorbell_OnlyTheStoreInsertsIntoOutbox pins that no Edge path writes an
// outbox row except Enqueue, EnqueueSnapshot and EnqueueIn. Another writer would
// be a site the doorbell does not cover: its messages would wait out the drain
// interval. EnqueueIn writes on its caller's transaction and rings nothing,
// because the row is invisible until that commit; its callers ring
// NotifyEnqueued after committing.
func TestDoorbell_OnlyTheStoreInsertsIntoOutbox(t *testing.T) {
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
	if len(found) != 3 || found[0] != want || found[1] != want || found[2] != want {
		t.Fatalf("INSERT INTO outbox found at %v, want exactly three times (Enqueue, EnqueueSnapshot, "+
			"EnqueueIn) in %s: any other writer skips the doorbell", found, want)
	}
}
