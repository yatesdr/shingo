package www

import (
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"strconv"

	"shingo/protocol/auth"
	"shingo/shared"
	"shingoedge/domain"
)

func (h *Handlers) handleConfig(w http.ResponseWriter, r *http.Request) {
	cfg := h.engine.AppConfig()
	mgr := h.engine.PLCManager()

	plcNames := mgr.PLCNames()
	plcStatus := make(map[string]bool)
	plcStatuses := mgr.PLCStatuses()
	for _, name := range plcNames {
		plcStatus[name] = plcStatuses[name] == "Connected"
	}

	anomalies, rpMap := loadAnomalyData(h)
	shiftList, _ := h.engine.ShiftService().List()
	if shiftList == nil {
		shiftList = []domain.Shift{}
	}
	shiftsJSON, _ := json.Marshal(shiftList)

	data := map[string]interface{}{
		"Page":              "config",
		"PLCStatus":         plcStatus,
		"PLCStatuses":       plcStatuses,
		"Config":            cfg,
		"PLCNames":          plcNames,
		"Anomalies":         anomalies,
		"ReportingPointMap": rpMap,
		"WarLinkConnected":  mgr.IsWarLinkConnected(),
		"StationIDDefault":  cfg.Namespace + "." + cfg.LineID,
		"ShiftsJSON":        template.JS(shiftsJSON),
	}
	h.renderTemplate(w, r, "config.html", data)
}

func (h *Handlers) handleProcesses(w http.ResponseWriter, r *http.Request) {
	processList, _ := h.engine.ProcessService().List()
	styles, _ := h.engine.StyleService().List()
	stationList, _ := h.engine.StationService().List()
	coreNodes := h.engine.CoreNodes()
	plcNames := h.engine.PLCManager().PLCNames()

	var activeProcess *domain.Process
	if processParam := r.URL.Query().Get("process"); processParam != "" {
		if processID, err := strconv.ParseInt(processParam, 10, 64); err == nil {
			for i := range processList {
				if processList[i].ID == processID {
					activeProcess = &processList[i]
					break
				}
			}
		}
	}
	if activeProcess == nil && len(processList) > 0 {
		activeProcess = &processList[0]
	}

	var activeProcessID int64
	var processStyles []domain.Style
	var processStations []domain.Station
	var processNodes []domain.Node
	stationNodeMap := map[int64][]string{}
	if activeProcess != nil {
		activeProcessID = activeProcess.ID
		processStyles, _ = h.engine.StyleService().ListByProcess(activeProcess.ID)
		processStations, _ = h.engine.StationService().ListByProcess(activeProcess.ID)
		processNodes, _ = h.engine.ProcessService().ListNodesByProcess(activeProcess.ID)
	}

	// Derive station→nodes map and claimed-by index from already-fetched processNodes
	stationNameMap := map[int64]string{}
	for _, s := range processStations {
		stationNameMap[s.ID] = s.Name
	}
	claimedByStation := map[string]interface{}{}
	for _, n := range processNodes {
		if n.OperatorStationID == nil {
			continue
		}
		sid := *n.OperatorStationID
		// Stale FK: process_node points at a station that no longer exists
		// (deleted screen left the FK behind). Don't grey out the node in
		// the picker for a phantom owner — clear the dangling pointer and
		// treat the node as unclaimed.
		if _, ok := stationNameMap[sid]; !ok {
			in := domain.NodeInput{
				ProcessID:         n.ProcessID,
				OperatorStationID: nil,
				CoreNodeName:      n.CoreNodeName,
				Code:              n.Code,
				Name:              n.Name,
				Sequence:          n.Sequence,
				Enabled:           n.Enabled,
			}
			if err := h.engine.ProcessService().UpdateNode(n.ID, in); err != nil {
				log.Printf("clear stale operator_station_id on node %d: %v", n.ID, err)
			}
			continue
		}
		stationNodeMap[sid] = append(stationNodeMap[sid], n.CoreNodeName)
		claimedByStation[n.CoreNodeName] = map[string]interface{}{
			"id":   sid,
			"name": stationNameMap[sid],
		}
	}

	anomalies, rpMap := loadAnomalyData(h)
	data := map[string]interface{}{
		"Page":              "processes",
		"Processes":         processList,
		"Styles":            styles,
		"Stations":          stationList,
		"CoreNodes":         coreNodes,
		"PLCNames":          plcNames,
		"ActiveProcess":     activeProcess,
		"ActiveProcessID":   activeProcessID,
		"ProcessStyles":     processStyles,
		"ProcessStations":   processStations,
		"ProcessNodes":      processNodes,
		"StationNodeMap":    stationNodeMap,
		"ClaimedByStation":  claimedByStation,
		"Anomalies":         anomalies,
		"ReportingPointMap": rpMap,
	}
	h.renderTemplate(w, r, "processes.html", data)
}

func (h *Handlers) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	next := shared.SafeNextPath(r.URL.Query().Get("next"))
	if username, ok := h.sessions.getUser(r); ok && username != "" {
		dest := next
		if dest == "" {
			dest = "/config"
		}
		http.Redirect(w, r, dest, http.StatusSeeOther)
		return
	}
	h.renderTemplate(w, r, "login.html", map[string]interface{}{
		"Page": "login",
		"Next": next,
	})
}

func (h *Handlers) handleLogin(w http.ResponseWriter, r *http.Request) {
	username := r.FormValue("username")
	password := r.FormValue("password")
	next := shared.SafeNextPath(r.FormValue("next"))
	if next == "" {
		next = shared.SafeNextPath(r.URL.Query().Get("next"))
	}
	dest := next
	if dest == "" {
		dest = "/config"
	}

	exists, _ := h.engine.AdminService().Exists()
	if !exists {
		hash, err := auth.HashPassword(password)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if _, err := h.engine.AdminService().Create(username, hash); err != nil {
			http.Error(w, "failed to create admin user", http.StatusInternalServerError)
			return
		}
		h.sessions.setUser(w, r, username)
		http.Redirect(w, r, dest, http.StatusSeeOther)
		return
	}

	user, err := h.engine.AdminService().Get(username)
	if err != nil || !auth.CheckPassword(user.PasswordHash, password) {
		h.renderTemplate(w, r, "login.html", map[string]interface{}{
			"Page":  "login",
			"Error": "Invalid username or password",
			"Next":  next,
		})
		return
	}

	h.sessions.setUser(w, r, username)
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (h *Handlers) handleLogout(w http.ResponseWriter, r *http.Request) {
	h.sessions.clear(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
