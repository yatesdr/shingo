package www

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// Item 10 audit UI backend. Three read endpoints surface bin_uop_audit
// for the per-bin timeline, per-operator activity, and per-station
// override-pattern views. Frontend (templates/audit.html, pages/audit.js,
// nav link) is deferred — defining the JSON shape here lets the UI
// land as a follow-up without touching backend code.
//
// All three accept ?limit and ?offset for paging. Limit clamps to
// [1, 500]; defaults to 100. Offset defaults to 0. Per-handler
// helpers parse the params so the handler bodies stay short.

func (h *Handlers) parseAuditPaging(r *http.Request) (limit, offset int) {
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			offset = n
		}
	}
	return limit, offset
}

// apiAuditBinTimeline returns the audit timeline for one bin.
//
//	GET /api/audit/bin/{id}[?limit=N&offset=M]
//
// Per-bin view shows before_uop, suggested_uop (in metadata for
// override rows), after_uop side-by-side — the visual diff of
// operator-vs-system disagreement.
func (h *Handlers) apiAuditBinTimeline(w http.ResponseWriter, r *http.Request) {
	binID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || binID <= 0 {
		h.jsonError(w, "bad bin id", http.StatusBadRequest)
		return
	}
	limit, offset := h.parseAuditPaging(r)
	rows, err := h.engine.AuditService().ListBinUOPByBin(binID, limit, offset)
	if err != nil {
		h.jsonError(w, "list audit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, rows)
}

// apiAuditOperatorActivity returns activity by an operator (or system
// actor like a station id).
//
//	GET /api/audit/operator/{name}[?limit=N&offset=M]
//
// "name" is the actor field — anything from a username to a station
// id like "ALN_001". Exact match.
func (h *Handlers) apiAuditOperatorActivity(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		h.jsonError(w, "missing operator name", http.StatusBadRequest)
		return
	}
	limit, offset := h.parseAuditPaging(r)
	rows, err := h.engine.AuditService().ListBinUOPByOperator(name, limit, offset)
	if err != nil {
		h.jsonError(w, "list audit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, rows)
}

// apiAuditStationOverrides returns recent override rows for a station —
// only the operator_override_pull_parts and
// operator_override_release_partial op tags. SCO and management use
// this to spot mislabelled bins, upstream overfill, and miscount drift.
//
//	GET /api/audit/station/{station}[?limit=N&offset=M]
func (h *Handlers) apiAuditStationOverrides(w http.ResponseWriter, r *http.Request) {
	station := chi.URLParam(r, "station")
	if station == "" {
		h.jsonError(w, "missing station id", http.StatusBadRequest)
		return
	}
	limit, offset := h.parseAuditPaging(r)
	rows, err := h.engine.AuditService().ListBinUOPOverridesByStation(station, limit, offset)
	if err != nil {
		h.jsonError(w, "list audit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, rows)
}

// apiAuditDiscrepancies returns the discrepancy ledger: bin_uop_audit
// rows where the tracked count diverged from reality — dropped stale ticks,
// negative remaining, and release-empties that still carried counted parts.
// A read-only view over bin_uop_audit, not a separate ledger table.
//
//	GET /api/audit/discrepancies[?limit=N&offset=M]
func (h *Handlers) apiAuditDiscrepancies(w http.ResponseWriter, r *http.Request) {
	limit, offset := h.parseAuditPaging(r)
	rows, err := h.engine.AuditService().ListBinUOPDiscrepancies(limit, offset)
	if err != nil {
		h.jsonError(w, "list discrepancies: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, rows)
}
