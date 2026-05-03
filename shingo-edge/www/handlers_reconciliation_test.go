package www

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"shingoedge/engine"
)

// TestApiUOPReconciliation_ReturnsMetricsJSON pins Item 9's HTTP shape:
// the endpoint marshals UOPReconcilerMetrics straight back to the
// caller. Dashboards depend on the JSON field names; this test pins
// every snake_case key to catch a future struct rename that would
// silently break observability.
func TestApiUOPReconciliation_ReturnsMetricsJSON(t *testing.T) {
	h, router := newTestHandlers(t)
	stub := h.engine.(*stubEngine)
	stub.reconcilerMetrics = engine.UOPReconcilerMetrics{
		Passes:         42,
		BinsSeen:       17,
		BinsHealed:     5,
		BinsSkipped:    2,
		BucketsSeen:    11,
		BucketsDrifted: 1,
		BucketsHealed:  1,
		BucketsSkipped: 0,
		FlushFailures:  3,
	}

	router.Route("/api", func(r chi.Router) {
		r.Get("/reconciliation/uop", h.apiUOPReconciliation)
	})

	resp := doRequest(t, router, "GET", "/api/reconciliation/uop", nil, nil)
	assertStatus(t, resp, http.StatusOK)

	var got map[string]any
	decodeJSON(t, resp, &got)

	wants := map[string]float64{
		"passes":          42,
		"bins_seen":       17,
		"bins_healed":     5,
		"bins_skipped":    2,
		"buckets_seen":    11,
		"buckets_drifted": 1,
		"buckets_healed":  1,
		"buckets_skipped": 0,
		"flush_failures":  3,
	}
	for k, want := range wants {
		v, ok := got[k]
		if !ok {
			t.Errorf("response missing key %q", k)
			continue
		}
		f, ok := v.(float64)
		if !ok {
			t.Errorf("response[%q] = %v (%T), want number", k, v, v)
			continue
		}
		if f != want {
			t.Errorf("response[%q] = %v, want %v", k, f, want)
		}
	}

	// last_pass should be present even when zero — dashboards key on it.
	if _, ok := got["last_pass"]; !ok {
		t.Errorf("response missing last_pass key")
	}

	// Sanity: response is JSON.
	body := resp.Header.Get("Content-Type")
	if got := body; got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	_ = json.Marshal // anchor the json import; used by the assertion path indirectly via decodeJSON.
}
