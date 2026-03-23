package www

import (
	"net/http"
	"strconv"

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

func (h *Handlers) handleChangeover(w http.ResponseWriter, r *http.Request) {
	db := h.engine.DB()
	processes, _ := db.ListProcesses()

	var activeProcess *store.Process
	if processParam := r.URL.Query().Get("process"); processParam != "" {
		if processID, err := strconv.ParseInt(processParam, 10, 64); err == nil {
			for i := range processes {
				if processes[i].ID == processID {
					activeProcess = &processes[i]
					break
				}
			}
		}
	}
	if activeProcess == nil && len(processes) > 0 {
		activeProcess = &processes[0]
	}

	var activeProcessID int64
	var styles []store.Style
	var currentStyleName string
	var activeChangeover *store.ProcessChangeover
	var stationTasks []store.ChangeoverStationTask
	nodeTaskMap := map[int64][]changeoverNodeView{}
	var centralNodeTasks []changeoverNodeView

	if activeProcess != nil {
		activeProcessID = activeProcess.ID
		styles, _ = db.ListStylesByProcess(activeProcess.ID)
		if activeProcess.ActiveStyleID != nil {
			if s, err := db.GetStyle(*activeProcess.ActiveStyleID); err == nil {
				currentStyleName = s.Name
			}
		}
		activeChangeover, _ = db.GetActiveProcessChangeover(activeProcess.ID)
		if activeChangeover != nil {
			stationTasks, _ = db.ListChangeoverStationTasks(activeChangeover.ID)
			allNodeTasks, _ := db.ListChangeoverNodeTasks(activeChangeover.ID)

			// Build map of processNodeID → operatorStationID from process nodes
			processNodes, _ := db.ListProcessNodesByProcess(activeProcess.ID)
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
					if claim, err := db.GetStyleNodeClaim(*task.FromClaimID); err == nil {
						view.FromPayload = claim.PayloadCode
						view.FromRole = claim.Role
					}
				}
				if task.ToClaimID != nil {
					if claim, err := db.GetStyleNodeClaim(*task.ToClaimID); err == nil {
						view.ToPayload = claim.PayloadCode
						view.ToRole = claim.Role
					}
				}
				// Check linked orders for failures
				for _, orderID := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
					if orderID != nil {
						if o, err := db.GetOrder(*orderID); err == nil && o.Status == "failed" {
							view.LastOrderError = "Order " + o.UUID[:8] + " failed"
						}
					}
				}
				stationID := nodeStationMap[task.ProcessNodeID]
				if stationID != nil {
					nodeTaskMap[*stationID] = append(nodeTaskMap[*stationID], view)
				} else {
					centralNodeTasks = append(centralNodeTasks, view)
				}
			}
		}
	}

	var changeoverHistory []store.ProcessChangeover
	if activeProcess != nil {
		changeoverHistory, _ = db.ListProcessChangeovers(activeProcess.ID)
		// Filter out the active changeover from history
		filtered := changeoverHistory[:0]
		for _, c := range changeoverHistory {
			if activeChangeover == nil || c.ID != activeChangeover.ID {
				filtered = append(filtered, c)
			}
		}
		changeoverHistory = filtered
	}

	allNodesComplete := true
	for _, tasks := range nodeTaskMap {
		for _, t := range tasks {
			if t.State != "switched" && t.State != "verified" && t.State != "unchanged" && t.State != "released" {
				allNodesComplete = false
			}
		}
	}
	for _, t := range centralNodeTasks {
		if t.State != "switched" && t.State != "verified" && t.State != "unchanged" && t.State != "released" {
			allNodesComplete = false
		}
	}

	anomalies, rpMap := loadAnomalyData(h)
	data := map[string]interface{}{
		"Page":               "changeover",
		"Processes":          processes,
		"ActiveProcess":      activeProcess,
		"ActiveProcessID":    activeProcessID,
		"Styles":             styles,
		"CurrentStyle":       currentStyleName,
		"ActiveChangeover":   activeChangeover,
		"StationTasks":       stationTasks,
		"NodeTaskMap":        nodeTaskMap,
		"CentralNodeTasks":   centralNodeTasks,
		"AllNodesComplete":   allNodesComplete,
		"ChangeoverHistory":  changeoverHistory,
		"Anomalies":          anomalies,
		"ReportingPointMap":  rpMap,
	}
	h.renderTemplate(w, r, "changeover.html", data)
}

func (h *Handlers) handleChangeoverPartial(w http.ResponseWriter, r *http.Request) {
	db := h.engine.DB()
	processes, _ := db.ListProcesses()

	var activeProcess *store.Process
	if processParam := r.URL.Query().Get("process"); processParam != "" {
		if processID, err := strconv.ParseInt(processParam, 10, 64); err == nil {
			for i := range processes {
				if processes[i].ID == processID {
					activeProcess = &processes[i]
					break
				}
			}
		}
	}
	if activeProcess == nil && len(processes) > 0 {
		activeProcess = &processes[0]
	}

	var styles []store.Style
	var currentStyleName string
	var activeChangeover *store.ProcessChangeover
	var stationTasks []store.ChangeoverStationTask
	nodeTaskMap := map[int64][]changeoverNodeView{}
	var centralNodeTasks []changeoverNodeView

	if activeProcess != nil {
		styles, _ = db.ListStylesByProcess(activeProcess.ID)
		if activeProcess.ActiveStyleID != nil {
			if s, err := db.GetStyle(*activeProcess.ActiveStyleID); err == nil {
				currentStyleName = s.Name
			}
		}
		activeChangeover, _ = db.GetActiveProcessChangeover(activeProcess.ID)
		if activeChangeover != nil {
			stationTasks, _ = db.ListChangeoverStationTasks(activeChangeover.ID)
			allNodeTasks, _ := db.ListChangeoverNodeTasks(activeChangeover.ID)

			processNodes, _ := db.ListProcessNodesByProcess(activeProcess.ID)
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
					if claim, err := db.GetStyleNodeClaim(*task.FromClaimID); err == nil {
						view.FromPayload = claim.PayloadCode
						view.FromRole = claim.Role
					}
				}
				if task.ToClaimID != nil {
					if claim, err := db.GetStyleNodeClaim(*task.ToClaimID); err == nil {
						view.ToPayload = claim.PayloadCode
						view.ToRole = claim.Role
					}
				}
				for _, orderID := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
					if orderID != nil {
						if o, err := db.GetOrder(*orderID); err == nil && o.Status == "failed" {
							view.LastOrderError = "Order " + o.UUID[:8] + " failed"
						}
					}
				}
				stationID := nodeStationMap[task.ProcessNodeID]
				if stationID != nil {
					nodeTaskMap[*stationID] = append(nodeTaskMap[*stationID], view)
				} else {
					centralNodeTasks = append(centralNodeTasks, view)
				}
			}
		}
	}

	allNodesComplete := true
	for _, tasks := range nodeTaskMap {
		for _, t := range tasks {
			if t.State != "switched" && t.State != "verified" && t.State != "unchanged" && t.State != "released" {
				allNodesComplete = false
			}
		}
	}
	for _, t := range centralNodeTasks {
		if t.State != "switched" && t.State != "verified" && t.State != "unchanged" && t.State != "released" {
			allNodesComplete = false
		}
	}

	data := map[string]interface{}{
		"ActiveProcess":    activeProcess,
		"Styles":           styles,
		"CurrentStyle":     currentStyleName,
		"ActiveChangeover": activeChangeover,
		"StationTasks":     stationTasks,
		"NodeTaskMap":      nodeTaskMap,
		"CentralNodeTasks": centralNodeTasks,
		"AllNodesComplete": allNodesComplete,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "changeover-body", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
