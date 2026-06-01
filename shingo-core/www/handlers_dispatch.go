// handlers_dispatch.go — HTTP handlers for the bin-transit-state UI
// surfaces:
//   - capacity preview (Phase 4d)
//   - transit anomaly listing + recovery (Phase 5)
//
// Each is a thin wrapper that does parameter validation + permission
// gate (where applicable) and delegates to the dispatch / service
// layer for the actual work. Routes registered in router.go.

package www

import (
	"net/http"
)

// apiPreviewDropoffCapacity returns whether a delivery node would
// accept a fresh dispatch right now. Used by the operator UI to show
// "this would queue, blocking on Node X" inline before submitting an
// order.
//
//	GET /api/dispatch/preview-capacity?node=NAME
//	→ {"blocked": true, "reason": "destination LINE_01 occupied (1 bin(s))", "delivery_node": "LINE_01"}
func (h *Handlers) apiPreviewDropoffCapacity(w http.ResponseWriter, r *http.Request) {
	deliveryNode := r.URL.Query().Get("node")
	if deliveryNode == "" {
		h.jsonError(w, "node parameter is required", http.StatusBadRequest)
		return
	}
	preview := h.engine.Dispatcher().PreviewDropoffCapacity(deliveryNode)
	h.jsonOK(w, preview)
}

// apiListTransitAnomalies returns bins parked at the synthetic
// _TRANSIT node with no live order claim — the binary anomaly signal
// from bin-transit-state Phase 5. Operators use this to find bins
// that need physical recovery (robot fault between pickup and
// delivery left the bin in flight, then the order failed).
//
//	GET /api/dispatch/anomalies
//	→ [{...bin row...}, ...]
func (h *Handlers) apiListTransitAnomalies(w http.ResponseWriter, r *http.Request) {
	bins, err := h.engine.BinService().ListAnomalies()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, bins)
}

// apiClearTransitAnomaly is the operator's recovery action: "I found
// this bin physically; here's the real node it's sitting at." Moves
// the bin out of _TRANSIT, clears anomaly_at, records the action in
// recovery_actions for post-incident review.
//
//	POST /api/dispatch/clear-anomaly
//	body: {"bin_id": 123, "to_node_id": 45}
func (h *Handlers) apiClearTransitAnomaly(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BinID    int64 `json:"bin_id"`
		ToNodeID int64 `json:"to_node_id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.BinID == 0 || req.ToNodeID == 0 {
		h.jsonError(w, "bin_id and to_node_id are required", http.StatusBadRequest)
		return
	}
	actor := h.getUsername(r)
	if err := h.engine.BinService().RecoverTransitAnomaly(req.BinID, req.ToNodeID, actor); err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.jsonSuccess(w)
}
