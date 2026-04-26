package www

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"shingocore/store/bins"
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
		NodeName          string  `json:"node_name"`
		BinLabel          string  `json:"bin_label,omitempty"`
		BinTypeCode       string  `json:"bin_type_code,omitempty"`
		PayloadCode       string  `json:"payload_code,omitempty"`
		UOPRemaining      int     `json:"uop_remaining"`
		Manifest          *string `json:"manifest,omitempty"`
		ManifestConfirmed bool    `json:"manifest_confirmed"`
		Occupied          bool    `json:"occupied"`
	}

	nodes := h.engine.NodeService()
	result := make([]nodeBinInfo, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		entry := nodeBinInfo{NodeName: name}
		node, err := nodes.GetByDotName(name)
		if err != nil {
			result = append(result, entry)
			continue
		}
		bins, err := nodes.ListBinsByNode(node.ID)
		if err != nil || len(bins) == 0 {
			result = append(result, entry)
			continue
		}
		bin := bins[0]
		entry.Occupied = true
		entry.BinLabel = bin.Label
		entry.BinTypeCode = bin.BinTypeCode
		entry.PayloadCode = bin.PayloadCode
		entry.UOPRemaining = bin.UOPRemaining
		entry.Manifest = bin.Manifest
		entry.ManifestConfirmed = bin.ManifestConfirmed
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
	payloads := h.engine.PayloadService()
	payload, err := payloads.GetByCode(code)
	if err != nil {
		h.jsonOK(w, map[string]interface{}{"uop_capacity": 0, "items": []struct{}{}})
		return
	}
	type manifestItem struct {
		PartNumber  string `json:"part_number"`
		Quantity    int64  `json:"quantity"`
		Description string `json:"description"`
	}
	items, err := payloads.ListManifest(payload.ID)
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

// apiTelemetryNodeChildren returns the direct physical (non-synthetic) children of an NGRP node.
// GET /api/telemetry/node/{name}/children
func (h *Handlers) apiTelemetryNodeChildren(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		h.jsonOK(w, []struct{}{})
		return
	}
	nodes := h.engine.NodeService()
	node, err := nodes.GetByDotName(name)
	if err != nil {
		h.jsonOK(w, []struct{}{})
		return
	}
	children, err := nodes.ListChildNodes(node.ID)
	if err != nil {
		h.jsonOK(w, []struct{}{})
		return
	}
	type childInfo struct {
		Name     string `json:"name"`
		NodeType string `json:"node_type"`
	}
	var result []childInfo
	for _, c := range children {
		if !c.IsSynthetic {
			result = append(result, childInfo{
				Name:     node.Name + "." + c.Name,
				NodeType: c.NodeTypeCode,
			})
		}
	}
	if result == nil {
		result = []childInfo{}
	}
	h.jsonOK(w, result)
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

	nodes := h.engine.NodeService()
	node, err := nodes.GetByDotName(req.NodeName)
	if err != nil {
		h.jsonError(w, fmt.Sprintf("node %q not found", req.NodeName), http.StatusNotFound)
		return
	}
	binList, err := nodes.ListBinsByNode(node.ID)
	if err != nil || len(binList) == 0 {
		h.jsonError(w, fmt.Sprintf("no bin at node %s", req.NodeName), http.StatusBadRequest)
		return
	}
	bin := binList[0]

	manifest := bins.Manifest{Items: make([]bins.ManifestEntry, len(req.Manifest))}
	var totalQty int64
	for i, item := range req.Manifest {
		manifest.Items[i] = bins.ManifestEntry{CatID: item.PartNumber, Quantity: item.Quantity}
		totalQty += item.Quantity
	}
	manifestJSON, _ := json.Marshal(manifest)

	uop := req.UOPCount
	if uop <= 0 {
		uop = totalQty
	}

	if err := h.engine.BinManifest().SetForProduction(bin.ID, string(manifestJSON), req.PayloadCode, int(uop)); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.engine.BinManifest().Confirm(bin.ID, ""); err != nil {
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
	nodes := h.engine.NodeService()
	node, err := nodes.GetByDotName(req.NodeName)
	if err != nil {
		h.jsonError(w, fmt.Sprintf("node %q not found", req.NodeName), http.StatusNotFound)
		return
	}
	bins, err := nodes.ListBinsByNode(node.ID)
	if err != nil || len(bins) == 0 {
		h.jsonError(w, fmt.Sprintf("no bin at node %s", req.NodeName), http.StatusBadRequest)
		return
	}
	bin := bins[0]
	if err := h.engine.BinManifest().ClearForReuse(bin.ID); err != nil {
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

// ── E-Maint Robot Telemetry ──────────────────────────────────
//
// Generates an on-demand telemetry snapshot from the in-memory robot cache.
// No persistence, no background goroutine — just reads what robotRefreshLoop
// already keeps warm (2-second freshness).

// apiEMaintRobotTelemetry returns a fleet-wide telemetry report for e-maintenance.
// GET /api/telemetry/e-maint
func (h *Handlers) apiEMaintRobotTelemetry(w http.ResponseWriter, r *http.Request) {
	report := h.buildEMaintReport()
	h.jsonOK(w, report)
}

// apiEMaintRobotTelemetryDownload returns the same report as a downloadable JSON file.
// GET /api/telemetry/e-maint/download
func (h *Handlers) apiEMaintRobotTelemetryDownload(w http.ResponseWriter, r *http.Request) {
	report := h.buildEMaintReport()
	filename := fmt.Sprintf("robot-telemetry-%s.json", time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	json.NewEncoder(w).Encode(report)
}

func (h *Handlers) buildEMaintReport() map[string]any {
	robots := h.engine.GetAllCachedRobots()
	now := time.Now().UTC()

	entries := make([]map[string]any, 0, len(robots))
	for _, r := range robots {
		entry := map[string]any{
			"vehicle_id":        r.VehicleID,
			"connected":         r.Connected,
			"snapshot_at":       now.Format(time.RFC3339),
			"position": map[string]any{
				"x":               r.X,
				"y":               r.Y,
				"angle":            r.Angle,
				"current_station":  r.CurrentStation,
				"current_map":      r.CurrentMap,
			},
			"odometer": map[string]any{
				"total_m": r.OdoTotal,
				"today_m": r.OdoToday,
			},
			"runtime": map[string]any{
				"session_ms": r.SessionMs,
				"total_ms":   r.TotalMs,
			},
			"lifts": map[string]any{
				"total_count":      r.LiftCount,
				"current_height_mm": r.LiftHeight,
				"error_code":       r.LiftError,
			},
			"battery": map[string]any{
				"level_pct":  r.BatteryLevel,
				"charging":   r.Charging,
				"voltage_v":  r.BatteryV,
				"current_a":  r.BatteryA,
			},
			"controller": map[string]any{
				"temp_c":      r.CtrlTemp,
				"humidity_pct": r.CtrlHumi,
				"voltage_v":   r.CtrlVoltage,
			},
			"safety": map[string]any{
				"blocked":    r.Blocked,
				"emergency":  r.Emergency,
			},
			"task": map[string]any{
				"status":  r.State(),
				"model":   r.Model,
				"version": r.Version,
			},
		}
		entries = append(entries, entry)
	}

	return map[string]any{
		"report_id":    uuid.New().String(),
		"generated_at": now.Format(time.RFC3339),
		"source":       "shingo-core",
		"robot_count":  len(robots),
		"robots":       entries,
	}
}
