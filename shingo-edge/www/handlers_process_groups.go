// handlers_process_groups.go — process group CRUD for the Processes
// admin page sidebar. Groups are pure UI taxonomy; nothing in the
// runtime reads group_id.

package www

import (
	"encoding/json"
	"net/http"
)

// apiListProcessGroups returns every process_group row.
func (h *Handlers) apiListProcessGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := h.engine.ProcessService().ListGroups()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, groups)
}

// apiCreateProcessGroup creates a new process_group.
func (h *Handlers) apiCreateProcessGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	id, err := h.engine.ProcessService().CreateGroup(req.Name, req.Description)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("process-group-created")
	writeJSON(w, map[string]int64{"id": id})
}

// apiUpdateProcessGroup modifies a process_group's name and description.
func (h *Handlers) apiUpdateProcessGroup(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := h.engine.ProcessService().UpdateGroup(id, req.Name, req.Description); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("process-group-updated")
	writeJSON(w, map[string]string{"status": "ok"})
}

// apiDeleteProcessGroup removes a process_group. Member processes revert
// to Ungrouped via the ON DELETE SET NULL FK.
func (h *Handlers) apiDeleteProcessGroup(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	if err := h.engine.ProcessService().DeleteGroup(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("process-group-deleted")
	writeJSON(w, map[string]string{"status": "ok"})
}

// apiCountProcessGroupMembers returns how many processes are in a group.
// Used by the delete-confirm dialog.
func (h *Handlers) apiCountProcessGroupMembers(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	count, err := h.engine.ProcessService().CountGroupMembers(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]int{"count": count})
}

// apiSetProcessGroup assigns a process to a group (or ungroups it when
// group_id is null/0). Lightweight endpoint so the group modal can
// assign multiple processes without sending the full process update
// payload for each one.
func (h *Handlers) apiSetProcessGroup(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process ID")
		return
	}
	var req struct {
		GroupID *int64 `json:"group_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var gid *int64
	if req.GroupID != nil && *req.GroupID > 0 {
		gid = req.GroupID
	}
	if err := h.engine.ProcessService().SetGroupID(processID, gid); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("process-group-assigned")
	writeJSON(w, map[string]string{"status": "ok"})
}
