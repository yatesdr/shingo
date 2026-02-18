package www

import (
	"encoding/json"
	"log"
	"net/http"

	"warpath/rds"
)

func (h *Handlers) handleRobots(w http.ResponseWriter, r *http.Request) {
	robots, err := h.engine.RDSClient().GetRobotsStatus()
	if err != nil {
		log.Printf("robots: RDS error: %v", err)
	}
	data := map[string]any{
		"Page":          "robots",
		"Robots":        robots,
		"Authenticated": h.isAuthenticated(r),
	}
	h.render(w, "robots.html", data)
}

func (h *Handlers) apiRobotSetDispatchable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VehicleID string `json:"vehicle_id"`
		Type      string `json:"type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if err := h.engine.RDSClient().SetDispatchable(&rds.DispatchableRequest{
		Vehicles: []string{req.VehicleID},
		Type:     req.Type,
	}); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiRobotRedoFailed(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VehicleID string `json:"vehicle_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if err := h.engine.RDSClient().RedoFailed(&rds.RedoFailedRequest{
		Vehicles: []string{req.VehicleID},
	}); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiRobotManualFinish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VehicleID string `json:"vehicle_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if err := h.engine.RDSClient().ManualFinish(&rds.ManualFinishRequest{
		Vehicles: []string{req.VehicleID},
	}); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, map[string]string{"status": "ok"})
}
