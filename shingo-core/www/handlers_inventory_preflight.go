// HTTP boundary for the pre-flight inventory check. Edge POSTs the
// to-style's required payload list; Core responds with the per-payload
// availability and the missing subset so the operator UI can refuse
// the changeover with a specific diagnostic.

package www

import (
	"encoding/json"
	"net/http"
)

// apiInventoryPreflight wraps InventoryService.PreflightAvailability for the
// HTTP boundary.
//
// Request:  {"station": "...", "payloads": ["PN-A", "PN-B", ...]}
// Response: PreflightResult JSON (see service/inventory_preflight.go).
func (h *Handlers) apiInventoryPreflight(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Station  string   `json:"station"`
		Payloads []string `json:"payloads"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.jsonError(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}
	result, err := h.engine.InventoryService().PreflightAvailability(r.Context(), req.Station, req.Payloads)
	if err != nil {
		h.jsonError(w, "preflight: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, result)
}
