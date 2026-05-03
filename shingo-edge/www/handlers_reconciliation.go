package www

import (
	"net/http"
)

// apiUOPReconciliation returns the engine's reconciler metrics as JSON.
// Item 9 of the bin-as-truth refactor — exposes the counters the
// reconciler maintains (passes, bins seen / healed / skipped,
// buckets seen / drifted / healed / skipped, flush_failures, last_pass)
// so dashboards and on-call can trend reconciliation behavior over
// time. Sustained drift counter growth without matching heal counter
// growth signals a reconciler bug; flush_failures growth signals the
// outbox is wedged.
//
// Public endpoint — read-only counters with no operational secrets.
// Mounted at GET /api/reconciliation/uop in router.go.
func (s *Handlers) apiUOPReconciliation(w http.ResponseWriter, r *http.Request) {
	metrics := s.engine.ReconcilerMetrics()
	writeJSON(w, metrics)
}
