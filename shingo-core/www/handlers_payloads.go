package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"shingocore/store"
)

func (h *Handlers) handlePayloadsPage(w http.ResponseWriter, r *http.Request) {
	payloads, err := h.engine.DB().ListPayloads()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	compatNodes := make(map[int64][]string)
	for _, p := range payloads {
		nodeList, err := h.engine.DB().ListNodesForPayload(p.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, n := range nodeList {
			compatNodes[p.ID] = append(compatNodes[p.ID], n.Name)
		}
	}

	binTypes, err := h.engine.DB().ListBinTypes()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	payloadBinTypes := make(map[int64][]string)
	for _, p := range payloads {
		btList, err := h.engine.DB().ListBinTypesForPayload(p.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, bt := range btList {
			payloadBinTypes[p.ID] = append(payloadBinTypes[p.ID], bt.Code)
		}
	}

	data := map[string]any{
		"Page":            "payloads",
		"Payloads":        payloads,
		"BinTypes":        binTypes,
		"CompatNodes":     compatNodes,
		"PayloadBinTypes": payloadBinTypes,
	}
	h.render(w, r, "payloads.html", data)
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

// apiListManifest returns the manifest for a payload template (PayloadManifestItem).
func (h *Handlers) apiListManifest(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	items, err := h.engine.DB().ListPayloadManifest(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, items)
}

// apiCreateManifestItem adds a manifest item to a payload template.
func (h *Handlers) apiCreateManifestItem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PayloadID  int64  `json:"payload_id"`
		PartNumber string `json:"part_number"`
		Quantity   int64  `json:"quantity"`
		Notes      string `json:"notes"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	m := &store.PayloadManifestItem{
		PayloadID:  req.PayloadID,
		PartNumber: req.PartNumber,
		Quantity:   req.Quantity,
	}
	if err := h.engine.DB().CreatePayloadManifestItem(m); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, m)
}

// apiUpdateManifestItem updates a manifest item on a payload template.
func (h *Handlers) apiUpdateManifestItem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID         int64  `json:"id"`
		PartNumber string `json:"part_number"`
		Quantity   int64  `json:"quantity"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	if err := h.engine.DB().UpdatePayloadManifestItem(req.ID, req.PartNumber, req.Quantity); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

// apiDeleteManifestItem removes a manifest item from a payload template.
func (h *Handlers) apiDeleteManifestItem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if err := h.engine.DB().DeletePayloadManifestItem(req.ID); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

// apiConfirmManifest confirms a bin's manifest.
func (h *Handlers) apiConfirmManifest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"` // bin ID
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	if err := h.engine.BinManifest().Confirm(req.ID, ""); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

// apiListPayloadEvents returns audit log entries for a bin (replaces old payload events).
func (h *Handlers) apiListPayloadEvents(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	// Use the audit log for bin events
	events, err := h.engine.DB().ListEntityAudit("bin", id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, events)
}

// apiPayloadsByNode returns bins at a node (replaces old payloads-by-node).
func (h *Handlers) apiPayloadsByNode(w http.ResponseWriter, r *http.Request) {
	id, ok := h.parseIDParam(w, r, "id")
	if !ok {
		return
	}
	bins, err := h.engine.DB().ListBinsByNode(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, bins)
}

// apiBinManifest returns the parsed manifest for a bin.
func (h *Handlers) apiBinManifest(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	manifest, err := h.engine.DB().GetBinManifest(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, manifest)
}

// apiSetBinManifest updates a bin's manifest from JSON.
func (h *Handlers) apiSetBinManifest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BinID       int64              `json:"bin_id"`
		PayloadCode string             `json:"payload_code"`
		UOPRemaining int               `json:"uop_remaining"`
		Items       []store.ManifestEntry `json:"items"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	manifest := store.BinManifest{Items: req.Items}
	manifestJSON, _ := json.Marshal(manifest)
	if err := h.engine.BinManifest().SetForProduction(req.BinID, string(manifestJSON), req.PayloadCode, req.UOPRemaining); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

// apiClearBinManifest empties a bin's manifest.
func (h *Handlers) apiClearBinManifest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BinID int64 `json:"bin_id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	if err := h.engine.BinManifest().ClearForReuse(req.BinID); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
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
