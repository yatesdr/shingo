package www

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"shingoedge/config"
)

func (h *Handlers) handleTraffic(w http.ResponseWriter, r *http.Request) {
	cfg := h.engine.AppConfig()
	mgr := h.engine.PLCManager()

	plcNames := mgr.PLCNames()
	anomalies, rpMap := loadAnomalyData(h)

	cfg.Lock()
	bindings := make([]map[string]interface{}, 0, len(cfg.CountGroups.Bindings))
	for name, b := range cfg.CountGroups.Bindings {
		bindings = append(bindings, map[string]interface{}{
			"Name":       name,
			"PLC":        b.PLC,
			"RequestTag": b.RequestTag,
		})
	}
	heartbeatTag := cfg.CountGroups.HeartbeatTag
	heartbeatPLC := cfg.CountGroups.HeartbeatPLC
	cfg.Unlock()

	data := map[string]interface{}{
		"Page":              "traffic",
		"Bindings":          bindings,
		"HeartbeatTag":      heartbeatTag,
		"HeartbeatPLC":      heartbeatPLC,
		"PLCNames":          plcNames,
		"Anomalies":         anomalies,
		"ReportingPointMap": rpMap,
	}
	h.renderTemplate(w, r, "traffic.html", data)
}

// apiTrafficBindings returns the current count-group bindings as JSON.
func (h *Handlers) apiTrafficBindings(w http.ResponseWriter, r *http.Request) {
	cfg := h.engine.AppConfig()
	cfg.Lock()
	out := struct {
		HeartbeatTag string                    `json:"heartbeat_tag"`
		HeartbeatPLC string                    `json:"heartbeat_plc"`
		Bindings     map[string]config.Binding `json:"bindings"`
	}{
		HeartbeatTag: cfg.CountGroups.HeartbeatTag,
		HeartbeatPLC: cfg.CountGroups.HeartbeatPLC,
		Bindings:     cfg.CountGroups.Bindings,
	}
	cfg.Unlock()
	writeJSON(w, out)
}

// apiTrafficSaveHeartbeat saves the heartbeat tag + PLC config.
func (h *Handlers) apiTrafficSaveHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		HeartbeatTag string `json:"heartbeat_tag"`
		HeartbeatPLC string `json:"heartbeat_plc"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cfg := h.engine.AppConfig()
	cfg.Lock()
	cfg.CountGroups.HeartbeatTag = strings.TrimSpace(req.HeartbeatTag)
	cfg.CountGroups.HeartbeatPLC = strings.TrimSpace(req.HeartbeatPLC)
	cfg.Unlock()

	if err := cfg.Save(h.engine.ConfigPath()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	log.Printf("traffic: heartbeat config saved (tag=%s, plc=%s)", req.HeartbeatTag, req.HeartbeatPLC)
	writeJSON(w, map[string]string{"status": "ok"})
}

// apiTrafficAddBinding adds a new group binding.
func (h *Handlers) apiTrafficAddBinding(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		PLC        string `json:"plc"`
		RequestTag string `json:"request_tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.PLC = strings.TrimSpace(req.PLC)
	req.RequestTag = strings.TrimSpace(req.RequestTag)

	if req.Name == "" || req.PLC == "" || req.RequestTag == "" {
		writeError(w, http.StatusBadRequest, "name, plc, and request_tag are required")
		return
	}

	cfg := h.engine.AppConfig()
	cfg.Lock()
	if cfg.CountGroups.Bindings == nil {
		cfg.CountGroups.Bindings = make(map[string]config.Binding)
	}
	if _, exists := cfg.CountGroups.Bindings[req.Name]; exists {
		cfg.Unlock()
		writeError(w, http.StatusConflict, "binding already exists for "+req.Name)
		return
	}
	cfg.CountGroups.Bindings[req.Name] = config.Binding{
		PLC:        req.PLC,
		RequestTag: req.RequestTag,
	}
	cfg.Unlock()

	if err := cfg.Save(h.engine.ConfigPath()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	log.Printf("traffic: added binding %q → %s/%s", req.Name, req.PLC, req.RequestTag)
	writeJSON(w, map[string]string{"status": "ok"})
}

// apiTrafficDeleteBinding removes a group binding by name.
func (h *Handlers) apiTrafficDeleteBinding(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cfg := h.engine.AppConfig()
	cfg.Lock()
	delete(cfg.CountGroups.Bindings, req.Name)
	cfg.Unlock()

	if err := cfg.Save(h.engine.ConfigPath()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	log.Printf("traffic: deleted binding %q", req.Name)
	writeJSON(w, map[string]string{"status": "ok"})
}
