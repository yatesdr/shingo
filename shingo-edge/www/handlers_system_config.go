// handlers_system_config.go — the Configuration page's test doors (Core
// address, a Kafka broker) and the change-password door. The settings
// themselves are saved through the one door, PUT /api/config
// (handlers_config_save.go).

package www

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"shingo/protocol/auth"
)

// --- Core API ---

func (h *Handlers) apiTestCoreAPI(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CoreAPI string `json:"core_api"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.CoreAPI == "" {
		writeJSON(w, map[string]any{"connected": false, "error": "no URL"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, "GET", req.CoreAPI+"/api/health", nil)
	if err != nil {
		writeJSON(w, map[string]any{"connected": false, "error": err.Error()})
		return
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		writeJSON(w, map[string]any{"connected": false, "error": err.Error()})
		return
	}
	resp.Body.Close()
	writeJSON(w, map[string]any{"connected": resp.StatusCode < 500})
}

func (h *Handlers) apiTestKafka(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Broker string `json:"broker"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Broker == "" {
		writeError(w, http.StatusBadRequest, "broker address required")
		return
	}
	conn, err := net.DialTimeout("tcp", req.Broker, 5*time.Second)
	if err != nil {
		writeJSON(w, map[string]any{"connected": false, "error": err.Error()})
		return
	}
	conn.Close()
	writeJSON(w, map[string]any{"connected": true})
}

func (h *Handlers) apiChangePassword(w http.ResponseWriter, r *http.Request) {
	username, ok := h.sessions.getUser(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "not logged in")
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// An empty new password is refused here, not only in the page (U3
	// Account; Core already refuses it).
	if req.NewPassword == "" {
		writeError(w, http.StatusBadRequest, "new password is required")
		return
	}

	user, err := h.engine.AdminService().Get(username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "user not found")
		return
	}

	if !auth.CheckPassword(user.PasswordHash, req.OldPassword) {
		writeError(w, http.StatusBadRequest, "current password is incorrect")
		return
	}

	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}

	if err := h.engine.AdminService().UpdatePassword(username, hash); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to update password: %v", err))
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}
