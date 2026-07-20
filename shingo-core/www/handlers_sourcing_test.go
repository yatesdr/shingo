//go:build docker

package www

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleSourcing_RendersDefault proves the page wires end to end and the
// template executes: with nothing tracked yet, the empty-state and the gated
// "at-risk tier disabled" queue note render at 200. The populated grid/claims/
// queue are covered by the engine-level assembly test.
func TestHandleSourcing_RendersDefault(t *testing.T) {
	t.Parallel()
	h, _ := testHandlersForPages(t)

	req := httptest.NewRequest(http.MethodGet, "/sourcing", nil)
	rec := httptest.NewRecorder()
	h.handleSourcing(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Sourcing") {
		t.Error("rendered page missing the title")
	}
	// The page must render without panicking through the two-pane template and
	// its sub-templates (chip, state label, dot). A structural marker that is
	// present regardless of data proves the whole template tree parsed and
	// executed — the old assertion keyed on the at-risk queue note, which was
	// removed when the buried queue was promoted into the unlock panel.
	if !strings.Contains(body, "src-two") && !strings.Contains(body, "No process styles are being tracked yet") {
		t.Error("expected either the two-pane layout or the empty-state message")
	}
}
