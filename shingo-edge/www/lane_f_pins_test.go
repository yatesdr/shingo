package www

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/protocol"
)

// lane_f_pins_test.go — P0 pins for lane F's Edge Logs (diagnostics) Replay,
// taken at 5c0beb74 and moved by lane F (Edge Replay swallows failures).
//
// Before lane F a replay that did nothing answered 200 ok, and the page's
// replayDeadLetter dropped any non-409 failure without a word. AFTER: a
// missing row is a 404 and a delivered row a 409, each with the reason, and
// the page writes any failure beside the row.

// TestPinEdgeLogs_ReplayUnknownRowAnswersNotFound: an id that matches no row.
// AFTER: 404 {"error":"no outbox message N"}.
func TestPinEdgeLogs_ReplayUnknownRowAnswersNotFound(t *testing.T) {
	h, _ := newTestHandlers(t)
	rec := postReplay(t, h, chi.NewRouter(), 987654321)
	want := "{\"error\":\"no outbox message 987654321\"}\n"
	if rec.Code != http.StatusNotFound || rec.Body.String() != want {
		t.Errorf("replay of a missing row = %d %q, want 404 %q", rec.Code, rec.Body.String(), want)
	}
}

// TestPinEdgeLogs_ReplaySentRowAnswersConflict: a row already delivered.
// AFTER: 409 "already sent at … — nothing to replay"; retries unchanged.
func TestPinEdgeLogs_ReplaySentRowAnswersConflict(t *testing.T) {
	h, _ := newTestHandlers(t)
	id := seedReplayRow(t, protocol.SubjectBinUOPDelta, time.Duration(0))
	if _, err := testDB.Exec(`UPDATE outbox SET sent_at = datetime('now') WHERE id = ?`, id); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	rec := postReplay(t, h, chi.NewRouter(), id)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already sent at ") ||
		!strings.Contains(rec.Body.String(), "nothing to replay") {
		t.Errorf("replay of a sent row = %d %q, want 409 already sent … nothing to replay", rec.Code, rec.Body.String())
	}
	if got := rowRetries(t, id); got != outboxMaxRetries {
		t.Errorf("retries = %d, want %d unchanged (the replay did nothing)", got, outboxMaxRetries)
	}
}
