//go:build sim

package www

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/protocol/clock"
)

// registerSimRoutes adds the dev-only sim control endpoints (the live speed
// toggle) on the edge — the edge owns its own SimClock, which paces the fake
// PLC counters, so the dev top-strip changes both core and edge speed. Compiled
// only into -tags sim builds; the non-sim stub is a no-op.
func (h *Handlers) registerSimRoutes(r chi.Router) {
	r.Get("/sim/status", h.apiSimStatus)
	r.Post("/sim/speed", h.apiSimSetSpeed)
	r.Post("/sim/seed-production-demo", h.apiSimSeedProductionDemo)
	r.Post("/sim/seed-loader-graphs", h.apiSimSeedLoaderGraphs)
	r.Post("/sim/clear-hourly-counts", h.apiSimClearHourlyCounts)
}

// apiSimStatus reports the edge sim-clock speed + simulated time.
func (h *Handlers) apiSimStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"sim": true, "has_clock": false}
	if sc := clock.AsSimClock(); sc != nil {
		resp["has_clock"] = true
		resp["speed"] = sc.Speed()
		// Requested vs effective: SetSpeed clamps against max_speed, so a
		// crank past the cap otherwise reports plain success at a speed the
		// clock is not running. Reporting both lets the dev top-strip say
		// "asked N, running M" instead of silently disagreeing with itself.
		resp["requested_speed"] = sc.RequestedSpeed()
		resp["max_speed"] = sc.MaxSpeed()
		resp["sim_now"] = sc.Now().UTC().Format(time.RFC3339)
		resp["epoch"] = sc.Epoch().UTC().Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// apiSimSetSpeed changes the edge sim speed multiplier live via SimClock.SetSpeed.
// The re-pacing PLC tickers pick up the new rate on their next cycle, so the
// production counter rate changes without a restart.
func (h *Handlers) apiSimSetSpeed(w http.ResponseWriter, r *http.Request) {
	// Accept ?speed=N (a no-body POST is a CORS "simple request", so the dev
	// top-strip on the core page can set the edge's speed cross-origin without a
	// preflight) or a JSON body {"speed": N}.
	speed := 0.0
	if q := r.URL.Query().Get("speed"); q != "" {
		speed, _ = strconv.ParseFloat(q, 64)
	} else {
		var body struct {
			Speed float64 `json:"speed"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		speed = body.Speed
	}
	if speed <= 0 || speed > 100000 {
		http.Error(w, "speed must be in (0, 100000]", http.StatusBadRequest)
		return
	}
	sc := clock.AsSimClock()
	if sc == nil {
		http.Error(w, "no sim clock installed", http.StatusServiceUnavailable)
		return
	}
	sc.SetSpeed(speed)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":              true,
		"speed":           sc.Speed(),
		"requested_speed": sc.RequestedSpeed(),
		"max_speed":       sc.MaxSpeed(),
	})
}

// apiSimSeedProductionDemo inserts demo lineside buckets and hourly
// chart data so the /production page has rows to modify/delete and a
// populated Shift Production graph. Sim builds only — call once via
//
//	//   curl -X POST http://localhost:8081/sim/seed-production-demo
//
// Re-running is safe: hourly_counts are wiped+reinserted for today,
// and lineside buckets merge into any existing active rows for the
// same (node, style, part).
func (h *Handlers) apiSimSeedProductionDemo(w http.ResponseWriter, r *http.Request) {
	today := time.Now().Format("2006-01-02")

	bucketsInserted, bucketErr := h.engine.ProcessService().SeedDemoLinesideBuckets()

	// Make sure 3 default shifts exist so the chart renders even on a
	// fresh dev DB. Upsert is idempotent.
	_ = h.engine.ShiftService().Upsert(1, "1st Shift", "06:00", "14:00")
	_ = h.engine.ShiftService().Upsert(2, "2nd Shift", "14:00", "22:00")
	_ = h.engine.ShiftService().Upsert(3, "3rd Shift", "22:00", "06:00")

	// Seed hourly counts across ALL processes with varying volumes so
	// both per-process and "All Processes" graph views have data.
	processesSeeded, countsErr := h.engine.CounterService().SeedDemoHourlyCountsAllProcesses(today)

	resp := map[string]any{
		"ok":               bucketErr == nil && countsErr == nil,
		"date":             today,
		"buckets":          bucketsInserted,
		"processes_seeded": processesSeeded,
		"shifts_seed":      true,
	}
	if bucketErr != nil {
		resp["buckets_error"] = bucketErr.Error()
	}
	if countsErr != nil {
		resp["counts_error"] = countsErr.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// apiSimSeedLoaderGraphs clears hourly counts for all processes, then
// seeds the three loader processes (LOADER-CLIP, LOADER-COMP,
// LOADER-STUD) with 500 parts per shift (1500 total per process)
// spread unevenly across 24 hours. Sim builds only.
//
// Call via:
//
//	curl -X POST http://localhost:8081/sim/seed-loader-graphs
func (h *Handlers) apiSimSeedLoaderGraphs(w http.ResponseWriter, r *http.Request) {
	today := time.Now().Format("2006-01-02")

	_ = h.engine.ShiftService().Upsert(1, "1st Shift", "06:00", "14:00")
	_ = h.engine.ShiftService().Upsert(2, "2nd Shift", "14:00", "22:00")
	_ = h.engine.ShiftService().Upsert(3, "3rd Shift", "22:00", "06:00")

	// Find the three loader processes by name.
	processes, _ := h.engine.ProcessService().List()
	styles, _ := h.engine.StyleService().List()

	targetNames := map[string]bool{
		"LOADER-CLIP": true,
		"LOADER-COMP": true,
		"LOADER-STUD": true,
	}

	// Also clear hourly counts for ALL processes first so the old
	// seed data doesn't pollute the graph.
	for _, p := range processes {
		_ = h.engine.CounterService().SeedDemoHourlyCountsClear(p.ID, today)
	}

	type result struct {
		Name   string `json:"name"`
		ID     int64  `json:"id"`
		Total  int64  `json:"total"`
		Status string `json:"status"`
	}
	var results []result

	for _, p := range processes {
		if !targetNames[p.Name] {
			continue
		}
		var styleID int64
		for _, st := range styles {
			if st.ProcessID == p.ID {
				styleID = st.ID
				break
			}
		}
		if styleID == 0 {
			results = append(results, result{Name: p.Name, ID: p.ID, Status: "no style found"})
			continue
		}
		err := h.engine.CounterService().SeedDemoHourlyCountsForProcess(p.ID, styleID, today)
		if err != nil {
			results = append(results, result{Name: p.Name, ID: p.ID, Status: err.Error()})
		} else {
			results = append(results, result{Name: p.Name, ID: p.ID, Total: 1500, Status: "ok"})
		}
	}

	resp := map[string]any{
		"ok":      true,
		"date":    today,
		"results": results,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// apiSimClearHourlyCounts wipes all hourly_counts rows for today across
// every process, removing all injected demo data from the Shift
// Production graph. Sim builds only.
//
// Call via:
//
//	curl -X POST http://localhost:8081/sim/clear-hourly-counts
func (h *Handlers) apiSimClearHourlyCounts(w http.ResponseWriter, r *http.Request) {
	today := time.Now().Format("2006-01-02")
	processes, _ := h.engine.ProcessService().List()
	cleared := 0
	for _, p := range processes {
		if err := h.engine.CounterService().SeedDemoHourlyCountsClear(p.ID, today); err == nil {
			cleared++
		}
	}
	resp := map[string]any{
		"ok":                true,
		"date":              today,
		"processes_cleared": cleared,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
