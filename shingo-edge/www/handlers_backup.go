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

func (h *Handlers) apiUpdateBackupConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled               bool   `json:"enabled"`
		ScheduleInterval      string `json:"schedule_interval"`
		KeepHourly            int    `json:"keep_hourly"`
		KeepDaily             int    `json:"keep_daily"`
		KeepWeekly            int    `json:"keep_weekly"`
		KeepMonthly           int    `json:"keep_monthly"`
		Endpoint              string `json:"endpoint"`
		Bucket                string `json:"bucket"`
		Region                string `json:"region"`
		AccessKey             string `json:"access_key"`
		SecretKey             string `json:"secret_key"`
		UsePathStyle          bool   `json:"use_path_style"`
		InsecureSkipTLSVerify bool   `json:"insecure_skip_tls_verify"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	interval := time.Hour
	if strings.TrimSpace(req.ScheduleInterval) != "" {
		d, err := time.ParseDuration(req.ScheduleInterval)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid schedule_interval: "+err.Error())
			return
		}
		interval = d
	}
	if req.Enabled {
		if strings.TrimSpace(req.Endpoint) == "" || strings.TrimSpace(req.Bucket) == "" || strings.TrimSpace(req.AccessKey) == "" || strings.TrimSpace(req.SecretKey) == "" {
			writeError(w, http.StatusBadRequest, "endpoint, bucket, access key, and secret key are required to enable automatic backups")
			return
		}
		if interval <= 0 {
			writeError(w, http.StatusBadRequest, "schedule_interval must be greater than zero when automatic backups are enabled")
			return
		}
	}

	cfg := h.engine.AppConfig()
	cfg.Lock()
	cfg.Backup.Enabled = req.Enabled
	cfg.Backup.ScheduleInterval = interval
	cfg.Backup.KeepHourly = req.KeepHourly
	cfg.Backup.KeepDaily = req.KeepDaily
	cfg.Backup.KeepWeekly = req.KeepWeekly
	cfg.Backup.KeepMonthly = req.KeepMonthly
	cfg.Backup.S3.Endpoint = strings.TrimSpace(req.Endpoint)
	cfg.Backup.S3.Bucket = strings.TrimSpace(req.Bucket)
	cfg.Backup.S3.Region = strings.TrimSpace(req.Region)
	cfg.Backup.S3.AccessKey = strings.TrimSpace(req.AccessKey)
	cfg.Backup.S3.SecretKey = strings.TrimSpace(req.SecretKey)
	cfg.Backup.S3.UsePathStyle = req.UsePathStyle
	cfg.Backup.S3.InsecureSkipTLSVerify = req.InsecureSkipTLSVerify
	cfg.Unlock()

	if err := cfg.Save(h.engine.ConfigPath()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("backup-config-updated")
	writeJSON(w, map[string]string{"status": "ok"})
}

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
