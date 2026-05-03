package www

import (
	"net/http"
)

// apiBackfillBuckets is the admin entry point for re-running the
// Item 3 bucket backfill. The startup auto-fire path covers the
// fresh-Core case; this endpoint handles re-runs (e.g. after a Core
// restore, or when the auto-fire path was suppressed by a transient
// HTTP failure at boot).
//
//	POST /api/admin/uop/backfill[?force=true]
//
// Without force, the handler probes BucketBackfillNeeded first and
// no-ops if Core already has data — idempotent re-runs are safe.
// With force=true the probe is skipped and the seed deltas ship
// regardless. Useful when Core's state is known-stale (e.g. operator
// just wiped lineside_buckets and wants to re-seed).
//
// Response shape: {"emitted": <int>, "forced": <bool>, "skipped": <bool>}
//
// Gated by adminMiddleware in router.go alongside the other config
// admin endpoints.
func (h *Handlers) apiBackfillBuckets(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "true"

	if !force {
		needed, err := h.orchestration.BucketBackfillNeeded()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !needed {
			writeJSON(w, map[string]any{
				"emitted": 0,
				"forced":  false,
				"skipped": true,
			})
			return
		}
	}

	emitted, err := h.orchestration.BackfillBucketsForStation(true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"emitted": emitted,
		"forced":  force,
		"skipped": false,
	})
}
