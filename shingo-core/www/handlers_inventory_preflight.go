// HTTP boundary for the pre-flight inventory check. Edge POSTs the
// to-style required payload list; Core responds with the per-payload
// availability and the missing subset so the operator UI can refuse
// the changeover with a specific diagnostic.

package www

import (
	"encoding/json"
	"net/http"

	"shingo/protocol"
)

// apiInventoryPreflight wraps InventoryService.PreflightAvailability for the
// HTTP boundary.
//
// Request:  protocol.PreflightRequest
// Response: protocol.PreflightResponse (missing, absent, available)

func (h *Handlers) apiInventoryPreflight(w http.ResponseWriter, r *http.Request) {
	var req protocol.PreflightRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.jsonError(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}
	result, err := h.engine.InventoryService().PreflightAvailability(r.Context(), req.Station, req.Payloads)
	if err != nil {
		h.jsonError(w, "preflight: "+err.Error(), http.StatusInternalServerError)
		return
	}
	resp := protocol.PreflightResponse{Missing: result.Missing, Absent: result.Absent}
	for _, a := range result.Available {
		resp.Available = append(resp.Available, protocol.PreflightAvailability{
			PayloadCode: a.PayloadCode,
			BinCount:    a.BinCount,
		})
	}
	h.jsonOK(w, resp)
}
