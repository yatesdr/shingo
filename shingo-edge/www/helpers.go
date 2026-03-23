package www

import (
	"net/http"
	"strconv"

	"shingoedge/store"
)

// resolveProcessFromQuery reads the "process" query param and returns the
// matching process, falling back to the first process if none specified.
func resolveProcessFromQuery(r *http.Request, processes []store.Process) *store.Process {
	if param := r.URL.Query().Get("process"); param != "" {
		if id, err := strconv.ParseInt(param, 10, 64); err == nil {
			for i := range processes {
				if processes[i].ID == id {
					return &processes[i]
				}
			}
		}
	}
	if len(processes) > 0 {
		return &processes[0]
	}
	return nil
}

// loadAnomalyData loads unconfirmed anomalies and builds a reporting point map
// for display in the global anomaly popover. Used by all page handlers.
func loadAnomalyData(h *Handlers) ([]store.CounterSnapshot, map[int64]map[string]string) {
	db := h.engine.DB()
	anomalies, _ := db.ListUnconfirmedAnomalies()
	reportingPoints, _ := db.ListReportingPoints()

	rpMap := make(map[int64]map[string]string)
	for _, rp := range reportingPoints {
		rpMap[rp.ID] = map[string]string{
			"PLCName": rp.PLCName,
			"TagName": rp.TagName,
		}
	}

	return anomalies, rpMap
}
