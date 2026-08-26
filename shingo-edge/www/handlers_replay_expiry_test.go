package www

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/protocol"
	"shingoedge/store"
)

// outboxMaxRetries mirrors the cap the drainer enforces.
const outboxMaxRetries = store.MaxOutboxRetries

// handlers_replay_expiry_test.go — Replay must refuse an envelope Core will
// discard.
//
// On 2026-08-22 two dead-lettered production deltas were replayed from this
// button. The rows got sent_at, the edge logged "published outbox msg N", and
// the dead-letter count fell by two — while Core's ingestor threw both away
// because the envelopes had expired 23 hours earlier. Every layer reported a
// recovery that had not happened, and the only trace was one line in Core's
// journal carrying an envelope UUID.

func seedReplayRow(t *testing.T, subject string, ttl time.Duration) int64 {
	t.Helper()
	env, err := protocol.NewDataEnvelope(subject,
		protocol.Address{Role: protocol.RoleEdge, Station: "plant-a.line-1"},
		protocol.Address{Role: protocol.RoleCore},
		map[string]any{"bin_id": 27})
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	// Age the envelope by hand: NewDataEnvelope stamps from the clock, and the
	// case under test is a row that has been sitting in the outbox.
	if ttl < 0 {
		env.ExpiresAt = time.Now().UTC().Add(ttl)
	}
	data, err := env.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	id, err := testDB.EnqueueOutbox(data, subject)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := testDB.MarkOutboxExhausted(id, "seeded by test"); err != nil {
		t.Fatalf("mark exhausted: %v", err)
	}
	t.Cleanup(func() {
		if _, err := testDB.Exec(`DELETE FROM outbox WHERE id = ?`, id); err != nil {
			t.Errorf("cleanup row %d: %v", id, err)
		}
	})
	return id
}

func postReplay(t *testing.T, h *Handlers, r *chi.Mux, id int64) *httptest.ResponseRecorder {
	t.Helper()
	r.Post("/api/diagnostics/outbox/replay", h.apiReplayOutbox)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/api/diagnostics/outbox/replay?id="+strconv.FormatInt(id, 10), nil)
	r.ServeHTTP(rec, req)
	return rec
}

func rowRetries(t *testing.T, id int64) int {
	t.Helper()
	var n int
	if err := testDB.QueryRow(`SELECT retries FROM outbox WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("read retries: %v", err)
	}
	return n
}

func TestReplayOutbox_RefusesExpiredEnvelope(t *testing.T) {
	h, r := newTestHandlers(t)

	// plant.claims keeps its 5-minute TTL; back-date it so it has lapsed.
	id := seedReplayRow(t, protocol.SubjectPlantClaims, -time.Hour)

	rec := postReplay(t, h, r, id)

	if rec.Code != http.StatusConflict {
		t.Fatalf("replay of an expired envelope = %d, want 409 — the button must not "+
			"report a recovery Core will discard", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v (%q)", err, rec.Body.String())
	}
	if body["error"] == "" {
		t.Error("409 carried no reason — the operator has to be told WHY, or the " +
			"button just looks broken")
	}
	if got := rowRetries(t, id); got != outboxMaxRetries {
		t.Errorf("retries = %d, want %d unchanged — a refused replay must not "+
			"reset the counter and re-arm the row", got, outboxMaxRetries)
	}
}

// The two delta subjects carry NoExpiry since wave 2 unit 1, so they are always
// replayable — which is the case that most needed to work, since a lost delta
// is a permanently wrong count.
func TestReplayOutbox_NoExpirySubjectIsAlwaysReplayable(t *testing.T) {
	h, r := newTestHandlers(t)

	id := seedReplayRow(t, protocol.SubjectBinUOPDelta, 0)

	rec := postReplay(t, h, r, id)

	if rec.Code != http.StatusOK {
		t.Fatalf("replay of a NoExpiry delta = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := rowRetries(t, id); got != 0 {
		t.Errorf("retries = %d, want 0 — an accepted replay re-arms the row", got)
	}
}
