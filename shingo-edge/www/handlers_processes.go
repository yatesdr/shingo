// handlers_processes.go — process CRUD plus the active-style flip and
// per-process style list. SetActiveStyle lives here (rather than with
// styles) because it mutates the process row.

package www

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"shingoedge/domain"
)

// --- Processes Admin ---

// processListRow is the list's row shape: the process plus its DERIVED
// quality-containment state. Embedded, so the JSON stays flat and every
// existing consumer reads it unchanged. The state is derived from the
// claims, not stored on the process — the claim is the one source of truth
// and the divert reads it; these two fields are the settings draft's read.
type processListRow struct {
	domain.Process
	QualityHoldEnabled     bool   `json:"quality_hold_enabled"`
	QualityHoldDestination string `json:"quality_hold_destination"`
}

func (h *Handlers) apiListProcesses(w http.ResponseWriter, r *http.Request) {
	processes, err := h.engine.ProcessService().List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	states, err := h.engine.ProcessService().ContainmentStates()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows := make([]processListRow, len(processes))
	for i, p := range processes {
		rows[i] = processListRow{Process: p}
		if dest, ok := states[p.ID]; ok {
			rows[i].QualityHoldEnabled = true
			rows[i].QualityHoldDestination = dest
		}
	}
	writeJSON(w, rows)
}

// apiProcessContainmentSetting is the settings toggle's write: stamp or clear
// the containment destination on every live style's produce claims of the
// process. The claim stays the storage (the divert reads it); this endpoint
// is the batch editor. Fires the coalesced Core spec sync on any change.
func (h *Handlers) apiProcessContainmentSetting(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	var req struct {
		Enabled     bool   `json:"enabled"`
		Destination string `json:"destination"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Enabled && strings.TrimSpace(req.Destination) == "" {
		writeError(w, http.StatusBadRequest, "a containment destination is required when the hold is enabled")
		return
	}
	by := ""
	if u, ok := h.sessions.getUser(r); ok {
		by = u
	}
	if by == "" {
		by = "admin"
	}
	if err := h.engine.ProcessService().SetContainment(processID, req.Enabled, req.Destination, by); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.requestSpecChangePublish(processID)
	writeJSON(w, map[string]bool{"ok": true})
}

func (h *Handlers) apiCreateProcess(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name              string `json:"name"`
		Description       string `json:"description"`
		ProductionState   string `json:"production_state"`
		CounterPLCName    string `json:"counter_plc_name"`
		CounterTagName    string `json:"counter_tag_name"`
		CounterEnabled    bool   `json:"counter_enabled"`
		ChangeoverAutoArm string `json:"changeover_auto_arm"`
		GroupID           *int64 `json:"group_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	// Validate group_id before any writes — a stale modal can carry a
	// group_id whose group was deleted in another tab, and with FKs off
	// the SetGroupID call would otherwise write an orphan that the
	// sidebar renderer silently drops.
	if req.GroupID != nil && *req.GroupID > 0 {
		if err := h.validateGroupID(req.GroupID); err != nil {
			if errors.Is(err, errUnknownGroupID) {
				writeError(w, http.StatusBadRequest, err.Error())
			} else {
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
	}
	id, err := h.engine.ProcessService().Create(req.Name, req.Description, req.ProductionState, req.CounterPLCName, req.CounterTagName, req.CounterEnabled)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Persist the 3-value CATID auto-arm mode (default 'auto'). Separate setter so
	// the mode threads through without expanding the Create positional signature.
	if err := h.engine.ProcessService().SetChangeoverAutoArm(id, req.ChangeoverAutoArm); err != nil {
		log.Printf("set changeover_auto_arm on new process %d: %v", id, err)
	}
	// Assign to a group if one was selected (optional — nil = Ungrouped).
	if req.GroupID != nil && *req.GroupID > 0 {
		if err := h.engine.ProcessService().SetGroupID(id, req.GroupID); err != nil {
			log.Printf("set group_id on new process %d: %v", id, err)
		}
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
		Name              string `json:"name"`
		Description       string `json:"description"`
		ProductionState   string `json:"production_state"`
		CounterPLCName    string `json:"counter_plc_name"`
		CounterTagName    string `json:"counter_tag_name"`
		CounterEnabled    bool   `json:"counter_enabled"`
		ChangeoverAutoArm string `json:"changeover_auto_arm"`
		GroupID           *int64 `json:"group_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	// Validate group_id before any writes — see apiCreateProcess for the
	// orphan-id rationale.
	var gid *int64
	if req.GroupID != nil && *req.GroupID > 0 {
		gid = req.GroupID
	}
	if err := h.validateGroupID(gid); err != nil {
		if errors.Is(err, errUnknownGroupID) {
			writeError(w, http.StatusBadRequest, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	if err := h.engine.ProcessService().Update(id, req.Name, req.Description, req.ProductionState, req.CounterPLCName, req.CounterTagName, req.CounterEnabled); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Persist the 3-value CATID auto-arm mode alongside the counter-config edit.
	if err := h.engine.ProcessService().SetChangeoverAutoArm(id, req.ChangeoverAutoArm); err != nil {
		log.Printf("set changeover_auto_arm on process %d: %v", id, err)
	}
	// Update group assignment. req.GroupID == nil means "Ungrouped" (clear
	// the FK); a positive value means "assign to that group". We always write
	// so the user can ungroup via the General tab dropdown.
	if err := h.engine.ProcessService().SetGroupID(id, gid); err != nil {
		log.Printf("set group_id on process %d: %v", id, err)
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
	// h.orchestration, not the process service: deleting a process has to close
	// the demand episodes it owns before the row goes, or Core keeps them open
	// forever. That composition lives on the engine (engine/process_delete.go)
	// because the close writer does, and this handler does not orchestrate it —
	// it calls the one verb.
	if err := h.orchestration.DeleteProcess(id); err != nil {
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
	if err := h.orchestration.SetProcessActiveStyle(id, req.StyleID); err != nil {
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
