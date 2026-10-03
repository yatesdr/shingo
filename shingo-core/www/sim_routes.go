//go:build sim

package www

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/protocol/clock"
	"shingocore/fleet/simulator"
)

// registerSimRoutes adds the dev-only sim control endpoints (the live speed
// toggle). Compiled only into -tags sim builds; the non-sim stub
// (sim_routes_stub.go) is a no-op, so production never exposes these.
func (h *Handlers) registerSimRoutes(r chi.Router) {
	r.Get("/sim/status", h.apiSimStatus)
	r.Post("/sim/speed", h.apiSimSetSpeed)

	// Fault injection for scripted scenarios (the cancel-mid-carry return
	// above all): read the fleet's deck/fault state, force a deck, fault a
	// robot, make cancels refuse. All query-string driven so a curl one-liner
	// or a no-preflight browser POST can drive them.
	r.Get("/sim/robots", h.apiSimRobots)
	r.Post("/sim/robot/fault", h.apiSimRobotFault)
	r.Post("/sim/robot/deck", h.apiSimRobotDeck)
	r.Post("/sim/cancel-fails", h.apiSimCancelFails)
}

// simFleetControl is the slice of the simulator backend the injection routes
// drive. An interface rather than the concrete type so a test can hand the
// routes a fake; the engine's fleet backend satisfies it only in a sim build
// running the simulator, and the routes answer 503 otherwise.
type simFleetControl interface {
	SimRobots() ([]simulator.FleetRobot, error)
	SetRobotFault(id string, on bool) error
	SetRobotDeck(id string, loaded bool) error
	SetCancelFails(on bool) error
	CancelFails() bool
}

// simRobotJSON is one robot as a driver script reads it.
type simRobotJSON struct {
	ID         string `json:"id"`
	Busy       bool   `json:"busy"`
	At         string `json:"at"`
	DeckLoaded bool   `json:"deck_loaded"`
	Fault      bool   `json:"fault"`
}

func toSimRobotJSON(r simulator.FleetRobot) simRobotJSON {
	return simRobotJSON{ID: r.ID, Busy: r.Busy, At: r.At, DeckLoaded: r.Loaded, Fault: r.Fault}
}

// simControl resolves the simulator behind the engine, writing the 503 itself
// when there is none.
func (h *Handlers) simControl(w http.ResponseWriter) (simFleetControl, bool) {
	sc, ok := h.engine.Fleet().(simFleetControl)
	if !ok {
		http.Error(w, "fleet backend is not the simulator", http.StatusServiceUnavailable)
		return nil, false
	}
	return sc, true
}

// simBoolParam reads a required boolean query parameter ("1"/"0", "true"/
// "false"), writing the 400 itself when it is missing or unreadable.
func simBoolParam(w http.ResponseWriter, r *http.Request, name string) (bool, bool) {
	v, err := strconv.ParseBool(r.URL.Query().Get(name))
	if err != nil {
		http.Error(w, name+" must be 1 or 0", http.StatusBadRequest)
		return false, false
	}
	return v, true
}

func writeSimJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// apiSimRobots reports every robot in the sim fleet plus the cancel-refusal
// toggle, so a driver script can wait on state (a deck going loaded is how it
// sees a pickup complete). Busy is as of the driver's last tick (one simulated
// second); deck, position and fault are live.
func (h *Handlers) apiSimRobots(w http.ResponseWriter, r *http.Request) {
	sc, ok := h.simControl(w)
	if !ok {
		return
	}
	fleetRobots, err := sc.SimRobots()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	out := make([]simRobotJSON, 0, len(fleetRobots))
	for _, fr := range fleetRobots {
		out = append(out, toSimRobotJSON(fr))
	}
	writeSimJSON(w, map[string]any{"robots": out, "cancel_fails": sc.CancelFails()})
}

// apiSimRobotFault: POST /api/sim/robot/fault?id=AMR-03&on=1|0.
func (h *Handlers) apiSimRobotFault(w http.ResponseWriter, r *http.Request) {
	sc, ok := h.simControl(w)
	if !ok {
		return
	}
	id := r.URL.Query().Get("id")
	on, ok := simBoolParam(w, r, "on")
	if !ok {
		return
	}
	if err := sc.SetRobotFault(id, on); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("[sim] api: robot %s fault set to %v", id, on)
	writeSimJSON(w, map[string]any{"ok": true, "id": id, "fault": on})
}

// apiSimRobotDeck: POST /api/sim/robot/deck?id=AMR-03&loaded=1|0.
func (h *Handlers) apiSimRobotDeck(w http.ResponseWriter, r *http.Request) {
	sc, ok := h.simControl(w)
	if !ok {
		return
	}
	id := r.URL.Query().Get("id")
	loaded, ok := simBoolParam(w, r, "loaded")
	if !ok {
		return
	}
	if err := sc.SetRobotDeck(id, loaded); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("[sim] api: robot %s deck forced loaded=%v", id, loaded)
	writeSimJSON(w, map[string]any{"ok": true, "id": id, "deck_loaded": loaded})
}

// apiSimCancelFails: POST /api/sim/cancel-fails?on=1|0. While on, every fleet
// cancel is refused and the order keeps running on its robot.
func (h *Handlers) apiSimCancelFails(w http.ResponseWriter, r *http.Request) {
	sc, ok := h.simControl(w)
	if !ok {
		return
	}
	on, ok := simBoolParam(w, r, "on")
	if !ok {
		return
	}
	if err := sc.SetCancelFails(on); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	log.Printf("[sim] api: cancel-fails set to %v", on)
	writeSimJSON(w, map[string]any{"ok": true, "cancel_fails": on})
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
