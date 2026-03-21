package store

import "testing"

func TestRecordInboundMessage(t *testing.T) {
	db := testDB(t)

	first, err := db.RecordInboundMessage("msg-1", "order.redirect", "edge.1")
	if err != nil {
		t.Fatalf("record first message: %v", err)
	}
	if !first {
		t.Fatalf("expected first record to be new")
	}

	second, err := db.RecordInboundMessage("msg-1", "order.redirect", "edge.1")
	if err != nil {
		t.Fatalf("record duplicate message: %v", err)
	}
	if second {
		t.Fatalf("expected duplicate record to report already seen")
	}
}
