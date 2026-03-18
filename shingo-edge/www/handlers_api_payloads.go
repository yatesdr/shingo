package www

import (
	"encoding/json"
	"math"
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
		JobStyleID      int64   `json:"job_style_id"`
		Location        string  `json:"location"`
		StagingNode     string  `json:"staging_node"`
		Description     string  `json:"description"`
		PayloadCode     string  `json:"payload_code"`
		Manifest        string  `json:"manifest"`
		Multiplier      float64 `json:"multiplier"`
		ProductionUnits int     `json:"production_units"`
		Remaining       int     `json:"remaining"`
		ReorderPoint    int     `json:"reorder_point"`
		ReorderQty      int     `json:"reorder_qty"`
		RetrieveEmpty     bool    `json:"retrieve_empty"`
		Role              string  `json:"role"`
		AutoRemoveEmpties bool    `json:"auto_remove_empties"`
		AutoOrderEmpties  bool    `json:"auto_order_empties"`
		// Hot-swap configuration
		HotSwap             string `json:"hot_swap"`
		StagingNodeGroup    string `json:"staging_node_group"`
		StagingNode2        string `json:"staging_node_2"`
		StagingNode2Group   string `json:"staging_node_2_group"`
		FullPickupNode      string `json:"full_pickup_node"`
		FullPickupNodeGroup string `json:"full_pickup_node_group"`
		EmptyDropNode       string `json:"empty_drop_node"`
		EmptyDropNodeGroup  string `json:"empty_drop_node_group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Manifest == "" {
		req.Manifest = "{}"
	}
	if req.Multiplier <= 0 {
		req.Multiplier = 1
	}
	if req.ReorderQty <= 0 {
		req.ReorderQty = 1
	}
	id, err := h.engine.DB().CreatePayload(store.PayloadInput{
		JobStyleID:          req.JobStyleID,
		Location:            req.Location,
		StagingNode:         req.StagingNode,
		Description:         req.Description,
		Manifest:            req.Manifest,
		Multiplier:          req.Multiplier,
		ProductionUnits:     req.ProductionUnits,
		Remaining:           req.Remaining,
		ReorderPoint:        req.ReorderPoint,
		ReorderQty:          req.ReorderQty,
		RetrieveEmpty:       req.RetrieveEmpty,
		PayloadCode:         req.PayloadCode,
		Role:                req.Role,
		AutoRemoveEmpties:   req.AutoRemoveEmpties,
		AutoOrderEmpties:    req.AutoOrderEmpties,
		HotSwap:             req.HotSwap,
		StagingNodeGroup:    req.StagingNodeGroup,
		StagingNode2:        req.StagingNode2,
		StagingNode2Group:   req.StagingNode2Group,
		FullPickupNode:      req.FullPickupNode,
		FullPickupNodeGroup: req.FullPickupNodeGroup,
		EmptyDropNode:       req.EmptyDropNode,
		EmptyDropNodeGroup:  req.EmptyDropNodeGroup,
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
		Location        string  `json:"location"`
		StagingNode     string  `json:"staging_node"`
		Description     string  `json:"description"`
		PayloadCode     string  `json:"payload_code"`
		Manifest        string  `json:"manifest"`
		Multiplier      float64 `json:"multiplier"`
		ProductionUnits int     `json:"production_units"`
		Remaining       int     `json:"remaining"`
		ReorderPoint    int     `json:"reorder_point"`
		ReorderQty      int     `json:"reorder_qty"`
		RetrieveEmpty     bool    `json:"retrieve_empty"`
		Role              string  `json:"role"`
		AutoRemoveEmpties bool    `json:"auto_remove_empties"`
		AutoOrderEmpties  bool    `json:"auto_order_empties"`
		// Hot-swap configuration
		HotSwap             string `json:"hot_swap"`
		StagingNodeGroup    string `json:"staging_node_group"`
		StagingNode2        string `json:"staging_node_2"`
		StagingNode2Group   string `json:"staging_node_2_group"`
		FullPickupNode      string `json:"full_pickup_node"`
		FullPickupNodeGroup string `json:"full_pickup_node_group"`
		EmptyDropNode       string `json:"empty_drop_node"`
		EmptyDropNodeGroup  string `json:"empty_drop_node_group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.DB().UpdatePayload(id, store.PayloadInput{
		Location:            req.Location,
		StagingNode:         req.StagingNode,
		Description:         req.Description,
		Manifest:            req.Manifest,
		Multiplier:          req.Multiplier,
		ProductionUnits:     req.ProductionUnits,
		Remaining:           req.Remaining,
		ReorderPoint:        req.ReorderPoint,
		ReorderQty:          req.ReorderQty,
		RetrieveEmpty:       req.RetrieveEmpty,
		PayloadCode:         req.PayloadCode,
		Role:                req.Role,
		AutoRemoveEmpties:   req.AutoRemoveEmpties,
		AutoOrderEmpties:    req.AutoOrderEmpties,
		HotSwap:             req.HotSwap,
		StagingNodeGroup:    req.StagingNodeGroup,
		StagingNode2:        req.StagingNode2,
		StagingNode2Group:   req.StagingNode2Group,
		FullPickupNode:      req.FullPickupNode,
		FullPickupNodeGroup: req.FullPickupNodeGroup,
		EmptyDropNode:       req.EmptyDropNode,
		EmptyDropNodeGroup:  req.EmptyDropNodeGroup,
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

	var prodUnits int
	var status string

	if req.Reset {
		if err := h.engine.DB().ResetPayload(id, p.ProductionUnits); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		prodUnits = p.ProductionUnits
		status = "active"
	} else {
		prodUnits = int(math.Round(req.PieceCount / p.Multiplier))
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
