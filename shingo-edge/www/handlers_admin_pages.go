package www

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"

	"shingo/protocol/auth"
	"shingoedge/store"
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
	shifts, _ := h.engine.ListShifts()
	if shifts == nil {
		shifts = []store.Shift{}
	}
	shiftsJSON, _ := json.Marshal(shifts)

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
	processes, _ := h.engine.ListProcesses()
	styles, _ := h.engine.ListStyles()
	stations, _ := h.engine.ListOperatorStations()
	coreNodes := h.engine.CoreNodes()
	plcNames := h.engine.PLCManager().PLCNames()

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
	var processStyles []store.Style
	var processStations []store.OperatorStation
	var processNodes []store.ProcessNode
	stationNodeMap := map[int64][]string{}
	if activeProcess != nil {
		activeProcessID = activeProcess.ID
		processStyles, _ = h.engine.ListStylesByProcess(activeProcess.ID)
		processStations, _ = h.engine.ListOperatorStationsByProcess(activeProcess.ID)
		processNodes, _ = h.engine.ListProcessNodesByProcess(activeProcess.ID)
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
		stationNodeMap[sid] = append(stationNodeMap[sid], n.CoreNodeName)
		claimedByStation[n.CoreNodeName] = map[string]interface{}{
			"id":   sid,
			"name": stationNameMap[sid],
		}
	}

	anomalies, rpMap := loadAnomalyData(h)
	data := map[string]interface{}{
		"Page":               "processes",
		"Processes":          processes,
		"Styles":             styles,
		"Stations":           stations,
		"CoreNodes":          coreNodes,
		"PLCNames":           plcNames,
		"ActiveProcess":      activeProcess,
		"ActiveProcessID":    activeProcessID,
		"ProcessStyles":      processStyles,
		"ProcessStations":    processStations,
		"ProcessNodes":       processNodes,
		"StationNodeMap":     stationNodeMap,
		"ClaimedByStation":   claimedByStation,
		"Anomalies":          anomalies,
		"ReportingPointMap":  rpMap,
	}
	h.renderTemplate(w, r, "processes.html", data)
}

func (h *Handlers) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if username, ok := h.sessions.getUser(r); ok && username != "" {
		http.Redirect(w, r, "/config", http.StatusSeeOther)
		return
	}
	h.renderTemplate(w, r, "login.html", map[string]interface{}{
		"Page": "login",
	})
}

func (h *Handlers) handleLogin(w http.ResponseWriter, r *http.Request) {
	username := r.FormValue("username")
	password := r.FormValue("password")

	exists, _ := h.engine.AdminUserExists()
	if !exists {
		hash, err := auth.HashPassword(password)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if _, err := h.engine.CreateAdminUser(username, hash); err != nil {
			http.Error(w, "failed to create admin user", http.StatusInternalServerError)
			return
		}
		h.sessions.setUser(w, r, username)
		http.Redirect(w, r, "/config", http.StatusSeeOther)
		return
	}

	user, err := h.engine.GetAdminUser(username)
	if err != nil || !auth.CheckPassword(user.PasswordHash, password) {
		h.renderTemplate(w, r, "login.html", map[string]interface{}{
			"Page":  "login",
			"Error": "Invalid username or password",
		})
		return
	}

	h.sessions.setUser(w, r, username)
	http.Redirect(w, r, "/config", http.StatusSeeOther)
}

func (h *Handlers) handleLogout(w http.ResponseWriter, r *http.Request) {
	h.sessions.clear(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
