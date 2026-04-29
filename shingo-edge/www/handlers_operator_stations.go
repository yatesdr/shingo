package www

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/engine"
)

func (h *Handlers) handleOperatorStationDisplay(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		http.Error(w, "invalid station id", http.StatusBadRequest)
		return
	}
	station, err := h.engine.StationService().Get(id)
	if err != nil {
		http.Error(w, "station not found", http.StatusNotFound)
		return
	}
	_ = h.engine.StationService().Touch(id, "online")
	data := map[string]interface{}{
		"Page":    "operator-display",
		"Station": station,
	}
	h.renderTemplate(w, r, "operator-display.html", data)
}

func (h *Handlers) apiGetOperatorStationView(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid station id")
		return
	}
	view, err := h.engine.StationService().BuildView(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "station not found")
		return
	}
	views := []domain.OperatorStationView{*view}
	enrichViewBinState(h.engine.CoreAPI(), views)
	view.Nodes = views[0].Nodes
	_ = h.engine.StationService().Touch(id, "online")
	writeJSON(w, view)
}

func (h *Handlers) apiGetActiveOrders(w http.ResponseWriter, r *http.Request) {
	orders, err := h.engine.OrderService().ListActive()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, orders)
}

func (h *Handlers) apiListOperatorStations(w http.ResponseWriter, r *http.Request) {
	stations, err := h.engine.StationService().List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, stations)
}

func (h *Handlers) apiCreateOperatorStation(w http.ResponseWriter, r *http.Request) {
	var in domain.StationInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := h.engine.StationService().Create(in)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdateOperatorStation(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var in domain.StationInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.StationService().Update(id, in); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeleteOperatorStation(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.engine.StationService().Delete(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiMoveOperatorStation(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req struct {
		Direction string `json:"direction"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Direction != "up" && req.Direction != "down" {
		writeError(w, http.StatusBadRequest, "direction must be up or down")
		return
	}
	if err := h.engine.StationService().Move(id, req.Direction); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiListConfiguredProcessNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.engine.ProcessService().ListNodes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, nodes)
}

func (h *Handlers) apiListConfiguredProcessNodesByStation(w http.ResponseWriter, r *http.Request) {
	stationID, err := parseID(r, "stationID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid station id")
		return
	}
	nodes, err := h.engine.ProcessService().ListNodesByStation(stationID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, nodes)
}

func (h *Handlers) apiCreateProcessNode(w http.ResponseWriter, r *http.Request) {
	var in domain.NodeInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := h.engine.ProcessService().CreateNode(in)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, _ = h.engine.ProcessService().EnsureNodeRuntime(id)
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdateProcessNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var in domain.NodeInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.ProcessService().UpdateNode(id, in); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeleteProcessNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.engine.ProcessService().DeleteNode(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiRequestNodeMaterial(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	result, err := h.orchestration.RequestNodeMaterial(id, 1)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, result, "refreshMaterial")
}

func (h *Handlers) apiReleaseNodeEmpty(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	order, err := h.orchestration.ReleaseNodeEmpty(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, order, "refreshMaterial")
}

func (h *Handlers) apiReleaseNodePartial(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var req struct {
		Qty int64 `json:"qty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	order, err := h.orchestration.ReleaseNodePartial(id, req.Qty)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, order, "refreshMaterial")
}

// apiReleaseNodeStagedOrders releases both orders of a two-robot swap in one
// call. See Engine.ReleaseStagedOrders for ordering (B-then-A) and
// idempotency semantics.
//
// Phase 7 (lineside): the HMI's release prompt posts qty_by_part on this
// endpoint too (single release path covers single-order and two-robot
// swaps). Forwarded to the engine so two-robot releases capture lineside
// buckets like the single-order path does.
//
// Phase 8 (release-time manifest): body now also carries a disposition so
// the "SEND PARTIAL BACK" button can return the partially-consumed bin to
// the supermarket instead of declaring it empty. Legacy body shape
// (qty_by_part only, missing disposition) maps to zero-value disposition,
// which leaves the bin's manifest untouched at Core.
func (h *Handlers) apiReleaseNodeStagedOrders(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	// Body validation lives in parseReleaseRequest (handlers_release.go) so
	// every release endpoint inherits the same post-2026-04-27 guard. See
	// that function's docstring for the contract.
	req, err := parseReleaseRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	disp := buildReleaseDisposition(req.Disposition, req.QtyByPart, req.CalledBy)
	if err := h.orchestration.ReleaseStagedOrders(id, disp); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}

func (h *Handlers) apiFinalizeProduceNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	result, err := h.orchestration.FinalizeProduceNode(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, result, "refreshMaterial")
}

func (h *Handlers) apiLoadBin(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var req struct {
		PayloadCode string                       `json:"payload_code"`
		UOPCount    int64                        `json:"uop_count"`
		Manifest    []protocol.IngestManifestItem `json:"manifest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.orchestration.LoadBin(id, req.PayloadCode, req.UOPCount, req.Manifest); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}

func (h *Handlers) apiRequestEmptyBin(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var req struct {
		PayloadCode string `json:"payload_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	order, err := h.orchestration.RequestEmptyBin(id, req.PayloadCode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, order, "refreshMaterial")
}

func (h *Handlers) apiRequestFullBin(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var req struct {
		PayloadCode string `json:"payload_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	order, err := h.orchestration.RequestFullBin(id, req.PayloadCode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, order, "refreshMaterial")
}

func (h *Handlers) apiClearBin(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	if err := h.orchestration.ClearBin(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}

func (h *Handlers) apiNodeChildren(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		writeJSON(w, []struct{}{})
		return
	}
	children, _ := h.engine.CoreAPI().FetchNodeChildren(name)
	if children == nil {
		children = []engine.NodeChildInfo{}
	}
	writeJSON(w, children)
}

func (h *Handlers) apiPayloadManifest(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	if code == "" {
		writeJSON(w, map[string]interface{}{"uop_capacity": 0, "items": []struct{}{}})
		return
	}
	result, _ := h.engine.CoreAPI().FetchPayloadManifest(code)
	if result == nil {
		result = &engine.PayloadManifestResponse{Items: []engine.ManifestItem{}}
	}
	writeJSON(w, result)
}

func (h *Handlers) apiClearNodeOrders(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	if err := h.engine.ProcessService().UpdateNodeRuntimeOrders(id, nil, nil); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}

func (h *Handlers) apiStartProcessChangeover(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	var req struct {
		ToStyleID int64  `json:"to_style_id"`
		CalledBy  string `json:"called_by"`
		Notes     string `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	co, err := h.orchestration.StartProcessChangeover(processID, req.ToStyleID, req.CalledBy, req.Notes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: co})
	writeJSONWithTrigger(w, r, co, "refreshChangeover")
}

func (h *Handlers) apiCancelProcessChangeover(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}

	// Parse optional next_style_id for cancel-as-redirect
	var req struct {
		NextStyleID *int64 `json:"next_style_id,omitempty"`
	}
	// Body is optional — plain cancel has no body
	_ = json.NewDecoder(r.Body).Decode(&req)

	if req.NextStyleID != nil {
		if err := h.orchestration.CancelProcessChangeoverRedirect(processID, req.NextStyleID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "redirected"}})
		writeJSONWithTrigger(w, r, map[string]string{"status": "ok", "action": "redirected"}, "refreshChangeover")
		return
	}

	if err := h.orchestration.CancelProcessChangeover(processID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "cancelled"}})
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshChangeover")
}

// apiReleaseChangeoverWait gates the changeover wait-points (ready / tooling done).
//
// Phase 8: ReleaseChangeoverWait now routes every staged evacuation order
// through ReleaseOrderWithLineside with a capture_lineside disposition so
// the bin's manifest is cleared at Core before the fleet picks the bin up.
// called_by is captured from the body (matching apiStartProcessChangeover's
// pattern) and threaded through for audit.
//
// Post-2026-04-27 contract (cleanup PR): called_by is now REQUIRED. The
// other two release endpoints (apiReleaseOrder, apiReleaseNodeStagedOrders)
// adopted this in commit c56ceb9 to surface the disposition-bypass
// fingerprint. This endpoint had the same shape — empty body silently
// produced an empty audit trail at Core — and now matches.
func (h *Handlers) apiReleaseChangeoverWait(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	var req struct {
		CalledBy string `json:"called_by"`
	}
	if r.ContentLength == 0 {
		writeError(w, http.StatusBadRequest, "release requires a JSON body with called_by")
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.CalledBy) == "" {
		log.Printf("release-changeover-wait: called_by empty, defaulting to operator_station (process=%d)", processID)
		req.CalledBy = "operator_station"
	}
	if err := h.orchestration.ReleaseChangeoverWait(processID, req.CalledBy); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "wait-released"}})
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshChangeover")
}

func (h *Handlers) apiCompleteProcessProductionCutover(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	if err := h.orchestration.CompleteProcessProductionCutover(processID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "cutover-complete"}})
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshChangeover")
}

func (h *Handlers) apiStageNodeChangeoverMaterial(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	nodeID, err := parseID(r, "nodeID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	order, err := h.orchestration.StageNodeChangeoverMaterial(processID, nodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "stage-material"}})
	writeJSONWithTrigger(w, r, order, "refreshChangeover")
}

func (h *Handlers) apiEmptyNodeForToolChange(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	nodeID, err := parseID(r, "nodeID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var req struct {
		Qty int64 `json:"qty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	order, err := h.orchestration.EmptyNodeForToolChange(processID, nodeID, req.Qty)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "empty-for-tool-change"}})
	writeJSONWithTrigger(w, r, order, "refreshChangeover")
}

func (h *Handlers) apiReleaseNodeIntoProduction(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	nodeID, err := parseID(r, "nodeID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	order, err := h.orchestration.ReleaseNodeIntoProduction(processID, nodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "release-into-production"}})
	writeJSONWithTrigger(w, r, order, "refreshChangeover")
}

func (h *Handlers) apiSwitchNodeToTarget(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	nodeID, err := parseID(r, "nodeID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	if err := h.orchestration.SwitchNodeToTarget(processID, nodeID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "switch-to-target"}})
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshChangeover")
}

func (h *Handlers) apiSwitchOperatorStationToTarget(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	stationID, err := parseID(r, "stationID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid station id")
		return
	}
	if err := h.orchestration.SwitchOperatorStationToTarget(processID, stationID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "switch-station-to-target"}})
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshChangeover")
}

func (h *Handlers) apiGetStationClaimedNodes(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid station id")
		return
	}
	names, err := h.engine.StationService().GetNodeNames(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, names)
}

func (h *Handlers) apiSetStationClaimedNodes(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid station id")
		return
	}
	var req struct {
		Nodes []string `json:"nodes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.StationService().SetNodes(id, req.Nodes); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "material-refresh", Data: map[string]string{"action": "station-nodes-updated"}})
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiFlipABNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	if err := h.orchestration.FlipABNode(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}
