// handlers_flow.go — the flow composer's endpoints: preview a draft flow,
// save it, both on the shop-floor group beside the changeover routes.
//
// The engine does the work (engine.PreviewFlow / SaveFlow); this file maps a
// request body to a request, an error to a status, and a saved flow to the
// one per-process claims publish the plant-claims mirror needs.

package www

import (
	"encoding/json"
	"errors"
	"net/http"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/engine"
)

// flowPreviewBody is POST /api/processes/{id}/flow/preview.
//
// NO replace_all. A body carries the WHOLE flow for its style — a
// target-style claim absent from cells is a deletion — which is what all six
// producers on this tree already sent. See engine.FlowPreviewRequest for why
// the option was worse than useless: it was the one path on which a preview
// and the save that followed it validated different sets.
type flowPreviewBody struct {
	ToStyleID int64             `json:"to_style_id"`
	Cells     []domain.FlowCell `json:"cells"`
}

// flowSaveBody is POST /api/processes/{id}/flow/save. station_id names the
// operator station saving; the handler resolves its NAME for called_by, the
// client never sends the name.
type flowSaveBody struct {
	ToStyleID   int64             `json:"to_style_id"`
	Cells       []domain.FlowCell `json:"cells"`
	Fingerprint string            `json:"fingerprint"`
	StationID   int64             `json:"station_id"`
	// SourcePresetID / SourcePresetVersion mark this save as an APPLY of a
	// preset (U10). Both or neither. The SHAPE still arrives as cells like any
	// other save — there is no apply endpoint, and this is the only way a
	// preset reaches a style, which is what makes "never a save without its
	// preview" true by construction.
	//
	// These two are provenance, not authorship: `source` and `called_by` stay
	// the handler's (R1). A body naming a preset is saying which shape it
	// applied, not who it is.
	SourcePresetID      int64 `json:"source_preset_id"`
	SourcePresetVersion int   `json:"source_preset_version"`
}

// writeFlowRefusal maps the engine's named refusals to statuses shared by
// preview and save: the seam's gates are 409, a stale fingerprint is
// 409 with stale:true so the station can tell it from the gates.
//
// ErrRunningPositionMove is one of the 409s — it is a conflict with the state
// of the press, like the other two, and the engineer reads it by name because
// it says which position is running and which they were trying to move it to.
// ErrStyleAlreadyRunning stays on the list: owner ruling R3 took it out of the
// composer's save and preview, and planChangeover still raises it for a
// changeover TO the running style, which remains nonsense.
func writeFlowRefusal(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, engine.ErrStyleAlreadyRunning), errors.Is(err, engine.ErrChangeoverActive),
		errors.Is(err, engine.ErrRunningPositionMove):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, engine.ErrFlowStale):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "stale": true})
	default:
		return false
	}
	return true
}

func (h *Handlers) apiPreviewFlow(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	var body flowPreviewBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// ?preflight=1 is the confirm sheet asking Core about the parts — one
	// request, at the step before the robots move. The edit loop's previews
	// (a new one every 400 ms) do not ask, and the desktop never does: it
	// does not read the answer. See engine.flowPreflight.
	//
	// r.Context() travels, so a preview whose operator has left the screen
	// stops at the next phase boundary instead of finishing a plan and a Core
	// call for nobody.
	preview, err := h.orchestration.PreviewFlow(r.Context(), processID, engine.FlowPreviewRequest{
		ToStyleID: body.ToStyleID, Cells: body.Cells,
		Preflight: r.URL.Query().Get("preflight") == "1",
	})
	if err != nil {
		var noOrders *engine.FlowNoOrdersError
		if errors.As(err, &noOrders) && noOrders.Preview != nil {
			// 400 with the named reason — and the preview's findings and
			// unresolved nodes, which are usually why nothing fires.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{
				"error":       err.Error(),
				"order_count": 0,
				"findings":    noOrders.Preview.Findings,
				"unresolved":  noOrders.Preview.Unresolved,
				"preflight":   noOrders.Preview.Preflight,
				"fingerprint": noOrders.Preview.Fingerprint,
			})
			return
		}
		if !writeFlowRefusal(w, err) {
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, preview)
}

func (h *Handlers) apiSaveFlow(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	var body flowSaveBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.StationID == 0 {
		writeError(w, http.StatusBadRequest, "station_id is required: a flow is saved by a station")
		return
	}
	station, err := h.engine.StationService().Get(body.StationID)
	if err != nil || station == nil {
		writeError(w, http.StatusBadRequest, "unknown station")
		return
	}
	if station.ProcessID != processID {
		writeError(w, http.StatusBadRequest, "station "+station.Name+" is not a station of this process")
		return
	}
	// WHO SAVED THIS, AND FROM WHERE — decided here, from the authenticated
	// caller, and never from the body (owner ruling R1).
	//
	// Both surfaces POST this route. An admin session is a person on the
	// desktop; no session is a station on the floor, which is what the kiosk
	// is. The station is still required and still has to belong to the
	// process either way: a flow is saved AT a press, and the desktop says
	// which one it is looking at.
	//
	// This is the routing set's rule applied to a second write. A body that
	// declared its own source would be a page deciding authorship, and
	// flowSaveBody deliberately has nowhere to put one.
	source, calledBy := domain.ClaimSourceHMI, station.Name
	if user, ok := h.sessions.getUser(r); ok && user != "" {
		source, calledBy = domain.ClaimSourceAdmin, user
	}
	// PRESET PROVENANCE, WHEN THIS SAVE IS AN APPLY (U10). Both or neither: a
	// row that says "preset 42, version nothing" is provenance nobody can
	// read, and half-writing it is worse than not recording it.
	var presetID *int64
	var presetVersion *int
	switch {
	case body.SourcePresetID != 0 && body.SourcePresetVersion != 0:
		id, v := body.SourcePresetID, body.SourcePresetVersion
		presetID, presetVersion = &id, &v
	case body.SourcePresetID != 0 || body.SourcePresetVersion != 0:
		writeError(w, http.StatusBadRequest,
			"source_preset_id and source_preset_version go together: a version without an id, or an id without a version, is provenance nobody can read")
		return
	}
	result, err := h.orchestration.SaveFlow(processID, engine.FlowSaveRequest{
		ToStyleID: body.ToStyleID, Cells: body.Cells, Fingerprint: body.Fingerprint,
		Source: source, CalledBy: calledBy,
		SourcePresetID: presetID, SourcePresetVersion: presetVersion,
	})
	if err != nil {
		var invalid *engine.FlowValidationError
		switch {
		case errors.Is(err, engine.ErrFlowComposerDisabled):
			writeError(w, http.StatusForbidden, err.Error())
		case errors.As(err, &invalid):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "findings": invalid.Findings})
		case writeFlowRefusal(w, err):
		case errors.Is(err, protocol.ErrInvalidSwapMode):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	// The same three things an admin claim write does, once for the whole
	// flow: a backup, the boards, and ONE per-process publish so Core's
	// mirror carries the new rows.
	//
	// THE BOARDS AND THE PUBLISH GO ON THE SAME DOORBELL. A preset applied to
	// forty parts is forty saves, one after the other, and broadcasting per
	// save held every station at its 500 ms burst cadence for the whole apply
	// — the polls it provoked cost the box about twice what the apply did.
	// Coalesced, the burst is one broadcast and one publish.
	h.requestBackup("flow-saved")
	h.requestSpecChangePublishAndRefresh(processID)
	writeJSON(w, result)
}
