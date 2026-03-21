package www

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"
	"time"
)

func (h *Handlers) handleProduction(w http.ResponseWriter, r *http.Request) {
	db := h.engine.DB()

	processes, _ := db.ListProcesses()

	// Determine active process
	var activeProcessID int64
	if lineParam := r.URL.Query().Get("process"); lineParam != "" {
		if id, err := strconv.ParseInt(lineParam, 10, 64); err == nil {
			activeProcessID = id
		}
	}
	if activeProcessID == 0 && len(processes) > 0 {
		activeProcessID = processes[0].ID
	}

	// Date param (default today)
	dateStr := r.URL.Query().Get("date")
	if dateStr == "" {
		dateStr = time.Now().Format("2006-01-02")
	}

	// Styles for this process (for the style toggle)
	styles, _ := db.ListStylesByProcess(activeProcessID)

	// Style filter: "all" (default) or a specific style ID
	styleParam := r.URL.Query().Get("style")
	var filterStyleID int64
	var filterStyleName string
	if styleParam != "" && styleParam != "all" {
		if id, err := strconv.ParseInt(styleParam, 10, 64); err == nil {
			for _, js := range styles {
				if js.ID == id {
					filterStyleID = id
					filterStyleName = js.Name
					break
				}
			}
		}
	}

	shifts, _ := db.ListShifts()

	// Load hourly counts
	var hourlyCounts map[int]int64
	if activeProcessID > 0 {
		if filterStyleID > 0 {
			// Single style: convert list to map
			counts, _ := db.ListHourlyCounts(activeProcessID, filterStyleID, dateStr)
			hourlyCounts = make(map[int]int64)
			for _, c := range counts {
				hourlyCounts[c.Hour] = c.Delta
			}
		} else {
			hourlyCounts, _ = db.HourlyCountTotals(activeProcessID, dateStr)
		}
	}
	if hourlyCounts == nil {
		hourlyCounts = make(map[int]int64)
	}

	anomalies, rpMap := loadAnomalyData(h)

	shiftsJSON, _ := json.Marshal(shifts)
	if shiftsJSON == nil {
		shiftsJSON = []byte("[]")
	}

	hourlyCountsJSON, _ := json.Marshal(hourlyCounts)
	if hourlyCountsJSON == nil {
		hourlyCountsJSON = []byte("{}")
	}

	// Build style list for JS toggle: [{id, name}, ...]
	type styleEntry struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	styleList := make([]styleEntry, len(styles))
	for i, js := range styles {
		styleList[i] = styleEntry{ID: js.ID, Name: js.Name}
	}
	stylesJSON, _ := json.Marshal(styleList)
	if stylesJSON == nil {
		stylesJSON = []byte("[]")
	}

	data := map[string]interface{}{
		"Page":              "production",
		"Processes":         processes,
		"ActiveProcessID":   activeProcessID,
		"Date":              dateStr,
		"Shifts":            shifts,
		"ShiftsJSON":        template.JS(shiftsJSON),
		"HourlyCountsJSON":  template.JS(hourlyCountsJSON),
		"StylesJSON":        template.JS(stylesJSON),
		"FilterStyleID":     filterStyleID,
		"FilterStyleName":   filterStyleName,
		"Anomalies":         anomalies,
		"ReportingPointMap": rpMap,
	}

	h.renderTemplate(w, r, "production.html", data)
}

func (h *Handlers) apiListShifts(w http.ResponseWriter, r *http.Request) {
	shifts, err := h.engine.DB().ListShifts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, shifts)
}

func (h *Handlers) apiSaveShifts(w http.ResponseWriter, r *http.Request) {
	var shifts []struct {
		ShiftNumber int    `json:"shift_number"`
		Name        string `json:"name"`
		StartTime   string `json:"start_time"`
		EndTime     string `json:"end_time"`
	}
	if err := json.NewDecoder(r.Body).Decode(&shifts); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	db := h.engine.DB()
	for _, s := range shifts {
		if s.ShiftNumber < 1 || s.ShiftNumber > 3 {
			continue
		}
		if s.StartTime == "" && s.EndTime == "" {
			db.DeleteShift(s.ShiftNumber)
			continue
		}
		if err := db.UpsertShift(s.ShiftNumber, s.Name, s.StartTime, s.EndTime); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	h.requestBackup("shifts")
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

func (h *Handlers) apiGetHourlyCounts(w http.ResponseWriter, r *http.Request) {
	dateStr := r.URL.Query().Get("date")
	if dateStr == "" {
		dateStr = time.Now().Format("2006-01-02")
	}
	lineID, _ := strconv.ParseInt(r.URL.Query().Get("line_id"), 10, 64)

	if lineID == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
		return
	}

	counts, err := h.engine.DB().HourlyCountTotals(lineID, dateStr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, counts)
}
