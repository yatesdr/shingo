package www

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"shingocore/store"
)

func (h *Handlers) apiListBlueprints(w http.ResponseWriter, r *http.Request) {
	blueprints, err := h.engine.DB().ListBlueprints()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, blueprints)
}

func (h *Handlers) handleBlueprints(w http.ResponseWriter, r *http.Request) {
	blueprints, _ := h.engine.DB().ListBlueprints()

	// Build compatible nodes map: blueprint_id -> [node names]
	compatNodes := make(map[int64][]string)
	for _, bp := range blueprints {
		nodeList, _ := h.engine.DB().ListNodesForBlueprint(bp.ID)
		for _, n := range nodeList {
			compatNodes[bp.ID] = append(compatNodes[bp.ID], n.Name)
		}
	}

	binTypes, _ := h.engine.DB().ListBinTypes()

	// Build blueprint -> bin type codes map
	bpBinTypes := make(map[int64][]string)
	for _, bp := range blueprints {
		btList, _ := h.engine.DB().ListBinTypesForBlueprint(bp.ID)
		for _, bt := range btList {
			bpBinTypes[bp.ID] = append(bpBinTypes[bp.ID], bt.Code)
		}
	}

	data := map[string]any{
		"Page":        "blueprints",
		"Blueprints":  blueprints,
		"BinTypes":    binTypes,
		"CompatNodes": compatNodes,
		"BPBinTypes":  bpBinTypes,
	}
	h.render(w, r, "blueprints.html", data)
}

func (h *Handlers) handlePayloadCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	blueprintID, err := strconv.ParseInt(r.FormValue("blueprint_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid blueprint", http.StatusBadRequest)
		return
	}

	p := &store.Payload{
		BlueprintID: blueprintID,
		Notes:       r.FormValue("notes"),
	}

	if binStr := r.FormValue("bin_id"); binStr != "" {
		if bid, err := strconv.ParseInt(binStr, 10, 64); err == nil {
			p.BinID = &bid
		}
	}

	// Set UOP from blueprint capacity
	if bp, err := h.engine.DB().GetBlueprint(blueprintID); err == nil && bp.UOPCapacity > 0 {
		p.UOPRemaining = bp.UOPCapacity
	}

	// Mark confirmed if requested
	if r.FormValue("manifest_confirmed") == "1" {
		p.ManifestConfirmed = true
		now := time.Now()
		p.LoadedAt = &now
	}

	if err := h.engine.DB().CreatePayloadWithManifest(p); err != nil {
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

	blueprintID, err := strconv.ParseInt(r.FormValue("blueprint_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid blueprint", http.StatusBadRequest)
		return
	}

	p.BlueprintID = blueprintID
	p.Notes = r.FormValue("notes")
	p.BinID = nil

	if binStr := r.FormValue("bin_id"); binStr != "" {
		if bid, err := strconv.ParseInt(binStr, 10, 64); err == nil {
			p.BinID = &bid
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
		Quantity       int64   `json:"quantity"`
		ProductionDate string  `json:"production_date"`
		LotCode        string  `json:"lot_code"`
		Notes          string  `json:"notes"`
	}
	if !h.parseJSON(w, r, &req) {
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
		Quantity       int64   `json:"quantity"`
		ProductionDate string  `json:"production_date"`
		LotCode        string  `json:"lot_code"`
		Notes          string  `json:"notes"`
	}
	if !h.parseJSON(w, r, &req) {
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
	h.jsonSuccess(w)
}

func (h *Handlers) apiDeleteManifestItem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if err := h.engine.DB().DeleteManifestItem(req.ID); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiConfirmManifest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	p, err := h.engine.DB().GetPayload(req.ID)
	if err != nil {
		h.jsonError(w, "payload not found", http.StatusNotFound)
		return
	}

	p.ManifestConfirmed = true
	now := time.Now()
	p.LoadedAt = &now

	if err := h.engine.DB().UpdatePayload(p); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiBulkRegisterPayloads(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BlueprintID int64 `json:"blueprint_id"`
		Count       int   `json:"count"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	if req.Count <= 0 || req.Count > 100 {
		h.jsonError(w, "count must be 1-100", http.StatusBadRequest)
		return
	}

	var created []int64
	for i := 0; i < req.Count; i++ {
		p := &store.Payload{
			BlueprintID: req.BlueprintID,
		}
		if err := h.engine.DB().CreatePayload(p); err != nil {
			h.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		created = append(created, p.ID)
	}
	h.jsonOK(w, map[string]any{"created": len(created), "ids": created})
}

func (h *Handlers) apiListPayloadEvents(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	events, err := h.engine.DB().ListPayloadEvents(id, 50)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, events)
}

func (h *Handlers) apiPayloadsByNode(w http.ResponseWriter, r *http.Request) {
	id, ok := h.parseIDParam(w, r, "id")
	if !ok {
		return
	}
	payloads, err := h.engine.DB().ListPayloadsByNode(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, payloads)
}

func (h *Handlers) apiBulkRegisterBins(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BinTypeID int64  `json:"bin_type_id"`
		Count     int    `json:"count"`
		Prefix    string `json:"prefix"`
		NodeID    *int64 `json:"node_id,omitempty"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	if req.Count <= 0 || req.Count > 100 {
		h.jsonError(w, "count must be 1-100", http.StatusBadRequest)
		return
	}

	var created []int64
	for i := 0; i < req.Count; i++ {
		b := &store.Bin{
			BinTypeID: req.BinTypeID,
			Label:     fmt.Sprintf("%s%04d", req.Prefix, i+1),
			Status:    "available",
			NodeID:    req.NodeID,
		}
		if err := h.engine.DB().CreateBin(b); err != nil {
			h.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		created = append(created, b.ID)
	}
	h.jsonOK(w, map[string]any{"created": len(created), "ids": created})
}
