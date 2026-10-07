package www

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"shingoedge/config"
)

func (h *Handlers) requestBackup(reason string) {
	if h.backup == nil {
		return
	}
	h.backup.RequestBackup(reason)
}

func (h *Handlers) apiBackupStatus(w http.ResponseWriter, r *http.Request) {
	if h.backup == nil {
		writeError(w, http.StatusNotImplemented, "backup service unavailable")
		return
	}
	writeJSON(w, h.backup.Status())
}

func (h *Handlers) apiListBackups(w http.ResponseWriter, r *http.Request) {
	if h.backup == nil {
		writeError(w, http.StatusNotImplemented, "backup service unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	items, err := h.backup.ListBackups(ctx)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, items)
}

// apiTestBackupConfig tests the posted storage settings (Test connection on the
// Configuration page). The secret is never rendered (E8): a blank posted secret
// is the saved one, so the page can test what a save would store.
func (h *Handlers) apiTestBackupConfig(w http.ResponseWriter, r *http.Request) {
	if h.backup == nil {
		writeError(w, http.StatusNotImplemented, "backup service unavailable")
		return
	}
	var req config.BackupS3Config
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.SecretKey) == "" {
		cfg := h.engine.AppConfig()
		cfg.RLock()
		req.SecretKey = cfg.Backup.S3.SecretKey
		cfg.RUnlock()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := h.backup.TestConfig(ctx, req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiRunBackup(w http.ResponseWriter, r *http.Request) {
	if h.backup == nil {
		writeError(w, http.StatusNotImplemented, "backup service unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if err := h.backup.RunNow(ctx, "manual"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiStageBackupRestore(w http.ResponseWriter, r *http.Request) {
	if h.backup == nil {
		writeError(w, http.StatusNotImplemented, "backup service unavailable")
		return
	}
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Key) == "" {
		writeError(w, http.StatusBadRequest, "backup key is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if strings.TrimSpace(h.engine.AppConfig().StationID()) == "" {
		writeError(w, http.StatusBadRequest, "station ID must be configured before staging a restore")
		return
	}
	if err := h.backup.StageRestore(ctx, strings.TrimSpace(req.Key)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"status":           "ok",
		"restart_required": true,
	})
}
