package www

import (
	"net/http"

	"shingoedge/store"
)

// changeoverNodeView enriches a ChangeoverNodeTask with claim details for display.
type changeoverNodeView struct {
	store.ChangeoverNodeTask
	ProcessID       int64  `json:"process_id"`
	Situation       string `json:"situation"`
	FromPayload     string `json:"from_payload"`
	ToPayload       string `json:"to_payload"`
	FromRole        string `json:"from_role"`
	ToRole          string `json:"to_role"`
	LastOrderError  string `json:"last_order_error,omitempty"`
}

// changeoverViewData holds the common data loaded for changeover views.
type changeoverViewData struct {
	Styles           []store.Style
	CurrentStyleName string
	ActiveChangeover *store.ProcessChangeover
	StationTasks     []store.ChangeoverStationTask
	NodeTaskMap      map[int64][]changeoverNodeView
	CentralNodeTasks []changeoverNodeView
	AllNodesComplete bool
}

func (h *Handlers) buildChangeoverViewData(activeProcess *store.Process) changeoverViewData {
	var d changeoverViewData
	d.NodeTaskMap = map[int64][]changeoverNodeView{}

	if activeProcess == nil {
		d.AllNodesComplete = true
		return d
	}

	d.Styles, _ = h.engine.ListStylesByProcess(activeProcess.ID)
	if activeProcess.ActiveStyleID != nil {
		if s, err := h.engine.GetStyle(*activeProcess.ActiveStyleID); err == nil {
			d.CurrentStyleName = s.Name
		}
	}
	d.ActiveChangeover, _ = h.engine.GetActiveProcessChangeover(activeProcess.ID)
	if d.ActiveChangeover != nil {
		d.StationTasks, _ = h.engine.ListChangeoverStationTasks(d.ActiveChangeover.ID)
		allNodeTasks, _ := h.engine.ListChangeoverNodeTasks(d.ActiveChangeover.ID)

		// Build map of processNodeID -> operatorStationID from process nodes
		processNodes, _ := h.engine.ListProcessNodesByProcess(activeProcess.ID)
		nodeStationMap := make(map[int64]*int64, len(processNodes))
		for i := range processNodes {
			nodeStationMap[processNodes[i].ID] = processNodes[i].OperatorStationID
		}

		for _, task := range allNodeTasks {
			view := changeoverNodeView{
				ChangeoverNodeTask: task,
				ProcessID:          activeProcess.ID,
				Situation:          task.Situation,
			}
			if task.FromClaimID != nil {
				if claim, err := h.engine.GetStyleNodeClaim(*task.FromClaimID); err == nil {
					view.FromPayload = claim.PayloadCode
					view.FromRole = claim.Role
				}
			}
			if task.ToClaimID != nil {
				if claim, err := h.engine.GetStyleNodeClaim(*task.ToClaimID); err == nil {
					view.ToPayload = claim.PayloadCode
					view.ToRole = claim.Role
				}
			}
			// Check linked orders for failures
			for _, orderID := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
				if orderID != nil {
					if o, err := h.engine.GetOrder(*orderID); err == nil && o.Status == "failed" {
						view.LastOrderError = "Order " + o.UUID[:8] + " failed"
					}
				}
			}
			stationID := nodeStationMap[task.ProcessNodeID]
			if stationID != nil {
				d.NodeTaskMap[*stationID] = append(d.NodeTaskMap[*stationID], view)
			} else {
				d.CentralNodeTasks = append(d.CentralNodeTasks, view)
			}
		}
	}

	d.AllNodesComplete = true
	for _, tasks := range d.NodeTaskMap {
		for _, t := range tasks {
			if t.State != "switched" && t.State != "verified" && t.State != "unchanged" && t.State != "released" {
				d.AllNodesComplete = false
			}
		}
	}
	for _, t := range d.CentralNodeTasks {
		if t.State != "switched" && t.State != "verified" && t.State != "unchanged" && t.State != "released" {
			d.AllNodesComplete = false
		}
	}

	return d
}

func (h *Handlers) handleChangeover(w http.ResponseWriter, r *http.Request) {
	processes, _ := h.engine.ListProcesses()
	activeProcess := resolveProcessFromQuery(r, processes)

	d := h.buildChangeoverViewData(activeProcess)

	var activeProcessID int64
	if activeProcess != nil {
		activeProcessID = activeProcess.ID
	}

	var changeoverHistory []store.ProcessChangeover
	if activeProcess != nil {
		changeoverHistory, _ = h.engine.ListProcessChangeovers(activeProcess.ID)
		// Filter out the active changeover from history
		filtered := changeoverHistory[:0]
		for _, c := range changeoverHistory {
			if d.ActiveChangeover == nil || c.ID != d.ActiveChangeover.ID {
				filtered = append(filtered, c)
			}
		}
		changeoverHistory = filtered
	}

	anomalies, rpMap := loadAnomalyData(h)
	data := map[string]interface{}{
		"Page":               "changeover",
		"Processes":          processes,
		"ActiveProcess":      activeProcess,
		"ActiveProcessID":    activeProcessID,
		"Styles":             d.Styles,
		"CurrentStyle":       d.CurrentStyleName,
		"ActiveChangeover":   d.ActiveChangeover,
		"StationTasks":       d.StationTasks,
		"NodeTaskMap":        d.NodeTaskMap,
		"CentralNodeTasks":   d.CentralNodeTasks,
		"AllNodesComplete":   d.AllNodesComplete,
		"ChangeoverHistory":  changeoverHistory,
		"Anomalies":          anomalies,
		"ReportingPointMap":  rpMap,
	}
	h.renderTemplate(w, r, "changeover.html", data)
}

func (h *Handlers) handleChangeoverPartial(w http.ResponseWriter, r *http.Request) {
	processes, _ := h.engine.ListProcesses()
	activeProcess := resolveProcessFromQuery(r, processes)

	d := h.buildChangeoverViewData(activeProcess)

	data := map[string]interface{}{
		"ActiveProcess":    activeProcess,
		"Styles":           d.Styles,
		"CurrentStyle":     d.CurrentStyleName,
		"ActiveChangeover": d.ActiveChangeover,
		"StationTasks":     d.StationTasks,
		"NodeTaskMap":      d.NodeTaskMap,
		"CentralNodeTasks": d.CentralNodeTasks,
		"AllNodesComplete": d.AllNodesComplete,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "changeover-body", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
