package messaging

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"shingo/protocol"
)

// snapshot_coalesce_test.go — a full snapshot supersedes its unsent predecessors.
//
// After an outage the edge held an hour of superseded lineside reports and
// published all of them on recovery, so Core processed an hour of history to
// reach where the newest message alone would have put it — and most of the
// burst was discarded at the ingestor for expiry before it got that far.
//
// The dangerous half of this feature is what it must NOT apply to. A sequenced
// delta deleted by its successor is a permanently wrong count, which is exactly
// what the guard list prevents.

func snapshotTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/outbox.db?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE outbox (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		topic TEXT NOT NULL,
		payload BLOB NOT NULL,
		msg_type TEXT NOT NULL,
		retries INTEGER NOT NULL DEFAULT 0,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		sent_at TIMESTAMP
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func unsentPayloads(t *testing.T, db *sql.DB, msgType string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT payload FROM outbox WHERE sent_at IS NULL AND msg_type = ? ORDER BY id`, msgType)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, string(p))
	}
	return out
}

func TestEnqueueSnapshot_SupersedesUnsentPredecessors(t *testing.T) {
	db := snapshotTestDB(t)

	// A delta enqueued between the snapshots. It must survive all of them.
	if _, err := Enqueue(db, []byte("delta-1"), protocol.SubjectBinUOPDelta); err != nil {
		t.Fatalf("enqueue delta: %v", err)
	}

	for _, body := range []string{"snap-1", "snap-2", "snap-3"} {
		if err := EnqueueSnapshot(db, [][]byte{[]byte(body)}, protocol.SubjectLinesideLevelReport); err != nil {
			t.Fatalf("EnqueueSnapshot(%s): %v", body, err)
		}
	}

	got := unsentPayloads(t, db, protocol.SubjectLinesideLevelReport)
	if len(got) != 1 || got[0] != "snap-3" {
		t.Errorf("unsent lineside rows = %v, want exactly [snap-3] — an older full "+
			"snapshot has no value once a newer one exists", got)
	}

	if deltas := unsentPayloads(t, db, protocol.SubjectBinUOPDelta); len(deltas) != 1 {
		t.Errorf("the bin_uop_delta was collaterally deleted (%v) — coalescing must "+
			"never touch another msg_type; a deleted delta is a permanently wrong count", deltas)
	}
}

// A multi-payload snapshot is one atomic set. plant.claims PublishAll emits a
// message per process, so a per-process enqueue would have each process delete
// the ones before it and leave Core mirroring exactly one.
func TestEnqueueSnapshot_MultiPayloadSetIsAtomic(t *testing.T) {
	db := snapshotTestDB(t)

	if err := EnqueueSnapshot(db, [][]byte{[]byte("old-A"), []byte("old-B")}, protocol.SubjectPlantClaims); err != nil {
		t.Fatalf("first set: %v", err)
	}
	if err := EnqueueSnapshot(db, [][]byte{[]byte("new-A"), []byte("new-B"), []byte("new-C")}, protocol.SubjectPlantClaims); err != nil {
		t.Fatalf("second set: %v", err)
	}

	got := unsentPayloads(t, db, protocol.SubjectPlantClaims)
	if len(got) != 3 {
		t.Fatalf("unsent claims rows = %v, want all three of the newest set", got)
	}
	for _, p := range got {
		if p == "old-A" || p == "old-B" {
			t.Errorf("a superseded process payload survived: %v", got)
		}
	}
}

// A dead-lettered snapshot is doubly worthless — superseded AND unsendable — and
// this is the only thing that clears one before the retention window.
func TestEnqueueSnapshot_ClearsDeadLetteredPredecessors(t *testing.T) {
	db := snapshotTestDB(t)

	if err := EnqueueSnapshot(db, [][]byte{[]byte("stale")}, protocol.SubjectLinesideLevelReport); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.Exec(`UPDATE outbox SET retries = ?`, MaxRetries); err != nil {
		t.Fatalf("dead-letter it: %v", err)
	}

	if err := EnqueueSnapshot(db, [][]byte{[]byte("fresh")}, protocol.SubjectLinesideLevelReport); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	got := unsentPayloads(t, db, protocol.SubjectLinesideLevelReport)
	if len(got) != 1 || got[0] != "fresh" {
		t.Errorf("unsent rows = %v, want [fresh] — a dead-lettered snapshot is "+
			"superseded like any other", got)
	}
}

// The guard is the whole safety story. Coalescing a sequenced delta or a
// discrete event is silent permanent data loss, so it must be refused rather
// than documented.
func TestEnqueueSnapshot_RefusesNonSnapshotSubjects(t *testing.T) {
	db := snapshotTestDB(t)

	for _, subject := range []string{
		protocol.SubjectBinUOPDelta,
		protocol.SubjectLinesideBucketDelta,
		protocol.SubjectProductionTick,
		protocol.SubjectDemandOrigin,
		protocol.TypeComplexOrderRequest,
		protocol.TypeOrderCancel,
	} {
		if err := EnqueueSnapshot(db, [][]byte{[]byte("x")}, subject); err == nil {
			t.Errorf("EnqueueSnapshot accepted %q — coalescing it would delete "+
				"messages nothing else resupplies", subject)
		}
	}
}
