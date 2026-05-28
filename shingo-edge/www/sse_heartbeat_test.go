package www

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// flushRecorder wraps httptest.ResponseRecorder with a Flush method so
// HandleSSE doesn't bail with "SSE not supported" — the real handler
// requires the writer to implement http.Flusher.
type flushRecorder struct {
	*httptest.ResponseRecorder
	mu       sync.Mutex
	flushed  []byte
	flushCnt int
}

func (f *flushRecorder) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushCnt++
	f.flushed = append(f.flushed, f.ResponseRecorder.Body.Bytes()[len(f.flushed):]...)
}

func (f *flushRecorder) snapshot() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.flushed)
}

// TestSSE_HeartbeatCarriesBuildID pins Field-notes Note 9b. The 30s
// keepalive used to be a bare `: keepalive` comment which `EventSource`
// strips before the JS client sees it — so the existing build-ID
// reload mechanism in shared/utils.js could never fire mid-stream when
// a reverse proxy held the connection open through an Edge restart.
// The fix replaces the comment with a named `heartbeat` event carrying
// the build id.
func TestSSE_HeartbeatCarriesBuildID(t *testing.T) {
	t.Parallel()
	prevInterval := sseKeepaliveInterval
	sseKeepaliveInterval = 25 * time.Millisecond
	t.Cleanup(func() { sseKeepaliveInterval = prevInterval })

	hub := NewEventHub()
	hub.Start()
	t.Cleanup(hub.Stop)

	req := httptest.NewRequest("GET", "/events", nil)
	ctx, cancel := contextWithTimeout(req, 250*time.Millisecond)
	req = req.WithContext(ctx)
	defer cancel()

	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	hub.HandleSSE(rec, req)

	body := rec.snapshot()
	if !strings.Contains(body, "event: connected") {
		t.Fatalf("expected 'connected' event in SSE stream; got:\n%s", body)
	}
	if !strings.Contains(body, "event: heartbeat") {
		t.Fatalf("expected named 'heartbeat' event in SSE stream (Note 9b regression: bare `: keepalive` comment doesn't reach EventSource); got:\n%s", body)
	}
	if !strings.Contains(body, `"build":"`+serverInstance+`"`) {
		t.Errorf("expected heartbeat to carry build id %q; got:\n%s", serverInstance, body)
	}
}
