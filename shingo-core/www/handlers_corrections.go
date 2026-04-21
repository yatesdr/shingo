package www

import (
	"net/http"

	"shingocore/engine"
)

func (h *Handlers) apiCreateCorrection(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CorrectionType string `json:"correction_type"`
		NodeID         int64  `json:"node_id"`
		BinID          int64  `json:"bin_id"`
		CatID          string `json:"cat_id"`
		Description    string `json:"description"`
		Quantity       int64  `json:"quantity"`
		Reason         string `json:"reason"`
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
		BinID:          req.BinID,
		CatID:          req.CatID,
		Description:    req.Description,
		Quantity:       req.Quantity,
		Reason:         req.Reason,
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
		BinID  int64  `json:"bin_id"`
		NodeID int64  `json:"node_id"`
		Reason string `json:"reason"`
		Items  []struct {
			CatID    string `json:"cat_id"`
			Quantity int64  `json:"quantity"`
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
			CatID:    it.CatID,
			Quantity: it.Quantity,
		}
	}

	err := h.engine.ApplyBatchCorrection(engine.BatchCorrectionRequest{
		BinID:  req.BinID,
		NodeID: req.NodeID,
		Reason: req.Reason,
		Actor:  actor,
		Items:  items,
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
	corrections, err := h.engine.NodeService().ListCorrectionsByNode(nodeID, 20)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, corrections)
}
