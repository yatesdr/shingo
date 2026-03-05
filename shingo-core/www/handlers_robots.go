package www

import (
	"log"
	"net/http"

	"shingocore/fleet"
)

func (h *Handlers) handleRobots(w http.ResponseWriter, r *http.Request) {
	var robots []fleet.RobotStatus
	if rl, ok := h.engine.Fleet().(fleet.RobotLister); ok {
		var err error
		robots, err = rl.GetRobotsStatus()
		if err != nil {
			log.Printf("robots: fleet error: %v", err)
		}
	}
	data := map[string]any{
		"Page":          "robots",
		"Robots": robots,
	}
	h.render(w, r, "robots.html", data)
}

func (h *Handlers) apiRobotsStatus(w http.ResponseWriter, r *http.Request) {
	rl, ok := h.engine.Fleet().(fleet.RobotLister)
	if !ok {
		h.jsonOK(w, []any{})
		return
	}
	robots, err := rl.GetRobotsStatus()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, robots)
}

func (h *Handlers) apiRobotSetAvailability(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VehicleID string `json:"vehicle_id"`
		Available bool   `json:"available"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	rl, ok := h.engine.Fleet().(fleet.RobotLister)
	if !ok {
		h.jsonError(w, "fleet backend does not support robot management", http.StatusNotImplemented)
		return
	}
	if err := rl.SetAvailability(req.VehicleID, req.Available); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiRobotRetryFailed(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VehicleID string `json:"vehicle_id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	rl, ok := h.engine.Fleet().(fleet.RobotLister)
	if !ok {
		h.jsonError(w, "fleet backend does not support robot management", http.StatusNotImplemented)
		return
	}
	if err := rl.RetryFailed(req.VehicleID); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiRobotForceComplete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VehicleID string `json:"vehicle_id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	rl, ok := h.engine.Fleet().(fleet.RobotLister)
	if !ok {
		h.jsonError(w, "fleet backend does not support robot management", http.StatusNotImplemented)
		return
	}
	if err := rl.ForceComplete(req.VehicleID); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}
