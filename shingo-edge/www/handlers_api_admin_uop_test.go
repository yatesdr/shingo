package www

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
)

// newBackfillRouter mounts apiBackfillBuckets under /api/admin/uop/backfill
// inside the admin auth group, matching the production layout.
func newBackfillRouter(t *testing.T) (*Handlers, *chi.Mux, *stubEngine) {
	t.Helper()
	h, r := newTestHandlers(t)
	stub := h.engine.(*stubEngine)

	r.Route("/api", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(h.adminMiddleware)
			r.Post("/admin/uop/backfill", h.apiBackfillBuckets)
		})
	})

	return h, r, stub
}

// TestApiBackfillBuckets_RequiresAuth pins the admin gating: an
// unauthenticated POST gets bounced to /login (303) rather than firing
// the backfill. Without this, any client could trigger seed-delta
// emission against Core — a non-destructive but disruptive op (would
// duplicate at-least-once via dedup but spike audit volume).
func TestApiBackfillBuckets_RequiresAuth(t *testing.T) {
	_, router, _ := newBackfillRouter(t)
	resp := doRequest(t, router, "POST", "/api/admin/uop/backfill", nil, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("unauthenticated POST: status = %d, want 303 (admin gate must redirect)", resp.StatusCode)
	}
}

// TestApiBackfillBuckets_DefaultProbesNeeded pins the no-force path:
// the handler probes BucketBackfillNeeded first and returns
// {skipped: true, emitted: 0} when Core is already populated. Avoids
// burning SequenceID slots on idempotent re-runs.
func TestApiBackfillBuckets_DefaultProbesNeeded(t *testing.T) {
	h, router, stub := newBackfillRouter(t)
	stub.backfillNeeded = false // Core already populated → skip
	cookie := authCookie(t, h)

	resp := doRequest(t, router, "POST", "/api/admin/uop/backfill", nil, cookie)
	assertStatus(t, resp, http.StatusOK)

	var body map[string]any
	decodeJSON(t, resp, &body)
	if got, _ := body["skipped"].(bool); !got {
		t.Errorf("response.skipped = %v, want true (probe said not needed)", body["skipped"])
	}
	if got, _ := body["emitted"].(float64); got != 0 {
		t.Errorf("response.emitted = %v, want 0", body["emitted"])
	}
	if stub.backfillNeededCalls != 1 {
		t.Errorf("BucketBackfillNeeded calls = %d, want 1 (default path must probe)",
			stub.backfillNeededCalls)
	}
	if stub.backfillCalls != 0 {
		t.Errorf("BackfillBucketsForStation calls = %d, want 0 (probe said skip)",
			stub.backfillCalls)
	}
}

// TestApiBackfillBuckets_DefaultRunsIfNeeded pins the no-force happy
// path: probe says needed → handler fires the backfill and returns
// {emitted: N, forced: false}.
func TestApiBackfillBuckets_DefaultRunsIfNeeded(t *testing.T) {
	h, router, stub := newBackfillRouter(t)
	stub.backfillNeeded = true
	stub.backfillEmitted = 7
	cookie := authCookie(t, h)

	resp := doRequest(t, router, "POST", "/api/admin/uop/backfill", nil, cookie)
	assertStatus(t, resp, http.StatusOK)

	var body map[string]any
	decodeJSON(t, resp, &body)
	if got, _ := body["skipped"].(bool); got {
		t.Errorf("response.skipped = true, want false (probe said needed)")
	}
	if got, _ := body["emitted"].(float64); got != 7 {
		t.Errorf("response.emitted = %v, want 7", body["emitted"])
	}
	if got, _ := body["forced"].(bool); got {
		t.Errorf("response.forced = true, want false (no force query param)")
	}
}

// TestApiBackfillBuckets_ForceTrueSkipsProbe pins the doc-mandated
// force=true semantic: when ?force=true, the handler skips the
// BucketBackfillNeeded probe entirely and fires the backfill no matter
// what Core's state is. Useful when an operator just wiped
// lineside_buckets at Core and wants to re-seed without waiting for
// Core's next snapshot to confirm the empty state.
func TestApiBackfillBuckets_ForceTrueSkipsProbe(t *testing.T) {
	h, router, stub := newBackfillRouter(t)
	stub.backfillNeeded = false // probe would say "skip"
	stub.backfillEmitted = 4
	cookie := authCookie(t, h)

	resp := doRequest(t, router, "POST", "/api/admin/uop/backfill?force=true", nil, cookie)
	assertStatus(t, resp, http.StatusOK)

	var body map[string]any
	decodeJSON(t, resp, &body)
	if got, _ := body["forced"].(bool); !got {
		t.Errorf("response.forced = false, want true")
	}
	if got, _ := body["emitted"].(float64); got != 4 {
		t.Errorf("response.emitted = %v, want 4 (force ran the backfill despite probe)", body["emitted"])
	}
	if stub.backfillNeededCalls != 0 {
		t.Errorf("BucketBackfillNeeded calls = %d, want 0 (force=true must skip probe)",
			stub.backfillNeededCalls)
	}
	if stub.backfillCalls != 1 {
		t.Errorf("BackfillBucketsForStation calls = %d, want 1", stub.backfillCalls)
	}
	if !stub.backfillForce {
		t.Errorf("BackfillBucketsForStation force arg = false, want true (synchronous flush expected)")
	}
}
