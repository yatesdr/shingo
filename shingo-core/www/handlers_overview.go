package www

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

// handleOverview renders the Operations Overview exec dashboard (plan §15).
// Read-only and public like /missions — it reasons over data the public API
// already exposes. The page module fetches every section's data client-side.
func (h *Handlers) handleOverview(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Page": "overview",
	}
	h.render(w, r, "overview.html", data)
}

// apiFootprint powers the Plant Footprint section (plan §15.D): cells/bins
// under management plus the 30-day load/unload velocity series. Plant-wide —
// ignores station/robot filters (it's a growth narrative, not a snapshot).
func (h *Handlers) apiFootprint(w http.ResponseWriter, r *http.Request) {
	fp, err := h.engine.FootprintService().Get()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(fp)
}

// apiRobotsFleet powers the Robot Fleet section (plan §15.C): per-robot
// utilization rows (mission-derived, v1) and the Fleet Load chart's hourly
// concurrency curve, plus headline fleet KPIs. The typical-day overlay is
// deferred (returns an empty typical_series) — see Q-008.
func (h *Handlers) apiRobotsFleet(w http.ResponseWriter, r *http.Request) {
	f := parseMissionFilter(r)

	aggs, _ := h.engine.MissionService().RobotMissionAggs(f)
	type agg struct{ missions, busy int64 }
	byID := make(map[string]agg, len(aggs))
	for _, a := range aggs {
		byID[a.RobotID] = agg{a.Missions, a.BusyMS}
	}

	// Utilization window length. parseMissionFilter sets Until to end-of-day,
	// so a single-day "today" filter yields ~24h. Falls back to 24h.
	windowMS := int64(24 * 60 * 60 * 1000)
	if f.Since != nil && f.Until != nil {
		if d := f.Until.Sub(*f.Since).Milliseconds(); d > 0 {
			windowMS = d
		}
	}

	robots := h.engine.GetAllCachedRobots()
	rows := make([]map[string]any, 0, len(robots))
	var online, missionsTotal int64
	for _, rb := range robots {
		a := byID[rb.VehicleID]
		missionsTotal += a.missions
		if rb.Connected {
			online++
		}
		util := 0.0
		if windowMS > 0 {
			util = float64(a.busy) / float64(windowMS) * 100
			if util > 100 {
				util = 100
			}
		}
		rows = append(rows, map[string]any{
			"vehicle_id": rb.VehicleID,
			"state":      rb.State(),
			"util_pct":   util,
			"missions":   a.missions,
			"busy_ms":    a.busy,
			"battery":    rb.BatteryLevel,
			"connected":  rb.Connected,
			"blocked":    rb.Blocked,
			"charging":   rb.Charging,
		})
	}
	// Busiest robots first (worst-performer / hottest at the top).
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i]["util_pct"].(float64) > rows[j]["util_pct"].(float64)
	})

	// Fleet Load: hourly concurrency for the viewed day (the filter's Until
	// day, else today).
	day := time.Now()
	if f.Until != nil {
		day = *f.Until
	}
	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())
	conc, _ := h.engine.MissionService().HourlyConcurrency(dayStart, f.StationID)

	var peak, sum int64
	peakHour := ""
	for _, c := range conc {
		sum += c.Concurrency
		if c.Concurrency > peak {
			peak = c.Concurrency
			peakHour = c.Hour.Format("15:04")
		}
	}
	avgLoad := 0.0
	if len(conc) > 0 {
		avgLoad = float64(sum) / float64(len(conc))
	}
	size := int64(len(robots))
	utilPct := 0.0
	if size > 0 {
		utilPct = avgLoad / float64(size) * 100
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"fleet": map[string]any{
			"size":             size,
			"online":           online,
			"missions":         missionsTotal,
			"avg_load":         avgLoad,
			"peak_concurrency": peak,
			"peak_hour":        peakHour,
			"util_pct":         utilPct,
			"headroom":         float64(size) - avgLoad,
			"ceiling_reached":  size > 0 && peak >= size,
		},
		"load_series":    conc,
		"typical_series": []any{}, // typical-day overlay deferred (Q-008)
		"robots":         rows,
	})
}
