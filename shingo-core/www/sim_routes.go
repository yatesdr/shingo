//go:build sim

package www

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/shared/clock"
)

// registerSimRoutes adds the dev-only sim control endpoints (the live speed
// toggle). Compiled only into -tags sim builds; the non-sim stub
// (sim_routes_stub.go) is a no-op, so production never exposes these.
func (h *Handlers) registerSimRoutes(r chi.Router) {
	r.Get("/sim/status", h.apiSimStatus)
	r.Post("/sim/speed", h.apiSimSetSpeed)
}

// apiSimStatus reports the current sim-clock speed + simulated time so the dev
// top-strip can render the live state.
func (h *Handlers) apiSimStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"sim": true, "has_clock": false}
	if sc := clock.AsSimClock(); sc != nil {
		resp["has_clock"] = true
		resp["speed"] = sc.Speed()                    // EFFECTIVE rate the clock actually runs
		resp["requested_speed"] = sc.RequestedSpeed() // what was asked (may exceed speed when capped)
		resp["max_speed"] = sc.MaxSpeed()             // the effective-speed cap
		resp["sim_now"] = sc.Now().UTC().Format(time.RFC3339)
		resp["epoch"] = sc.Epoch().UTC().Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// apiSimSetSpeed changes the sim speed multiplier live via SimClock.SetSpeed.
// The re-pacing tickers (PLC counters, fleet driver) pick up the new rate on
// their next cycle, so production + transit speed change without a restart.
func (h *Handlers) apiSimSetSpeed(w http.ResponseWriter, r *http.Request) {
	// Accept ?speed=N (a no-body POST is a CORS "simple request", so the dev
	// top-strip can set the edge's speed cross-origin without a preflight) or a
	// JSON body {"speed": N}.
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
	// Return the EFFECTIVE speed (post-cap) plus the request + cap so the dev
	// top-strip can show "asked N×, running M×" when a crank was clamped.
	json.NewEncoder(w).Encode(map[string]any{
		"ok":              true,
		"speed":           sc.Speed(),
		"requested_speed": sc.RequestedSpeed(),
		"max_speed":       sc.MaxSpeed(),
	})
}
