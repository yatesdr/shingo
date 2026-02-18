package www

import (
	"encoding/json"
	"net/http"
	"strconv"

	"shingocore/store"
)

func (h *Handlers) handlePayloads(w http.ResponseWriter, r *http.Request) {
	payloads, _ := h.engine.DB().ListPayloads()
	types, _ := h.engine.DB().ListPayloadTypes()
	nodes, _ := h.engine.DB().ListNodes()

	data := map[string]any{
		"Page":          "payloads",
		"Payloads":      payloads,
		"PayloadTypes":  types,
		"Nodes":         nodes,
		"Authenticated": h.isAuthenticated(r),
	}
	h.render(w, "payloads.html", data)
}

func (h *Handlers) handlePayloadCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	typeID, err := strconv.ParseInt(r.FormValue("payload_type_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid payload type", http.StatusBadRequest)
		return
	}

	p := &store.Payload{
		PayloadTypeID: typeID,
		Status:        r.FormValue("status"),
		Notes:         r.FormValue("notes"),
	}

	if nodeStr := r.FormValue("node_id"); nodeStr != "" {
		if nid, err := strconv.ParseInt(nodeStr, 10, 64); err == nil {
			p.NodeID = &nid
		}
	}

	if p.Status == "" {
		p.Status = "empty"
	}

	if err := h.engine.DB().CreatePayload(p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/payloads", http.StatusSeeOther)
}

func (h *Handlers) handlePayloadUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	p, err := h.engine.DB().GetPayload(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	typeID, err := strconv.ParseInt(r.FormValue("payload_type_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid payload type", http.StatusBadRequest)
		return
	}

	p.PayloadTypeID = typeID
	p.Status = r.FormValue("status")
	p.Notes = r.FormValue("notes")
	p.NodeID = nil

	if nodeStr := r.FormValue("node_id"); nodeStr != "" {
		if nid, err := strconv.ParseInt(nodeStr, 10, 64); err == nil {
			p.NodeID = &nid
		}
	}

	if err := h.engine.DB().UpdatePayload(p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/payloads", http.StatusSeeOther)
}

func (h *Handlers) handlePayloadDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.engine.DB().DeletePayload(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/payloads", http.StatusSeeOther)
}

func (h *Handlers) apiListPayloads(w http.ResponseWriter, r *http.Request) {
	payloads, err := h.engine.DB().ListPayloads()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, payloads)
}

func (h *Handlers) apiGetPayload(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	p, err := h.engine.DB().GetPayload(id)
	if err != nil {
		h.jsonError(w, "not found", http.StatusNotFound)
		return
	}
	h.jsonOK(w, p)
}

func (h *Handlers) apiListManifest(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	items, err := h.engine.DB().ListManifestItems(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, items)
}

func (h *Handlers) apiCreateManifestItem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PayloadID      int64   `json:"payload_id"`
		PartNumber     string  `json:"part_number"`
		Quantity       float64 `json:"quantity"`
		ProductionDate string  `json:"production_date"`
		LotCode        string  `json:"lot_code"`
		Notes          string  `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}

	m := &store.ManifestItem{
		PayloadID:      req.PayloadID,
		PartNumber:     req.PartNumber,
		Quantity:       req.Quantity,
		ProductionDate: req.ProductionDate,
		LotCode:        req.LotCode,
		Notes:          req.Notes,
	}
	if err := h.engine.DB().CreateManifestItem(m); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, m)
}

func (h *Handlers) apiUpdateManifestItem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID             int64   `json:"id"`
		PartNumber     string  `json:"part_number"`
		Quantity       float64 `json:"quantity"`
		ProductionDate string  `json:"production_date"`
		LotCode        string  `json:"lot_code"`
		Notes          string  `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}

	m := &store.ManifestItem{
		ID:             req.ID,
		PartNumber:     req.PartNumber,
		Quantity:       req.Quantity,
		ProductionDate: req.ProductionDate,
		LotCode:        req.LotCode,
		Notes:          req.Notes,
	}
	if err := h.engine.DB().UpdateManifestItem(m); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeleteManifestItem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if err := h.engine.DB().DeleteManifestItem(req.ID); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, map[string]string{"status": "ok"})
}
