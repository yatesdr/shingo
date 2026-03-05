package store

import "testing"

func TestOutboxCRUD(t *testing.T) {
	db := testDB(t)

	// Enqueue
	if err := db.EnqueueOutbox("shingo.dispatch", []byte(`{"test":true}`), "order.ack", "line-1"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	db.EnqueueOutbox("shingo.dispatch", []byte(`{"test":2}`), "order.update", "line-2")

	// List pending
	msgs, err := db.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2", len(msgs))
	}
	if msgs[0].Topic != "shingo.dispatch" {
		t.Errorf("topic = %q, want %q", msgs[0].Topic, "shingo.dispatch")
	}
	if msgs[0].MsgType != "order.ack" {
		t.Errorf("msg_type = %q, want %q", msgs[0].MsgType, "order.ack")
	}

	// Ack
	db.AckOutbox(msgs[0].ID)
	msgs2, _ := db.ListPendingOutbox(10)
	if len(msgs2) != 1 {
		t.Errorf("pending after ack = %d, want 1", len(msgs2))
	}

	// Increment retries
	db.IncrementOutboxRetries(msgs2[0].ID)
	msgs3, _ := db.ListPendingOutbox(10)
	if msgs3[0].Retries != 1 {
		t.Errorf("retries = %d, want 1", msgs3[0].Retries)
	}
}
