package www

import (
	"net/http"

	"shingocore/engine"
)

func (h *Handlers) apiCreateCorrection(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CorrectionType string  `json:"correction_type"`
		NodeID         int64   `json:"node_id"`
		PayloadID     int64   `json:"payload_id"`
		CatID          string  `json:"cat_id"`
		Description    string  `json:"description"`
		Quantity       int64   `json:"quantity"`
		Reason         string  `json:"reason"`
		ManifestItemID int64   `json:"manifest_item_id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	actor := h.getUsername(r)
	if actor == "" {
		actor = "admin"
	}

	id, err := h.engine.ApplyCorrection(engine.ApplyCorrectionRequest{
		CorrectionType: req.CorrectionType,
		NodeID:         req.NodeID,
		PayloadID:     req.PayloadID,
		CatID:          req.CatID,
		Description:    req.Description,
		Quantity:       req.Quantity,
		Reason:         req.Reason,
		ManifestItemID: req.ManifestItemID,
		Actor:          actor,
	})
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.jsonOK(w, map[string]any{"id": id})
}

func (h *Handlers) apiApplyBatchCorrection(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PayloadID int64 `json:"payload_id"`
		NodeID    int64 `json:"node_id"`
		Reason    string `json:"reason"`
		Items     []struct {
			ID       int64   `json:"id"`
			CatID    string  `json:"cat_id"`
			Quantity int64   `json:"quantity"`
		} `json:"items"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		h.jsonError(w, "reason is required", http.StatusBadRequest)
		return
	}

	actor := h.getUsername(r)
	if actor == "" {
		actor = "admin"
	}

	items := make([]engine.BatchCorrectionItem, len(req.Items))
	for i, it := range req.Items {
		items[i] = engine.BatchCorrectionItem{
			ID:       it.ID,
			CatID:    it.CatID,
			Quantity: it.Quantity,
		}
	}

	err := h.engine.ApplyBatchCorrection(engine.BatchCorrectionRequest{
		PayloadID: req.PayloadID,
		NodeID:    req.NodeID,
		Reason:    req.Reason,
		Actor:     actor,
		Items:     items,
	})
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.jsonOK(w, map[string]any{"ok": true})
}

func (h *Handlers) apiListNodeCorrections(w http.ResponseWriter, r *http.Request) {
	nodeID, ok := h.parseIDParam(w, r, "node_id")
	if !ok {
		return
	}
	corrections, err := h.engine.DB().ListCorrectionsByNode(nodeID, 20)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, corrections)
}
