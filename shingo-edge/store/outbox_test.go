package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		os.Remove(dbPath)
	})
	return db
}

func TestOutbox_EnqueueAndList(t *testing.T) {
	db := testDB(t)

	id, err := db.EnqueueOutbox([]byte(`{"test":"data"}`), "order.request")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if id == 0 {
		t.Fatal("enqueue should return non-zero ID")
	}

	msgs, err := db.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("pending = %d, want 1", len(msgs))
	}
	if msgs[0].ID != id {
		t.Errorf("msg ID = %d, want %d", msgs[0].ID, id)
	}
	if msgs[0].MsgType != "order.request" {
		t.Errorf("msg type = %q, want %q", msgs[0].MsgType, "order.request")
	}
	if string(msgs[0].Payload) != `{"test":"data"}` {
		t.Errorf("payload = %q", msgs[0].Payload)
	}
}

func TestOutbox_AckRemovesFromPending(t *testing.T) {
	db := testDB(t)

	id, _ := db.EnqueueOutbox([]byte(`{}`), "test")

	if err := db.AckOutbox(id); err != nil {
		t.Fatalf("ack: %v", err)
	}

	msgs, _ := db.ListPendingOutbox(10)
	if len(msgs) != 0 {
		t.Errorf("pending after ack = %d, want 0", len(msgs))
	}
}

func TestOutbox_IncrementRetries(t *testing.T) {
	db := testDB(t)

	id, _ := db.EnqueueOutbox([]byte(`{}`), "test")

	for i := 0; i < 3; i++ {
		if err := db.IncrementOutboxRetries(id); err != nil {
			t.Fatalf("increment retries: %v", err)
		}
	}

	msgs, _ := db.ListPendingOutbox(10)
	if len(msgs) != 1 {
		t.Fatalf("pending = %d, want 1", len(msgs))
	}
	if msgs[0].Retries != 3 {
		t.Errorf("retries = %d, want 3", msgs[0].Retries)
	}
}

func TestOutbox_MaxRetriesExcluded(t *testing.T) {
	db := testDB(t)

	id, _ := db.EnqueueOutbox([]byte(`{}`), "test")

	// Increment to max retries
	for i := 0; i < MaxOutboxRetries; i++ {
		db.IncrementOutboxRetries(id)
	}

	// Should be excluded from pending (dead-lettered)
	msgs, _ := db.ListPendingOutbox(10)
	if len(msgs) != 0 {
		t.Errorf("pending after max retries = %d, want 0 (dead-lettered)", len(msgs))
	}
}

func TestOutbox_PurgeOld(t *testing.T) {
	db := testDB(t)

	// Enqueue and ack a message
	id, _ := db.EnqueueOutbox([]byte(`{}`), "test")
	db.AckOutbox(id)

	// Purge with a very short duration won't catch it (just created)
	deleted, err := db.PurgeOldOutbox(24 * time.Hour)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0 (too recent)", deleted)
	}
}

func TestOutbox_ListPendingLimit(t *testing.T) {
	db := testDB(t)

	for i := 0; i < 5; i++ {
		db.EnqueueOutbox([]byte(`{}`), "test")
	}

	msgs, _ := db.ListPendingOutbox(3)
	if len(msgs) != 3 {
		t.Errorf("pending with limit 3 = %d, want 3", len(msgs))
	}

	msgs, _ = db.ListPendingOutbox(10)
	if len(msgs) != 5 {
		t.Errorf("pending with limit 10 = %d, want 5", len(msgs))
	}
}
