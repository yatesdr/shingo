// handlers_system_config.go — system-level config endpoints: Core API
// URL, messaging/Kafka, station ID, auto-confirm flag, change-password.
// All of these mutate the persisted config file via cfg.Save and the
// auth-side flow uses the protocol/auth package.

package www

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"shingo/protocol/auth"
)

// --- Core API ---

func (h *Handlers) apiUpdateCoreAPI(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CoreAPI string `json:"core_api"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := h.engine.AppConfig()
	cfg.Lock()
	cfg.CoreAPI = req.CoreAPI
	cfg.Unlock()
	if err := cfg.Save(h.engine.ConfigPath()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

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

// --- Config Admin ---

func (h *Handlers) apiUpdateMessaging(w http.ResponseWriter, r *http.Request) {
	var req struct {
		KafkaBrokers []string `json:"kafka_brokers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cfg := h.engine.AppConfig()
	cfg.Lock()
	cfg.Messaging.Kafka.Brokers = req.KafkaBrokers
	cfg.Unlock()

	if err := cfg.Save(h.engine.ConfigPath()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := h.orchestration.ReconnectKafka(); err != nil {
		log.Printf("kafka reconnect after config update: %v", err)
	}

	h.requestBackup("messaging-config")
	writeJSON(w, map[string]string{"status": "ok"})
}

// apiUpdateStationID writes this edge's identity into shingoedge.yaml.
//
// IT TAKES EFFECT ON RESTART, AND SAYING SO IS THE FIX. This endpoint used to
// return a bare {"status":"ok"} while the running station id stayed exactly
// where it was — captured once at main.go's identity block and closed over by
// the Kafka ingest filter. Its sibling apiUpdateMessaging calls ReconnectKafka;
// this one cannot, because the station id is not one connection, it is the
// ingest filter, the envelope source address, the consumer group and the backup
// manifest. Rewiring all of those live is a larger change than telling the
// truth, so it tells the truth.
//
// THE OTHER HALF OF THE OLD DEFECT IS ALREADY GONE. Saving used to persist the
// derived Kafka group id alongside the station id — KafkaConfig.GroupID carried
// a yaml tag and Save marshals the whole struct — which pinned the consumer
// group to the OLD station id forever. Renaming through this endpoint therefore
// made the "one edge is deaf" condition permanent rather than fixing it. The
// field is `yaml:"-"` now, so no Save can write it and no config can override
// the derivation. See config.KafkaConfig.GroupID.
//
// station_uid is the enrolled identity Core minted. station_id is the legacy
// override and is accepted for the migration window only.
func (h *Handlers) apiUpdateStationID(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StationUID string `json:"station_uid"`
		StationID  string `json:"station_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cfg := h.engine.AppConfig()
	cfg.Lock()
	if req.StationUID != "" {
		cfg.StationUID = req.StationUID
	}
	if req.StationID != "" {
		cfg.Messaging.StationID = req.StationID
	}
	cfg.Unlock()

	if err := cfg.Save(h.engine.ConfigPath()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("station-id")
	writeJSON(w, map[string]string{
		"status": "ok",
		"note":   "written to shingoedge.yaml — RESTART shingoedge for it to take effect",
	})
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

func (h *Handlers) apiUpdateAutoConfirm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AutoConfirm bool `json:"auto_confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cfg := h.engine.AppConfig()
	cfg.Lock()
	cfg.Web.AutoConfirm = req.AutoConfirm
	cfg.Unlock()

	if err := cfg.Save(h.engine.ConfigPath()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("auto-confirm")
	writeJSON(w, map[string]string{"status": "ok"})
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
