package www

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"shingocore/store"
)

// apiTelemetryNodeBins returns bin state for requested core nodes.
// GET /api/telemetry/node-bins?nodes=NODE-A,NODE-B
// Returns a JSON array of {node_name, bin_label, payload_code, uop_remaining, occupied}.
func (h *Handlers) apiTelemetryNodeBins(w http.ResponseWriter, r *http.Request) {
	nodesParam := r.URL.Query().Get("nodes")
	if nodesParam == "" {
		h.jsonOK(w, []struct{}{})
		return
	}
	names := strings.Split(nodesParam, ",")

	type nodeBinInfo struct {
		NodeName     string `json:"node_name"`
		BinLabel     string `json:"bin_label,omitempty"`
		PayloadCode  string `json:"payload_code,omitempty"`
		UOPRemaining int    `json:"uop_remaining"`
		Occupied     bool   `json:"occupied"`
	}

	db := h.engine.DB()
	result := make([]nodeBinInfo, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		entry := nodeBinInfo{NodeName: name}
		node, err := db.GetNodeByDotName(name)
		if err != nil {
			result = append(result, entry)
			continue
		}
		bins, err := db.ListBinsByNode(node.ID)
		if err != nil || len(bins) == 0 {
			result = append(result, entry)
			continue
		}
		bin := bins[0]
		entry.Occupied = true
		entry.BinLabel = bin.Label
		entry.PayloadCode = bin.PayloadCode
		entry.UOPRemaining = bin.UOPRemaining
		result = append(result, entry)
	}
	h.jsonOK(w, result)
}

// apiTelemetryPayloadManifest returns the default manifest template and UOP capacity for a payload.
// GET /api/telemetry/payload/{code}/manifest
func (h *Handlers) apiTelemetryPayloadManifest(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	if code == "" {
		h.jsonOK(w, map[string]interface{}{"uop_capacity": 0, "items": []struct{}{}})
		return
	}
	db := h.engine.DB()
	payload, err := db.GetPayloadByCode(code)
	if err != nil {
		h.jsonOK(w, map[string]interface{}{"uop_capacity": 0, "items": []struct{}{}})
		return
	}
	type manifestItem struct {
		PartNumber  string `json:"part_number"`
		Quantity    int64  `json:"quantity"`
		Description string `json:"description"`
	}
	items, err := db.ListPayloadManifest(payload.ID)
	if err != nil || len(items) == 0 {
		// No manifest template — return a single entry with the payload code as part number
		h.jsonOK(w, map[string]interface{}{
			"uop_capacity": payload.UOPCapacity,
			"items": []manifestItem{
				{PartNumber: code, Quantity: int64(payload.UOPCapacity), Description: payload.Description},
			},
		})
		return
	}
	result := make([]manifestItem, len(items))
	for i, item := range items {
		result[i] = manifestItem{
			PartNumber:  item.PartNumber,
			Quantity:    item.Quantity,
			Description: item.Description,
		}
	}
	h.jsonOK(w, map[string]interface{}{
		"uop_capacity": payload.UOPCapacity,
		"items":        result,
	})
}

// apiBinLoad sets the manifest on the bin at a node. Direct HTTP replacement
// for bin loading — synchronous, returns updated bin state.
// POST /api/telemetry/bin-load
func (h *Handlers) apiBinLoad(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeName    string `json:"node_name"`
		PayloadCode string `json:"payload_code"`
		UOPCount    int64  `json:"uop_count"`
		Manifest    []struct {
			PartNumber  string `json:"part_number"`
			Quantity    int64  `json:"quantity"`
			Description string `json:"description,omitempty"`
		} `json:"manifest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.NodeName == "" {
		h.jsonError(w, "node_name is required", http.StatusBadRequest)
		return
	}

	db := h.engine.DB()
	node, err := db.GetNodeByDotName(req.NodeName)
	if err != nil {
		h.jsonError(w, fmt.Sprintf("node %q not found", req.NodeName), http.StatusNotFound)
		return
	}
	bins, err := db.ListBinsByNode(node.ID)
	if err != nil || len(bins) == 0 {
		h.jsonError(w, fmt.Sprintf("no bin at node %s", req.NodeName), http.StatusBadRequest)
		return
	}
	bin := bins[0]

	manifest := store.BinManifest{Items: make([]store.ManifestEntry, len(req.Manifest))}
	var totalQty int64
	for i, item := range req.Manifest {
		manifest.Items[i] = store.ManifestEntry{CatID: item.PartNumber, Quantity: item.Quantity}
		totalQty += item.Quantity
	}
	manifestJSON, _ := json.Marshal(manifest)

	uop := req.UOPCount
	if uop <= 0 {
		uop = totalQty
	}

	if err := db.SetBinManifest(bin.ID, string(manifestJSON), req.PayloadCode, int(uop)); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := db.ConfirmBinManifest(bin.ID); err != nil {
		log.Printf("telemetry: bin-load confirm manifest on bin %d: %v", bin.ID, err)
	}

	log.Printf("telemetry: bin-load bin=%d at node=%s payload=%s uop=%d", bin.ID, req.NodeName, req.PayloadCode, uop)
	h.eventHub.Broadcast("bin-update", sseJSON(map[string]any{
		"node_id": node.ID, "action": "loaded", "bin_id": bin.ID,
	}))
	h.jsonOK(w, map[string]interface{}{
		"status":        "ok",
		"bin_id":        bin.ID,
		"bin_label":     bin.Label,
		"payload_code":  req.PayloadCode,
		"uop_remaining": uop,
	})
}

// apiBinClear clears the manifest on the bin at a node, resetting it to empty.
// POST /api/telemetry/bin-clear
func (h *Handlers) apiBinClear(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeName string `json:"node_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.NodeName == "" {
		h.jsonError(w, "node_name is required", http.StatusBadRequest)
		return
	}
	db := h.engine.DB()
	node, err := db.GetNodeByDotName(req.NodeName)
	if err != nil {
		h.jsonError(w, fmt.Sprintf("node %q not found", req.NodeName), http.StatusNotFound)
		return
	}
	bins, err := db.ListBinsByNode(node.ID)
	if err != nil || len(bins) == 0 {
		h.jsonError(w, fmt.Sprintf("no bin at node %s", req.NodeName), http.StatusBadRequest)
		return
	}
	bin := bins[0]
	if err := db.ClearBinManifest(bin.ID); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("telemetry: bin-clear bin=%d at node=%s", bin.ID, req.NodeName)
	h.eventHub.Broadcast("bin-update", sseJSON(map[string]any{
		"node_id": node.ID, "action": "cleared", "bin_id": bin.ID,
	}))
	h.jsonOK(w, map[string]interface{}{
		"status":    "ok",
		"bin_id":    bin.ID,
		"bin_label": bin.Label,
	})
}
