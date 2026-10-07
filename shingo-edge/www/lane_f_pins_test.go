package www

import (
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/protocol"
)

// lane_f_pins_test.go — P0 pins for lane F's Edge Logs (diagnostics) Replay,
// taken at 5c0beb74. Predicted values are in the evidence folder's
// predictions/p0-bins-f.md.
//
// The page's replayDeadLetter (templates/diagnostics.html:121-134) handles a
// 409 and reloads on ok; any other failure is dropped without a word. These pin
// what the server answers for the replays that do nothing.

// TestPinEdgeLogs_ReplayUnknownRowAnswersOK: Requeue is an UPDATE … WHERE
// id = ? AND sent_at IS NULL (store/messaging/messaging.go:290-293), so an id
// that matches no row is a silent no-op. TODAY: 200 {"status":"ok"}.
func TestPinEdgeLogs_ReplayUnknownRowAnswersOK(t *testing.T) {
	h, _ := newTestHandlers(t)
	rec := postReplay(t, h, chi.NewRouter(), 987654321)
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"status\":\"ok\"}\n" {
		t.Errorf("replay of a missing row = %d %q, want 200 {\"status\":\"ok\"}", rec.Code, rec.Body.String())
	}
}

// TestPinEdgeLogs_ReplaySentRowAnswersOK: a row already delivered is skipped
// by the same WHERE, and the answer is still ok. TODAY: 200, retries unchanged.
func TestPinEdgeLogs_ReplaySentRowAnswersOK(t *testing.T) {
	h, _ := newTestHandlers(t)
	id := seedReplayRow(t, protocol.SubjectBinUOPDelta, time.Duration(0))
	if _, err := testDB.Exec(`UPDATE outbox SET sent_at = datetime('now') WHERE id = ?`, id); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	rec := postReplay(t, h, chi.NewRouter(), id)
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"status\":\"ok\"}\n" {
		t.Errorf("replay of a sent row = %d %q, want 200 {\"status\":\"ok\"}", rec.Code, rec.Body.String())
	}
	if got := rowRetries(t, id); got != outboxMaxRetries {
		t.Errorf("retries = %d, want %d unchanged (the replay did nothing)", got, outboxMaxRetries)
	}
}
