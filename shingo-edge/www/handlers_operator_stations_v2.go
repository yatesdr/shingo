package www

import (
	"encoding/json"
	"net/http"

	"shingoedge/store"
)

func (h *Handlers) handleOperatorStationDisplay(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		http.Error(w, "invalid station id", http.StatusBadRequest)
		return
	}
	view, err := h.engine.DB().BuildOperatorStationView(id)
	if err != nil {
		http.Error(w, "station not found", http.StatusNotFound)
		return
	}
	_ = h.engine.DB().TouchOperatorStation(id, "online")
	data := map[string]interface{}{
		"Page":    "operator-display",
		"Station": view.Station,
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

func (h *Handlers) apiListOpStationNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.engine.DB().ListOpStationNodes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, nodes)
}

func (h *Handlers) apiListOpStationNodesByStation(w http.ResponseWriter, r *http.Request) {
	stationID, err := parseID(r, "stationID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid station id")
		return
	}
	nodes, err := h.engine.DB().ListOpStationNodesByStation(stationID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, nodes)
}

func (h *Handlers) apiCreateOpStationNode(w http.ResponseWriter, r *http.Request) {
	var in store.OpStationNodeInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := h.engine.DB().CreateOpStationNode(in)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, _ = h.engine.DB().EnsureOpNodeRuntime(id)
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdateOpStationNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var in store.OpStationNodeInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.DB().UpdateOpStationNode(id, in); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeleteOpStationNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.engine.DB().DeleteOpStationNode(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiListOpNodeAssignmentsByProcess(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "processID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	assignments, err := h.engine.DB().ListOpNodeAssignmentsByProcess(processID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, assignments)
}

func (h *Handlers) apiListOpNodeAssignmentsByNode(w http.ResponseWriter, r *http.Request) {
	opNodeID, err := parseID(r, "opNodeID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	assignments, err := h.engine.DB().ListOpNodeAssignmentsByNode(opNodeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, assignments)
}

func (h *Handlers) apiListOpNodeAssignmentsByStyle(w http.ResponseWriter, r *http.Request) {
	styleID, err := parseID(r, "styleID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid style id")
		return
	}
	assignments, err := h.engine.DB().ListOpNodeAssignmentsByStyle(styleID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, assignments)
}

func (h *Handlers) apiUpsertOpNodeAssignment(w http.ResponseWriter, r *http.Request) {
	var in store.OpNodeStyleAssignmentInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := h.engine.DB().UpsertOpNodeAssignment(in)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiDeleteOpNodeAssignment(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.engine.DB().DeleteOpNodeAssignment(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiRequestOpNodeMaterial(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	result, err := h.engine.RequestOpNodeMaterial(id, 1)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, result)
}

func (h *Handlers) apiReleaseOpNodeEmpty(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	order, err := h.engine.ReleaseOpNodeEmpty(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, order)
}

func (h *Handlers) apiReleaseOpNodePartial(w http.ResponseWriter, r *http.Request) {
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
	order, err := h.engine.ReleaseOpNodePartial(id, req.Qty)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, order)
}

func (h *Handlers) apiConfirmOpNodeManifest(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	if err := h.engine.ConfirmOpNodeManifest(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiStartProcessChangeoverV2(w http.ResponseWriter, r *http.Request) {
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
	co, err := h.engine.StartProcessChangeoverV2(processID, req.ToStyleID, req.CalledBy, req.Notes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, co)
}

func (h *Handlers) apiCancelProcessChangeoverV2(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	if err := h.engine.CancelProcessChangeoverV2(processID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiAdvanceProcessChangeoverPhase(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	var req struct {
		Phase string `json:"phase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.AdvanceProcessChangeoverPhase(processID, req.Phase); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
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
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiStageOpNodeChangeoverMaterial(w http.ResponseWriter, r *http.Request) {
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
	order, err := h.engine.StageOpNodeChangeoverMaterial(processID, nodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, order)
}

func (h *Handlers) apiEmptyOpNodeForToolChange(w http.ResponseWriter, r *http.Request) {
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
	order, err := h.engine.EmptyOpNodeForToolChange(processID, nodeID, req.Qty)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, order)
}

func (h *Handlers) apiReleaseOpNodeIntoProduction(w http.ResponseWriter, r *http.Request) {
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
	order, err := h.engine.ReleaseOpNodeIntoProduction(processID, nodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, order)
}

func (h *Handlers) apiSwitchOpNodeToTarget(w http.ResponseWriter, r *http.Request) {
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
	if err := h.engine.SwitchOpNodeToTarget(processID, nodeID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
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
	writeJSON(w, map[string]string{"status": "ok"})
}
