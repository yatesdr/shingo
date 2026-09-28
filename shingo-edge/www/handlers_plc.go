// handlers_plc.go — PLC / WarLink admin endpoints and reporting-point
// CRUD. Reporting points are PLC-bound (plc_name + tag_name) and their
// add/remove tag-publishing cascade goes through the same WarLink
// machinery, so they group with PLC rather than living elsewhere.

package www

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// --- PLC / WarLink ---

func (h *Handlers) apiListPLCs(w http.ResponseWriter, r *http.Request) {
	mgr := h.engine.PLCManager()
	type plcInfo struct {
		Name      string `json:"name"`
		Status    string `json:"status"`
		Connected bool   `json:"connected"`
	}
	names := mgr.PLCNames()
	result := make([]plcInfo, len(names))
	for i, name := range names {
		mp := mgr.GetPLC(name)
		status := "Unknown"
		if mp != nil {
			status = mp.Status
		}
		result[i] = plcInfo{Name: name, Status: status, Connected: mgr.IsConnected(name)}
	}
	writeJSON(w, result)
}

func (h *Handlers) apiWarLinkStatus(w http.ResponseWriter, r *http.Request) {
	mgr := h.engine.PLCManager()
	cfg := h.engine.AppConfig()
	errStr := ""
	if err := mgr.WarLinkError(); err != nil {
		errStr = err.Error()
	}
	mode := cfg.WarLink.Mode
	if mode == "" {
		mode = "sse"
	}
	writeJSON(w, map[string]any{
		"connected": mgr.IsWarLinkConnected(),
		"host":      cfg.WarLink.Host,
		"port":      cfg.WarLink.Port,
		"enabled":   cfg.WarLink.Enabled,
		"mode":      mode,
		"error":     errStr,
	})
}

func (h *Handlers) apiUpdateWarLink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		PollRate string `json:"poll_rate"`
		Enabled  bool   `json:"enabled"`
		Mode     string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.Mode != "" && req.Mode != "poll" && req.Mode != "sse" {
		writeError(w, http.StatusBadRequest, "mode must be \"poll\" or \"sse\"")
		return
	}

	cfg := h.engine.AppConfig()
	cfg.Lock()
	if req.Host != "" {
		cfg.WarLink.Host = req.Host
	}
	if req.Port > 0 {
		cfg.WarLink.Port = req.Port
	}
	if req.PollRate != "" {
		d, err := time.ParseDuration(req.PollRate)
		if err != nil {
			cfg.Unlock()
			writeError(w, http.StatusBadRequest, "invalid poll_rate: "+err.Error())
			return
		}
		cfg.WarLink.PollRate = d
	}
	cfg.WarLink.Enabled = req.Enabled
	if req.Mode != "" {
		cfg.WarLink.Mode = req.Mode
	}
	cfg.Unlock()

	if err := cfg.Save(h.engine.ConfigPath()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.orchestration.ApplyWarLinkConfig()

	h.requestBackup("warlink-config")
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiPLCTags(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	tags, err := h.engine.PLCManager().DiscoverTags(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, tags)
}

func (h *Handlers) apiPLCAllTags(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	tags, err := h.engine.PLCManager().FetchAllTags(ctx, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, tags)
}

func (h *Handlers) apiReadTag(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PLCName string `json:"plc_name"`
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	val, err := h.engine.PLCManager().ReadTag(req.PLCName, req.TagName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"value": val})
}
