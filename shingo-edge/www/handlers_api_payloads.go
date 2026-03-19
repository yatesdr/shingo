package www

import (
	"encoding/json"
	"net/http"

	"shingoedge/engine"
	"shingoedge/store"
)

// --- Payloads Admin ---

func (h *Handlers) apiListPayloads(w http.ResponseWriter, r *http.Request) {
	payloads, err := h.engine.DB().ListPayloads()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, payloads)
}

func (h *Handlers) apiListPayloadsByJobStyle(w http.ResponseWriter, r *http.Request) {
	jobStyleID, err := parseID(r, "jobStyleID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid job style ID")
		return
	}
	payloads, err := h.engine.DB().ListPayloadsByJobStyle(jobStyleID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, payloads)
}

func (h *Handlers) apiCreatePayload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JobStyleID          int64  `json:"job_style_id"`
		Location            string `json:"location"`
		StagingNode         string `json:"staging_node"`
		Description         string `json:"description"`
		PayloadCode         string `json:"payload_code"`
		Role                string `json:"role"`
		AutoReorder         bool   `json:"auto_reorder"`
		ReorderPoint        int    `json:"reorder_point"`
		CycleMode           string `json:"cycle_mode"`
		StagingNodeGroup    string `json:"staging_node_group"`
		StagingNode2        string `json:"staging_node_2"`
		StagingNode2Group   string `json:"staging_node_2_group"`
		FullPickupNode      string `json:"full_pickup_node"`
		FullPickupNodeGroup string `json:"full_pickup_node_group"`
		OutgoingNode       string `json:"outgoing_node"`
		OutgoingNodeGroup  string `json:"outgoing_node_group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := h.engine.DB().CreatePayload(store.PayloadInput{
		JobStyleID:          req.JobStyleID,
		Location:            req.Location,
		StagingNode:         req.StagingNode,
		Description:         req.Description,
		PayloadCode:         req.PayloadCode,
		Role:                req.Role,
		AutoReorder:         req.AutoReorder,
		ReorderPoint:        req.ReorderPoint,
		CycleMode:           req.CycleMode,
		StagingNodeGroup:    req.StagingNodeGroup,
		StagingNode2:        req.StagingNode2,
		StagingNode2Group:   req.StagingNode2Group,
		FullPickupNode:      req.FullPickupNode,
		FullPickupNodeGroup: req.FullPickupNodeGroup,
		OutgoingNode:       req.OutgoingNode,
		OutgoingNodeGroup:  req.OutgoingNodeGroup,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdatePayload(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Location            string `json:"location"`
		StagingNode         string `json:"staging_node"`
		Description         string `json:"description"`
		PayloadCode         string `json:"payload_code"`
		Role                string `json:"role"`
		AutoReorder         bool   `json:"auto_reorder"`
		ReorderPoint        int    `json:"reorder_point"`
		CycleMode           string `json:"cycle_mode"`
		StagingNodeGroup    string `json:"staging_node_group"`
		StagingNode2        string `json:"staging_node_2"`
		StagingNode2Group   string `json:"staging_node_2_group"`
		FullPickupNode      string `json:"full_pickup_node"`
		FullPickupNodeGroup string `json:"full_pickup_node_group"`
		OutgoingNode       string `json:"outgoing_node"`
		OutgoingNodeGroup  string `json:"outgoing_node_group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.DB().UpdatePayload(id, store.PayloadInput{
		Location:            req.Location,
		StagingNode:         req.StagingNode,
		Description:         req.Description,
		PayloadCode:         req.PayloadCode,
		Role:                req.Role,
		AutoReorder:         req.AutoReorder,
		ReorderPoint:        req.ReorderPoint,
		CycleMode:           req.CycleMode,
		StagingNodeGroup:    req.StagingNodeGroup,
		StagingNode2:        req.StagingNode2,
		StagingNode2Group:   req.StagingNode2Group,
		FullPickupNode:      req.FullPickupNode,
		FullPickupNodeGroup: req.FullPickupNodeGroup,
		OutgoingNode:       req.OutgoingNode,
		OutgoingNodeGroup:  req.OutgoingNodeGroup,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeletePayload(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	if err := h.engine.DB().DeletePayload(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiPayloadCount(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		PieceCount float64 `json:"piece_count"`
		Reset      bool    `json:"reset"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	p, err := h.engine.DB().GetPayload(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "payload not found")
		return
	}

	// Look up UOP capacity from catalog (single source of truth)
	bp, err := h.engine.DB().GetPayloadCatalogByCode(p.PayloadCode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "payload catalog entry not found for code: "+p.PayloadCode)
		return
	}

	var prodUnits int
	var status string

	if req.Reset {
		if err := h.engine.DB().ResetPayload(id, bp.UOPCapacity); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		prodUnits = bp.UOPCapacity
		status = "active"
	} else {
		// Multiplier is always 1 — piece count = production units
		prodUnits = int(req.PieceCount)
		if prodUnits < 0 {
			prodUnits = 0
		}

		status = p.Status
		if prodUnits == 0 {
			status = "empty"
		} else if prodUnits <= p.ReorderPoint {
			status = "replenishing"
		} else {
			status = "active"
		}

		if err := h.engine.DB().UpdatePayloadRemaining(id, prodUnits, status); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	h.engine.Events.Emit(engine.Event{
		Type: engine.EventPayloadUpdated,
		Payload: engine.PayloadUpdatedEvent{
			PayloadID:    id,
			JobStyleID:   p.JobStyleID,
			Location:     p.Location,
			OldRemaining: p.Remaining,
			NewRemaining: prodUnits,
			Status:       status,
		},
	})

	writeJSON(w, map[string]interface{}{
		"status":           "ok",
		"production_units": prodUnits,
		"payload_status":   status,
	})
}

// --- Public Payload Read (for operator displays) ---

func (h *Handlers) apiListPayloadsByLinePublic(w http.ResponseWriter, r *http.Request) {
	lineID, err := parseID(r, "lineID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid line ID")
		return
	}
	line, err := h.engine.DB().GetProductionLine(lineID)
	if err != nil {
		writeError(w, http.StatusNotFound, "line not found")
		return
	}
	if line.ActiveJobStyleID == nil {
		writeJSON(w, []interface{}{})
		return
	}
	payloads, err := h.engine.DB().ListPayloadsByJobStyle(*line.ActiveJobStyleID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, payloads)
}
