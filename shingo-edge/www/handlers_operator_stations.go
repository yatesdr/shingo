package www

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"shingo/protocol"
	"shingoedge/engine"
	"shingoedge/store"
)

func (h *Handlers) handleOperatorStationDisplay(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		http.Error(w, "invalid station id", http.StatusBadRequest)
		return
	}
	station, err := h.engine.DB().GetOperatorStation(id)
	if err != nil {
		http.Error(w, "station not found", http.StatusNotFound)
		return
	}
	_ = h.engine.DB().TouchOperatorStation(id, "online")
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
	view, err := h.engine.DB().BuildOperatorStationView(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "station not found")
		return
	}
	views := []store.OperatorStationView{*view}
	enrichViewBinState(h.engine.CoreAPI(), views)
	view.Nodes = views[0].Nodes
	_ = h.engine.DB().TouchOperatorStation(id, "online")
	writeJSON(w, view)
}

func (h *Handlers) apiGetActiveOrders(w http.ResponseWriter, r *http.Request) {
	orders, err := h.engine.DB().ListActiveOrders()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, orders)
}

func (h *Handlers) apiListOperatorStations(w http.ResponseWriter, r *http.Request) {
	stations, err := h.engine.DB().ListOperatorStations()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, stations)
}

func (h *Handlers) apiCreateOperatorStation(w http.ResponseWriter, r *http.Request) {
	var in store.OperatorStationInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := h.engine.DB().CreateOperatorStation(in)
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
	var in store.OperatorStationInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.DB().UpdateOperatorStation(id, in); err != nil {
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
	if err := h.engine.DB().DeleteOperatorStation(id); err != nil {
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
	if err := h.engine.DB().MoveOperatorStation(id, req.Direction); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiListConfiguredProcessNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.engine.DB().ListProcessNodes()
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
	nodes, err := h.engine.DB().ListProcessNodesByStation(stationID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, nodes)
}

func (h *Handlers) apiCreateProcessNode(w http.ResponseWriter, r *http.Request) {
	var in store.ProcessNodeInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := h.engine.DB().CreateProcessNode(in)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, _ = h.engine.DB().EnsureProcessNodeRuntime(id)
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdateProcessNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var in store.ProcessNodeInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.DB().UpdateProcessNode(id, in); err != nil {
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
	if err := h.engine.DB().DeleteProcessNode(id); err != nil {
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
	result, err := h.engine.RequestNodeMaterial(id, 1)
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
	order, err := h.engine.ReleaseNodeEmpty(id)
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
	order, err := h.engine.ReleaseNodePartial(id, req.Qty)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, order, "refreshMaterial")
}

func (h *Handlers) apiConfirmNodeManifest(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	if err := h.engine.ConfirmNodeManifest(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiFinalizeProduceNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	result, err := h.engine.FinalizeProduceNode(id)
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
	if err := h.engine.LoadBin(id, req.PayloadCode, req.UOPCount, req.Manifest); err != nil {
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
	order, err := h.engine.RequestEmptyBin(id, req.PayloadCode)
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
	if err := h.engine.ClearBin(id); err != nil {
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
	if err := h.engine.DB().UpdateProcessNodeRuntimeOrders(id, nil, nil); err != nil {
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
	co, err := h.engine.StartProcessChangeover(processID, req.ToStyleID, req.CalledBy, req.Notes)
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
	if err := h.engine.CancelProcessChangeover(processID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "cancelled"}})
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshChangeover")
}

func (h *Handlers) apiCompleteProcessProductionCutover(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	if err := h.engine.CompleteProcessProductionCutover(processID); err != nil {
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
	order, err := h.engine.StageNodeChangeoverMaterial(processID, nodeID)
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
	order, err := h.engine.EmptyNodeForToolChange(processID, nodeID, req.Qty)
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
	order, err := h.engine.ReleaseNodeIntoProduction(processID, nodeID)
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
	if err := h.engine.SwitchNodeToTarget(processID, nodeID); err != nil {
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
	if err := h.engine.SwitchOperatorStationToTarget(processID, stationID); err != nil {
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
	names, err := h.engine.DB().GetStationNodeNames(id)
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
	if err := h.engine.DB().SetStationNodes(id, req.Nodes); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "material-refresh", Data: map[string]string{"action": "station-nodes-updated"}})
	writeJSON(w, map[string]string{"status": "ok"})
}
