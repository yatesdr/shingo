package www

import (
	"fmt"
	"log"
	"net/http"

	"shingocore/fleet"
)

// apiFireAlarmStatus returns the current fire alarm state from RDS.
//
//	GET /api/fire-alarm/status  (protected — requires auth)
//	Response: {"is_fire": bool, "changed_at": string}
func (h *Handlers) apiFireAlarmStatus(w http.ResponseWriter, r *http.Request) {
	if !h.engine.AppConfig().FireAlarm.Enabled {
		h.jsonError(w, "fire alarm feature is disabled", http.StatusNotFound)
		return
	}

	fc, ok := h.engine.Fleet().(fleet.FireAlarmController)
	if !ok {
		h.jsonError(w, "fleet backend does not support fire alarm", http.StatusNotImplemented)
		return
	}

	status, err := fc.GetFireAlarmStatus()
	if err != nil {
		h.jsonError(w, "failed to query fire alarm status: "+err.Error(), http.StatusBadGateway)
		return
	}

	h.jsonOK(w, map[string]any{
		"is_fire":    status.IsFire,
		"changed_at": status.ChangedAt,
	})
}

// apiFireAlarmTrigger activates or clears the fire alarm via RDS.
//
//	POST /api/fire-alarm/trigger  (protected — requires auth)
//	Body: {"on": bool, "autoResume": bool}
//	Response: {"status": "ok"}
func (h *Handlers) apiFireAlarmTrigger(w http.ResponseWriter, r *http.Request) {
	if !h.engine.AppConfig().FireAlarm.Enabled {
		h.jsonError(w, "fire alarm feature is disabled", http.StatusNotFound)
		return
	}

	var req struct {
		On         bool `json:"on"`
		AutoResume bool `json:"autoResume"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	fc, ok := h.engine.Fleet().(fleet.FireAlarmController)
	if !ok {
		h.jsonError(w, "fleet backend does not support fire alarm", http.StatusNotImplemented)
		return
	}

	actor := h.getUsername(r)
	if actor == "" {
		actor = "ui"
	}

	if err := fc.SetFireAlarm(req.On, req.AutoResume); err != nil {
		h.jsonError(w, "failed to set fire alarm: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Audit trail
	action := "cleared"
	if req.On {
		action = "activated"
	}
	detail := fmt.Sprintf("on=%v autoResume=%v", req.On, req.AutoResume)

	if err := h.engine.DB().AppendAudit("firealarm", 0, action, "", detail, actor); err != nil {
		log.Printf("fire-alarm: audit write failed: %v", err)
		// Non-fatal — don't fail the request over an audit error
	}

	// SSE broadcast so other admin browsers update in real-time
	h.eventHub.Broadcast("fire-alarm", sseJSON(map[string]any{
		"is_fire": req.On,
		"actor":   actor,
	}))

	log.Printf("fire-alarm: %s by %s (autoResume=%v)", action, actor, req.AutoResume)
	h.jsonSuccess(w)
}
