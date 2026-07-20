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
	if !strings.Contains(body, "at-risk tier is disabled") {
		t.Error("expected the yellow-dark queue note in the default render")
	}
}
