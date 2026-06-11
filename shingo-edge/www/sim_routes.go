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
// toggle) on the edge — the edge owns its own SimClock, which paces the fake
// PLC counters, so the dev top-strip changes both core and edge speed. Compiled
// only into -tags sim builds; the non-sim stub is a no-op.
func (h *Handlers) registerSimRoutes(r chi.Router) {
	r.Get("/sim/status", h.apiSimStatus)
	r.Post("/sim/speed", h.apiSimSetSpeed)
}

// apiSimStatus reports the edge sim-clock speed + simulated time.
func (h *Handlers) apiSimStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"sim": true, "has_clock": false}
	if sc := clock.AsSimClock(); sc != nil {
		resp["has_clock"] = true
		resp["speed"] = sc.Speed()
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
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "speed": sc.Speed()})
}
