// handlers_flow_presets.go — the desktop Presets tab's four endpoints.
//
// FOUR, AND APPLY IS NOT ONE OF THEM. A preset is applied to one style at a
// time through the doors that already exist — `flow/preview` with the preset's
// cells, then `flow/save` on that preview's fingerprint — so a preset can
// never change a style without a preview on screen (SYNTH R-L1: explicit
// re-apply with a previewed diff). An apply endpoint would be a second way to
// write claims, and the one that skipped the preview.
//
// Admin-gated with the rest of the process routes: naming a flow is an
// engineer's act on the desktop, and the floor never names one. `created_by`
// comes from the session, never from the body, exactly as the routing set's
// origin does.

package www

import (
	"encoding/json"
	"net/http"
)

func (h *Handlers) apiListFlowPresets(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	view, err := h.engine.ProcessService().PresetsFor(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, view)
}

// apiCreateFlowPreset names a shape: either one style's flow, or one of the
// migration offer's candidate shapes.
//
// The part is stripped by the service before the store sees it, so the store's
// refusal of a payload is a PIN on this path rather than an error a user can
// reach — which is why the tab has no "remove the part" step and no way to
// save a preset that carries one.
func (h *Handlers) apiCreateFlowPreset(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	var req struct {
		Name string `json:"name"`
		// Exactly one of the two. from_style_id names the shape of a style's
		// flow (D1's `Save as preset…`); from_candidate_shape names one of the
		// offered shapes and stamps the styles that run it.
		FromStyleID        int64  `json:"from_style_id"`
		FromCandidateShape string `json:"from_candidate_shape"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "a preset needs a name")
		return
	}
	if (req.FromStyleID == 0) == (req.FromCandidateShape == "") {
		writeError(w, http.StatusBadRequest,
			"name exactly one of from_style_id or from_candidate_shape")
		return
	}
	// THE SESSION IS THE AUTHOR. A body that named one would be a page
	// deciding authorship, the same rule the routing origin and the flow
	// save's source are held to.
	createdBy, _ := h.sessions.getUser(r)

	var newID int64
	if req.FromStyleID != 0 {
		newID, err = h.engine.ProcessService().CreateFromStyle(id, req.FromStyleID, req.Name, createdBy)
	} else {
		newID, err = h.engine.ProcessService().CreateFromCandidate(id, req.Name, req.FromCandidateShape, createdBy)
	}
	if err != nil {
		// The store's validation refusals are the engineer's to read by name:
		// a node outside the routing set is a precondition they can clear.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.requestBackup("flow-preset-created")
	writeJSON(w, map[string]int64{"id": newID})
}

// apiRenameFlowPreset renames every version of a preset's name, and nothing
// else (owner ruling R6). A preset is named once and read for months, and the
// first name an engineer gives a shape is often the one they think better of;
// the alternatives — re-create and archive, or a version n+1 under a new name
// — both leave the list saying something untrue, which is why the menu had no
// Rename until there was an endpoint that meant it.
//
// PATCH, and the body carries a name and nothing else: a route that could also
// move the shape would be a preset edited in place, and a claim's
// source_preset_version would stop meaning what it says.
func (h *Handlers) apiRenameFlowPreset(w http.ResponseWriter, r *http.Request) {
	if _, err := parseID(r, "id"); err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	pid, err := parseID(r, "presetID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid preset id")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "a preset needs a name")
		return
	}
	if err := h.engine.ProcessService().Rename(pid, req.Name); err != nil {
		// A name already in use is the engineer's to read and clear.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.requestBackup("flow-preset-renamed")
	writeJSON(w, map[string]string{"status": "ok"})
}

// apiArchiveFlowPreset hides one version from both surfaces. The row stays and
// so does every member's provenance: a claim points at it, and that reference
// has to keep meaning what it meant.
func (h *Handlers) apiArchiveFlowPreset(w http.ResponseWriter, r *http.Request) {
	if _, err := parseID(r, "id"); err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	pid, err := parseID(r, "presetID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid preset id")
		return
	}
	if err := h.engine.ProcessService().Archive(pid); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("flow-preset-archived")
	writeJSON(w, map[string]string{"status": "ok"})
}
