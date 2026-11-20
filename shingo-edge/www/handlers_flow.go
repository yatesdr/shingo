// handlers_flow.go — the flow composer's endpoints: preview a draft flow,
// save it, mint a style from a recipe and parts, and ask Core for a new part,
// all on the shop-floor group beside the changeover routes.
//
// The engine does the work (engine.PreviewFlow / SaveFlow / MintStyle /
// CreatePart); this file maps a request body to a request, an error to a
// status, and a saved flow to the one per-process claims publish the
// plant-claims mirror needs.

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
//
// taken_off names the parts the engineer took off this flow in the draft, so
// the unplaced-part finding does not read them as lost. Request-only: it is
// not stored and not on the station poll. See engine.FlowPreviewRequest.
type flowPreviewBody struct {
	ToStyleID int64             `json:"to_style_id"`
	Cells     []domain.FlowCell `json:"cells"`
	TakenOff  []string          `json:"taken_off"`
	// THE MINT FORM (pick-a-part U3): a recipe and parts instead of a style
	// and cells — the same body flow/mint takes (composer-model.js
	// mintBody). preset_id or parts set means this form, and the other three
	// are ignored: the mint plan decides the style and the cells. preset_id 0
	// with parts is the station asking about parts that already run in a style
	// (the plan resolves them before it needs a recipe).
	PresetID  int64             `json:"preset_id"`
	Parts     []engine.PartPick `json:"parts"`
	Overrides map[string]string `json:"overrides"`
	// StyleID previews a style tapped in the list; Split previews One
	// Part, Two Spots. Both are engine.MintRequest's.
	StyleID int64 `json:"style_id"`
	Split   bool  `json:"split"`
	New     bool  `json:"new"`
}

// flowSaveBody is POST /api/processes/{id}/flow/save. station_id is
// optional: when set it names the operator station the desktop is looking
// at, and the handler refuses one that is not this process's; zero or absent
// is a process with no screen yet. It is never authorship — called_by is the
// session's person (R1), not the station's name.
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
	// ?steps=1 asks for every cell's steady-state legs as Core would receive
	// them (engine.FlowPreview.Steady). Not on the mint form.
	//
	// r.Context() travels, so a preview whose operator has left the screen
	// stops at the next phase boundary instead of finishing a plan and a Core
	// call for nobody.
	preq := engine.FlowPreviewRequest{
		ToStyleID: body.ToStyleID, Cells: body.Cells, TakenOff: body.TakenOff,
		Preflight: r.URL.Query().Get("preflight") == "1",
		Steps:     r.URL.Query().Get("steps") == "1",
	}
	if body.PresetID != 0 || len(body.Parts) > 0 || body.StyleID != 0 {
		preq = engine.FlowPreviewRequest{Preflight: preq.Preflight, Mint: &engine.MintRequest{
			PresetID: body.PresetID, Parts: body.Parts, Overrides: body.Overrides,
			StyleID: body.StyleID, Split: body.Split, New: body.New,
		}}
	}
	preview, err := h.orchestration.PreviewFlow(r.Context(), processID, preq)
	if writeMintRefusal(w, err) {
		return
	}
	if err != nil {
		var noOrders *engine.FlowNoOrdersError
		if errors.As(err, &noOrders) && noOrders.Preview != nil {
			// 400 with the named reason — and the preview's findings and
			// unresolved nodes, which are usually why nothing fires.
			out := map[string]any{
				"error":       err.Error(),
				"order_count": 0,
				"findings":    noOrders.Preview.Findings,
				"unresolved":  noOrders.Preview.Unresolved,
				"preflight":   noOrders.Preview.Preflight,
				"fingerprint": noOrders.Preview.Fingerprint,
				"mint":        noOrders.Preview.Mint,
			}
			// Present when asked, absent otherwise, as on a 200 (omitempty).
			if len(noOrders.Preview.Steady) > 0 {
				out["steady"] = noOrders.Preview.Steady
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(out)
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
	// THE STATION IS OPTIONAL. A body that names one still has to name a
	// real station of THIS process — a desktop looking at one press and
	// saving into another is refused here, before the engine runs. A body
	// that names none saves: nothing below reads the station (the author is
	// the session's person, and engine.SaveFlow takes no station), and a
	// process has no screen until someone hangs one on it, which is after
	// its first style is set up, not before. Refusing that save made the
	// screen a prerequisite of the setup it is part of.
	// TestFlowSave_AProcessWithNoScreenSavesItsFirstStyle pins both halves.
	if body.StationID != 0 {
		station, err := h.engine.StationService().Get(body.StationID)
		if err != nil || station == nil {
			writeError(w, http.StatusBadRequest, "unknown station")
			return
		}
		if station.ProcessID != processID {
			writeError(w, http.StatusBadRequest, "station "+station.Name+" is not a station of this process")
			return
		}
	}
	// WHO SAVED THIS — decided here, from the signed-in caller, and never
	// from the body (owner ruling R1). The route is behind login: only the
	// desktop saves a flow, so the author is the session's person, whichever
	// screen (if any) the desktop was looking at.
	//
	// This is the routing set's rule applied to a second write. A body that
	// declared its own source would be a page deciding authorship, and
	// flowSaveBody deliberately has nowhere to put one.
	calledBy, _ := h.sessions.getUser(r)
	source := domain.ClaimSourceAdmin
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
		case errors.Is(err, protocol.ErrInvalidSwapMode), errors.Is(err, domain.ErrKeepStagedSpot),
			errors.Is(err, domain.ErrRefusedLeg):
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
	// BOTH ARE COALESCED. A preset applied to forty parts is forty saves, one
	// after the other: the publish rides the spec-change doorbell, and the
	// boards' refresh is the /api write's nudge (nudgeAfterWrite), one per
	// burst (EventHub.Nudge).
	h.requestBackup("flow-saved")
	h.requestSpecChangePublish(processID)
	writeJSON(w, result)
}

// writeMintRefusal maps the mint's own refusals: a recipe that is not this
// process's is 404, a request the recipe cannot take (parts that do not fit
// its slots, a part on a node no slot has, a move off the routing set) is 422
// with the sentence. Reports whether it wrote.
func writeMintRefusal(w http.ResponseWriter, err error) bool {
	var refused *engine.MintRefusal
	switch {
	case errors.Is(err, engine.ErrMintRecipe):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.As(err, &refused):
		writeError(w, http.StatusUnprocessableEntity, refused.Reason)
	default:
		return false
	}
	return true
}

// apiMintFlow is POST /api/processes/{id}/flow/mint: a style from (recipe,
// parts), resolved or minted (pick-a-part U3). The body is
// composer-model.js's mintBody — recipe, parts in tap order, station, and the
// Positions screen's moves if any. Never cells.
//
// The station resolves the same way flow/save does, and the source and
// called_by come from the caller, never the body (owner ruling R1). A station
// on a cell whose HMI setup is off is refused (403) any mint that would write
// — engine.gateRefusal; opening a style by style_id writes nothing and
// is not gated.
func (h *Handlers) apiMintFlow(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	var body engine.MintRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	stationName, ok := h.stationOfProcess(w, processID, body.StationID, "a style is minted at a station")
	if !ok {
		return
	}
	body.Source, body.CalledBy = domain.ClaimSourceHMI, stationName
	if user, ok := h.sessions.getUser(r); ok && user != "" {
		body.Source, body.CalledBy = domain.ClaimSourceAdmin, user
	}
	result, err := h.orchestration.MintStyle(processID, body)
	if err != nil {
		var invalid *engine.FlowValidationError
		switch {
		case writeMintRefusal(w, err):
		case errors.Is(err, engine.ErrFlowComposerDisabled):
			writeError(w, http.StatusForbidden, err.Error())
		case errors.As(err, &invalid):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "findings": invalid.Findings})
		case writeFlowRefusal(w, err):
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	// A minted style is a new flow: the same backup and one publish a save
	// fires (the boards' refresh is the /api write's nudge, as a save's is).
	// A resolved one changed nothing, so nothing is announced.
	if result.Minted {
		h.requestBackup("flow-minted")
		h.requestSpecChangePublish(processID)
	}
	writeJSON(w, result)
}

// stationOfProcess is the floor doors' station: the body's station_id, which
// must name a station of processID; its name is what the door records. Each
// refusal is a 400, written here; ok false means one was. why says what the
// door does at a station.
func (h *Handlers) stationOfProcess(w http.ResponseWriter, processID, stationID int64, why string) (string, bool) {
	if stationID == 0 {
		writeError(w, http.StatusBadRequest, "station_id is required: "+why)
		return "", false
	}
	station, err := h.engine.StationService().Get(stationID)
	if err != nil || station == nil {
		writeError(w, http.StatusBadRequest, "unknown station")
		return "", false
	}
	if station.ProcessID != processID {
		writeError(w, http.StatusBadRequest, "station "+station.Name+" is not a station of this process")
		return "", false
	}
	return station.Name, true
}

// partCreateBody is POST /api/processes/{id}/parts: a part Core does not have,
// from the station's new-part form (composer-render.js). The process is the
// URL's; the numbers are whole, and blank optional fields are null.
type partCreateBody struct {
	StationID    int64  `json:"station_id"`
	PartNumber   string `json:"part_number"`
	StandardPack int    `json:"standard_pack"`
	BinTypeCode  string `json:"bin_type_code"`
	CATID        string `json:"catid"`
	ToteLayers   *int   `json:"tote_layers"`
	UOPPerTote   *int   `json:"uop_per_tote"`
}

// partCreator is what the new-part door asks of the engine beyond
// EngineOrchestration: the part.create ask (engine.CreatePart). Asserted, as
// holdMarker is, so the orchestration interface does not widen for it.
type partCreator interface {
	CreatePart(processID int64, req protocol.PartCreateRequest) (*protocol.PartCreateResponse, error)
}

// apiCreatePart is POST /api/processes/{id}/parts: the station asks Core to
// make a new part for this process, gated as the mint is — the station must
// be the process's, and a process whose HMI setup is off is refused (403,
// engine.ErrFlowComposerDisabled's sentence).
//
// The request is held until Core's word, for the ask's whole life
// (engine.CreatePart's wait), so a late yes or no still answers it:
//
//   - 200: Core made it, or this cell had made it before (repeat true). The
//     body is Core's answer (protocol.PartCreateResponse), already applied:
//     the part is in the catalog and on this process's part set.
//   - 400: refused before anything was sent (engine.PartCreateInvalid, a Core
//     that does not answer the kind yet), or a bad body.
//   - 409: Core refused it, with Core's sentence; nothing applied.
//   - 504: no answer from Core in the ask's life
//     (engine.ErrPartCreateNoAnswer's sentence); the form can send it again.
func (h *Handlers) apiCreatePart(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	var body partCreateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, ok := h.stationOfProcess(w, processID, body.StationID, "a part is made at a station"); !ok {
		return
	}
	creator, ok := h.orchestration.(partCreator)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "a new part cannot be made from here")
		return
	}
	made, err := creator.CreatePart(processID, protocol.PartCreateRequest{
		PartNumber: body.PartNumber, StandardPack: body.StandardPack, BinTypeCode: body.BinTypeCode,
		CATID: body.CATID, ToteLayers: body.ToteLayers, UOPPerTote: body.UOPPerTote,
	})
	if err != nil {
		var invalid *engine.PartCreateInvalid
		var refusal *engine.CoreRefusal
		switch {
		case errors.Is(err, engine.ErrFlowComposerDisabled):
			writeError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, engine.ErrPartCreateNoAnswer):
			writeError(w, http.StatusGatewayTimeout, err.Error())
		case errors.As(err, &refusal):
			// Core's sentence, whatever status Core answered with: a 408
			// passed through would read to a browser as its own timeout.
			writeError(w, http.StatusConflict, refusal.Sentence)
		case errors.As(err, &invalid), errors.Is(err, engine.ErrCoreDoesNotAnswer):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, made)
}
