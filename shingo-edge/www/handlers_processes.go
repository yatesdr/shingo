// handlers_processes.go — process CRUD plus the active-style flip and
// per-process style list. SetActiveStyle lives here (rather than with
// styles) because it mutates the process row.

package www

import (
	"encoding/json"
	"log"
	"net/http"
)

// --- Processes Admin ---

func (h *Handlers) apiListProcesses(w http.ResponseWriter, r *http.Request) {
	processes, err := h.engine.ProcessService().List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, processes)
}

func (h *Handlers) apiCreateProcess(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name               string `json:"name"`
		Description        string `json:"description"`
		ProductionState    string `json:"production_state"`
		CounterPLCName     string `json:"counter_plc_name"`
		CounterTagName     string `json:"counter_tag_name"`
		CounterEnabled     bool   `json:"counter_enabled"`
		AutoCutoverEnabled bool   `json:"auto_cutover_enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	id, err := h.engine.ProcessService().Create(req.Name, req.Description, req.ProductionState, req.CounterPLCName, req.CounterTagName, req.CounterEnabled, req.AutoCutoverEnabled)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("process-created")
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdateProcess(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Name               string `json:"name"`
		Description        string `json:"description"`
		ProductionState    string `json:"production_state"`
		CounterPLCName     string `json:"counter_plc_name"`
		CounterTagName     string `json:"counter_tag_name"`
		CounterEnabled     bool   `json:"counter_enabled"`
		AutoCutoverEnabled bool   `json:"auto_cutover_enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.ProcessService().Update(id, req.Name, req.Description, req.ProductionState, req.CounterPLCName, req.CounterTagName, req.CounterEnabled, req.AutoCutoverEnabled); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Re-sync the reporting point so the counter config edit takes effect
	// immediately — previously, this only ran on SetActiveStyle, which meant
	// that adding/changing counter fields on an already-active process
	// silently did nothing until the style was re-activated. The sync is a
	// no-op when counter config or active style is missing, so it's safe to
	// call unconditionally. Log-and-continue on error: the process update
	// itself succeeded and we don't want to fail the whole request if the
	// secondary sync hits a transient issue.
	if err := h.orchestration.SyncProcessCounter(id); err != nil {
		log.Printf("sync reporting point after process %d update: %v", id, err)
	}
	h.requestBackup("process-updated")
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeleteProcess(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	if err := h.engine.ProcessService().Delete(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("process-deleted")
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiSetActiveStyle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		StyleID *int64 `json:"style_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.ProcessService().SetActiveStyle(id, req.StyleID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.orchestration.SyncProcessCounter(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("active-style-updated")
	h.eventHub.Broadcast(SSEEvent{Type: "material-refresh", Data: map[string]string{"action": "active-style-changed"}})
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiListProcessStyles(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	styles, err := h.engine.StyleService().ListByProcess(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, styles)
}
