package www

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// seedOutboxRow enqueues one row and, when exhausted is true, drives it past
// the retry cap so it lands in the dead-letter state. Rows are removed again on
// cleanup because testDB is shared across this package.
func seedOutboxRow(t *testing.T, msgType string, exhausted bool) int64 {
	t.Helper()
	id, err := testDB.EnqueueOutbox([]byte(`{"x":1}`), msgType)
	if err != nil {
		t.Fatalf("enqueue outbox: %v", err)
	}
	if exhausted {
		if err := testDB.MarkOutboxExhausted(id, "seeded by test"); err != nil {
			t.Fatalf("mark exhausted: %v", err)
		}
	}
	t.Cleanup(func() {
		if _, err := testDB.Exec(`DELETE FROM outbox WHERE id = ?`, id); err != nil {
			t.Errorf("cleanup outbox row %d: %v", id, err)
		}
	})
	return id
}

func getStatus(t *testing.T, h *Handlers, r *chi.Mux) map[string]any {
	t.Helper()
	r.Get("/status", h.apiStatus)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /status = %d, want 200 — a 503 means the engine no longer "+
			"satisfies statusEngine, which makes every assertion below vacuous",
			rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /status: %v (body %q)", err, rec.Body.String())
	}
	return body
}

// TestStatus_ReportsDeadLettersAndLastPublish is the regression guard for the
// gap that made the 2026-08-21 Springfield loss invisible.
//
// /status called itself the on-call surface and reported kafka_connected: true
// throughout an outage — the field is "a writer exists", and Connect() does no
// I/O, so it cannot go false once set. Meanwhile outbox_depth counts only rows
// under the retry cap, so as messages exhausted their retries the depth FELL,
// and a depth returning to zero looked like recovery. 120 messages were
// permanently lost while every field on this endpoint read healthy.
func TestStatus_ReportsDeadLettersAndLastPublish(t *testing.T) {
	h, r := newTestHandlers(t)

	seedOutboxRow(t, "test.pending", false)
	seedOutboxRow(t, "test.dead", true)
	seedOutboxRow(t, "test.dead", true)

	publishedAt := time.Date(2026, 8, 21, 19, 41, 10, 0, time.UTC)
	eng := h.orchestration.(*stubEngine)
	eng.statusKafkaConnected = true
	eng.statusLastPublishOK = false
	eng.statusLastPublishAt = publishedAt
	eng.statusLastPublishEver = true

	body := getStatus(t, h, r)

	dead, ok := body["dead_letters"]
	if !ok {
		t.Fatal("dead_letters missing — pending depth cannot be read on its own, " +
			"because it falls as rows die and that is indistinguishable from recovery")
	}
	if got := dead.(float64); got != 2 {
		t.Errorf("dead_letters = %v, want 2", got)
	}
	if got := body["outbox_depth"].(float64); got != 1 {
		t.Errorf("outbox_depth = %v, want 1 — exhausted rows must not be counted "+
			"as pending, they will never be sent again", got)
	}

	if _, ok := body["kafka_last_publish_ok"]; !ok {
		t.Fatal("kafka_last_publish_ok missing — it is the only field on this " +
			"endpoint that reports broker reachability")
	}
	if got := body["kafka_last_publish_ok"].(bool); got {
		t.Error("kafka_last_publish_ok = true, want false — the stub's last publish failed")
	}
	at, ok := body["kafka_last_publish_at"].(string)
	if !ok {
		t.Fatal("kafka_last_publish_at missing after a publish attempt")
	}
	if parsed, err := time.Parse(time.RFC3339Nano, at); err != nil {
		t.Errorf("kafka_last_publish_at %q does not parse: %v", at, err)
	} else if !parsed.Equal(publishedAt) {
		t.Errorf("kafka_last_publish_at = %v, want %v", parsed, publishedAt)
	}

	// kafka_connected keeps its name and its value. Renaming a key on an
	// on-call JSON surface breaks whatever is already parsing it; the fix is
	// the new field beside it, not a rename.
	if got, ok := body["kafka_connected"].(bool); !ok || !got {
		t.Errorf("kafka_connected = %v, want true — the existing field must keep "+
			"its meaning and name", body["kafka_connected"])
	}
}

// TestStatus_NoPublishYetOmitsTimestamp pins the freshly-booted case: an Edge
// that has not published anything must not report a failure it never had.
func TestStatus_NoPublishYetOmitsTimestamp(t *testing.T) {
	h, r := newTestHandlers(t)

	eng := h.orchestration.(*stubEngine)
	eng.statusLastPublishEver = false

	body := getStatus(t, h, r)

	if _, present := body["kafka_last_publish_at"]; present {
		t.Error("kafka_last_publish_at is present before any publish attempt — it " +
			"must be omitted, or a just-booted Edge reads as having failed")
	}
	if got := body["kafka_last_publish_ok"].(bool); got {
		t.Error("kafka_last_publish_ok = true before any publish attempt")
	}
}
