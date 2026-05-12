// HTTP boundary for the system-wide bin count endpoint. Edge POSTs a
// payload list; Core responds with the per-payload bin count using the
// "in the kanban loop" inclusion policy (see service/inventory_system_count.go).
//
// This is intentionally separate from /api/inventory/preflight, which
// answers a different question ("is this bin pickable as a source right
// now?"). Conflating the two was the 2026-05-11 SNF2 plant incident.

package www

import (
	"encoding/json"
	"net/http"
)

// apiInventorySystemCount wraps InventoryService.SystemBinCount for the
// HTTP boundary.
//
// Request:  {"payloads": ["PN-A", "PN-B", ...]}
// Response: SystemBinCountResult JSON (see service/inventory_system_count.go).
func (h *Handlers) apiInventorySystemCount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Payloads []string `json:"payloads"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.jsonError(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}
	result, err := h.engine.InventoryService().SystemBinCount(r.Context(), req.Payloads)
	if err != nil {
		h.jsonError(w, "system-count: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, result)
}
