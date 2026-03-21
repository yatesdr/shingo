package www

import (
	"encoding/json"
	"html/template"
	"net/http"

	"shingoedge/store"
)

func (h *Handlers) handleSetup(w http.ResponseWriter, r *http.Request) {
	db := h.engine.DB()
	cfg := h.engine.AppConfig()
	mgr := h.engine.PLCManager()

	processes, _ := db.ListProcesses()
	styles, _ := db.ListStyles()
	slots, _ := db.ListSlots()
	reportingPoints, _ := db.ListReportingPoints()
	// Build StyleMap (ID -> Name) for display
	styleMap := make(map[int64]string)
	for _, js := range styles {
		styleMap[js.ID] = js.Name
	}

	// Build ProcessMap (ID -> Name) for display
	processMap := make(map[int64]string)
	for _, l := range processes {
		processMap[l.ID] = l.Name
	}

	// Build PLCNames list and connection status from WarLink discovery
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
		"Page":              "setup",
		"PLCStatus":         plcStatus,
		"PLCStatuses":       plcStatuses,
		"Processes":         processes,
		"Styles":            styles,
		"Slots":             slots,
		"ReportingPoints":   reportingPoints,
		"Config":            cfg,
		"StyleMap":          styleMap,
		"ProcessMap":        processMap,
		"PLCNames":          plcNames,
		"Anomalies":         anomalies,
		"ReportingPointMap": rpMap,
		"WarLinkConnected":  mgr.IsWarLinkConnected(),
		"StationIDDefault":  cfg.Namespace + "." + cfg.LineID,
		"ShiftsJSON":        template.JS(shiftsJSON),
	}

	h.renderTemplate(w, r, "setup.html", data)
}

func (h *Handlers) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	// If already logged in, redirect to setup
	if username, ok := h.sessions.getUser(r); ok && username != "" {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
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

	// Check if any admin user exists; if not, create one
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
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
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
	http.Redirect(w, r, "/setup", http.StatusSeeOther)
}

func (h *Handlers) handleLogout(w http.ResponseWriter, r *http.Request) {
	h.sessions.clear(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
