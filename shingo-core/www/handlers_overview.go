package www

import (
	"encoding/json"
	"net/http"
	"sort"

	"shingo/shared/clock"
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
	fp, err := h.engine.FootprintService().Get(plantLocation)
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

	// Utilization window length = elapsed time, not the nominal range. The
	// filter's Until is end-of-day, so a mid-shift "today" view would otherwise
	// divide busy time by a full 24h at 9am and read absurdly low. Clamp the
	// window end to min(now, until) so the denominator is the time that has
	// actually elapsed (Q-033 phase 1; shift-awareness deferred). Falls back to
	// 24h when the filter has no explicit range.
	windowMS := int64(24 * 60 * 60 * 1000)
	if f.Since != nil && f.Until != nil {
		end := *f.Until
		if now := clock.Now(); now.Before(end) {
			end = now
		}
		if d := end.Sub(*f.Since).Milliseconds(); d > 0 {
			windowMS = d
		}
	}

	robots := h.engine.GetAllCachedRobots()
	rows := make([]map[string]any, 0, len(robots))
	var online, missionsTotal, size int64
	for _, rb := range robots {
		// Skip ghost cache entries with no vehicle_id: they render as a nameless
		// offline row and inflate the fleet size (Springfield showed 8 for 7 real
		// robots). size/online are counted from real robots only.
		if rb.VehicleID == "" {
			continue
		}
		size++
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
	// day, else today). Truncate the day in the plant timezone:
	// parseMissionFilter normalizes Until to UTC, so truncating in its own
	// (UTC) location rolled "today 23:59 plant-local" into tomorrow's UTC day —
	// charting an all-future, all-zero series (0% util, "No peak in window").
	day := clock.Now()
	if f.Until != nil {
		day = *f.Until
	}
	dayStart := plantDayStart(day)
	conc, _ := h.engine.MissionService().HourlyConcurrency(dayStart, f.StationID)

	// Clamp the series to elapsed hours (min(now, until)). A "today" view at 9am
	// otherwise averages in ~15 future zero-hours (deflating avg load / fleet
	// util) and a stray future bucket could read as the peak. Comparison is on
	// the absolute instant, so the bucket/cutoff display locations don't matter.
	cutoff := clock.Now()
	if f.Until != nil && f.Until.Before(cutoff) {
		cutoff = *f.Until
	}
	kept := conc[:0]
	for _, c := range conc {
		if !c.Hour.After(cutoff) {
			kept = append(kept, c)
		}
	}
	conc = kept

	var peak, sum int64
	peakHour := ""
	for _, c := range conc {
		sum += c.Concurrency
		if c.Concurrency > peak {
			peak = c.Concurrency
			peakHour = c.Hour.In(plantLocation).Format("15:04") // plant-local, not UTC
		}
	}
	avgLoad := 0.0
	if len(conc) > 0 {
		avgLoad = float64(sum) / float64(len(conc))
	}
	// size is the count of real (non-blank) robots, accumulated above.
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
