package www

import (
	"net/http"

	"shingocore/engine"
)

func (h *Handlers) apiCreateCorrection(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CorrectionType string  `json:"correction_type"`
		NodeID         int64   `json:"node_id"`
		InstanceID     int64   `json:"instance_id"`
		CatID          string  `json:"cat_id"`
		Description    string  `json:"description"`
		Quantity       float64 `json:"quantity"`
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
		InstanceID:     req.InstanceID,
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
