package www

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
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
