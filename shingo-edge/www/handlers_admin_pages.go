package www

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"

	"shingoedge/store"
)

func (h *Handlers) handleConfig(w http.ResponseWriter, r *http.Request) {
	db := h.engine.DB()
	cfg := h.engine.AppConfig()
	mgr := h.engine.PLCManager()

	plcNames := mgr.PLCNames()
	plcStatus := make(map[string]bool)
	plcStatuses := mgr.PLCStatuses()
	for _, name := range plcNames {
		plcStatus[name] = plcStatuses[name] == "Connected"
	}

	anomalies, rpMap := loadAnomalyData(h)
	shifts, _ := db.ListShifts()
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
	db := h.engine.DB()
	processes, _ := db.ListProcesses()
	styles, _ := db.ListStyles()
	stations, _ := db.ListOperatorStations()
	nodes, _ := db.ListProcessNodes()
	assignments, _ := db.ListProcessNodeAssignmentsByProcess(0)
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
	var processAssignments []store.ProcessNodeStyleAssignment
	var processCounter *store.ProcessCounterBinding
	if activeProcess != nil {
		activeProcessID = activeProcess.ID
		processStyles, _ = db.ListStylesByProcess(activeProcess.ID)
		processStations, _ = db.ListOperatorStationsByProcess(activeProcess.ID)
		processNodes, _ = db.ListProcessNodesByProcess(activeProcess.ID)
		processAssignments, _ = db.ListProcessNodeAssignmentsByProcess(activeProcess.ID)
		processCounter, _ = db.GetProcessCounterBinding(activeProcess.ID)
	}

	anomalies, rpMap := loadAnomalyData(h)
	data := map[string]interface{}{
		"Page":               "processes",
		"Processes":          processes,
		"Styles":             styles,
		"Stations":           stations,
		"Nodes":              nodes,
		"Assignments":        assignments,
		"CoreNodes":          coreNodes,
		"PLCNames":           plcNames,
		"ActiveProcess":      activeProcess,
		"ActiveProcessID":    activeProcessID,
		"ProcessStyles":      processStyles,
		"ProcessStations":    processStations,
		"ProcessNodes":       processNodes,
		"ProcessAssignments": processAssignments,
		"ProcessCounter":     processCounter,
		"Anomalies":          anomalies,
		"ReportingPointMap":  rpMap,
	}
	h.renderTemplate(w, r, "processes.html", data)
}

func (h *Handlers) handleSetup(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/config", http.StatusSeeOther)
}

func (h *Handlers) handleOperatorStationAdmin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/processes", http.StatusSeeOther)
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

	db := h.engine.DB()

	exists, _ := db.AdminUserExists()
	if !exists {
		hash, err := hashPassword(password)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if _, err := db.CreateAdminUser(username, hash); err != nil {
			http.Error(w, "failed to create admin user", http.StatusInternalServerError)
			return
		}
		h.sessions.setUser(w, r, username)
		http.Redirect(w, r, "/config", http.StatusSeeOther)
		return
	}

	user, err := db.GetAdminUser(username)
	if err != nil || !checkPassword(password, user.PasswordHash) {
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
