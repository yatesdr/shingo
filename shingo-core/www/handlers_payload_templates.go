package www

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"shingocore/domain"
)

// manifestLine is the shape every JSON manifest payload shares: a part number
// and its per-cycle ratio.
type manifestLine struct {
	PartNumber    string
	PartsPerCycle int64
}

// validateManifestLines rejects a manifest line whose per-cycle ratio is
// missing or non-positive, naming every offending part rather than the first.
//
// A MISSING RATIO ARRIVES AS ZERO AND CANNOT BE TOLD FROM A DECLARED ONE. JSON
// omits it, the browser used to submit a blank box as 0, and a spreadsheet cell
// left empty parses to 0 — three spellings of "I did not say" landing on a
// value that reads as "there are none of these in the bin". Four rows reached
// Springfield that way (payload_manifest ids 117/123/137/150) and were
// corrected by hand at the plant once the owner declared them entry oversights.
//
// Non-positive is refused rather than only missing, because the count a bin
// ships to the inventory ledger is uop_remaining x this number: a zero line
// contributes nothing to any count while looking configured. A part that
// genuinely is not in the carrier is a line that does not belong on the
// manifest.
//
// The form refuses this too, and that is not redundancy — the form is one of
// five doors. The bulk importer and three JSON endpoints reach the same column,
// and a client-side check closes none of them. It is also what has to be true
// before a CHECK (parts_per_cycle > 0) can ship without breaking the plants'
// own imports.
func validateManifestLines(lines []manifestLine) error {
	var bad []string
	for _, l := range lines {
		if l.PartNumber == "" {
			continue
		}
		if l.PartsPerCycle < 1 {
			bad = append(bad, fmt.Sprintf("%s (%d)", l.PartNumber, l.PartsPerCycle))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("parts_per_cycle must be 1 or more on every manifest line; "+
		"it is how many of the part ONE production cycle uses, usually 1. Fix: %s",
		strings.Join(bad, ", "))
}

func (h *Handlers) handlePayloadCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	uop, _ := strconv.Atoi(r.FormValue("uop_capacity"))

	p := &domain.Payload{
		Code:                 r.FormValue("code"),
		Description:          r.FormValue("description"),
		UOPCapacity:          uop,
		RobotGroup:           r.FormValue("robot_group"),
		AdvancedLoadSequence: r.FormValue("advanced_load_sequence"),
	}

	if _, err := h.engine.ValidateAdvancedLoadSequence(0, p.AdvancedLoadSequence); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := h.engine.PayloadService().Create(p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/payloads", http.StatusSeeOther)
}

func (h *Handlers) handlePayloadUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	p, err := h.engine.PayloadService().Get(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	p.Code = r.FormValue("code")
	p.Description = r.FormValue("description")
	p.UOPCapacity, _ = strconv.Atoi(r.FormValue("uop_capacity"))
	p.RobotGroup = r.FormValue("robot_group")
	p.AdvancedLoadSequence = r.FormValue("advanced_load_sequence")

	if _, err := h.engine.ValidateAdvancedLoadSequence(p.ID, p.AdvancedLoadSequence); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := h.engine.PayloadService().Update(p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/payloads", http.StatusSeeOther)
}

func (h *Handlers) handlePayloadDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.engine.PayloadService().Delete(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/payloads", http.StatusSeeOther)
}

func (h *Handlers) apiCreatePayloadTemplate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code                 string  `json:"code"`
		Description          string  `json:"description"`
		UOPCapacity          int     `json:"uop_capacity"`
		RobotGroup           string  `json:"robot_group"`
		AdvancedLoadSequence string  `json:"advanced_load_sequence"`
		BinTypeIDs           []int64 `json:"bin_type_ids"`
		Manifest             []struct {
			PartNumber    string `json:"part_number"`
			PartsPerCycle int64  `json:"parts_per_cycle"`
		} `json:"manifest"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	lines := make([]manifestLine, 0, len(req.Manifest))
	for _, it := range req.Manifest {
		lines = append(lines, manifestLine{PartNumber: it.PartNumber, PartsPerCycle: it.PartsPerCycle})
	}
	if err := validateManifestLines(lines); err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	p := &domain.Payload{
		Code:                 req.Code,
		Description:          req.Description,
		UOPCapacity:          req.UOPCapacity,
		RobotGroup:           req.RobotGroup,
		AdvancedLoadSequence: req.AdvancedLoadSequence,
	}
	// Config-time validation (fail loud on a real missing key, warn-and-save when
	// unverifiable). A new payload has no assigned nodes yet, so this rejects only
	// an unknown sequence name; a real key check happens on later edits / Check.
	check, verr := h.engine.ValidateAdvancedLoadSequence(0, p.AdvancedLoadSequence)
	if verr != nil {
		h.jsonError(w, verr.Error(), http.StatusBadRequest)
		return
	}
	if err := h.engine.PayloadService().Create(p); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if len(req.BinTypeIDs) > 0 {
		if err := h.engine.PayloadService().SetBinTypes(p.ID, req.BinTypeIDs); err != nil {
			h.jsonError(w, "bin types: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if len(req.Manifest) > 0 {
		var items []*domain.PayloadManifestItem
		for _, it := range req.Manifest {
			items = append(items, &domain.PayloadManifestItem{
				PartNumber:    it.PartNumber,
				PartsPerCycle: it.PartsPerCycle,
			})
		}
		if err := h.engine.PayloadService().ReplaceManifest(p.ID, items); err != nil {
			h.jsonError(w, "manifest: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Response is the payload with any "saved unverified" warnings appended.
	// warnings is omitempty, so a clean save is byte-compatible with the prior
	// bare-payload response (existing clients decode it straight into a payload).
	h.jsonOK(w, struct {
		*domain.Payload
		Warnings []string `json:"warnings,omitempty"`
	}{p, check.Warnings})
}

func (h *Handlers) apiUpdatePayloadTemplate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID                   int64   `json:"id"`
		Code                 string  `json:"code"`
		Description          string  `json:"description"`
		UOPCapacity          int     `json:"uop_capacity"`
		RobotGroup           string  `json:"robot_group"`
		AdvancedLoadSequence string  `json:"advanced_load_sequence"`
		BinTypeIDs           []int64 `json:"bin_type_ids"`
		Manifest             []struct {
			PartNumber    string `json:"part_number"`
			PartsPerCycle int64  `json:"parts_per_cycle"`
		} `json:"manifest"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	lines := make([]manifestLine, 0, len(req.Manifest))
	for _, it := range req.Manifest {
		lines = append(lines, manifestLine{PartNumber: it.PartNumber, PartsPerCycle: it.PartsPerCycle})
	}
	if err := validateManifestLines(lines); err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	p, err := h.engine.PayloadService().Get(req.ID)
	if err != nil {
		h.jsonError(w, "not found", http.StatusNotFound)
		return
	}

	p.Code = req.Code
	p.Description = req.Description
	p.UOPCapacity = req.UOPCapacity
	p.RobotGroup = req.RobotGroup
	p.AdvancedLoadSequence = req.AdvancedLoadSequence

	// Validate the (possibly new) sequence against this payload's assigned node
	// locations BEFORE persisting: a real missing key rejects the save; an
	// unverifiable case saves with warnings (flagged unverified).
	check, verr := h.engine.ValidateAdvancedLoadSequence(p.ID, p.AdvancedLoadSequence)
	if verr != nil {
		h.jsonError(w, verr.Error(), http.StatusBadRequest)
		return
	}

	if err := h.engine.PayloadService().Update(p); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := h.engine.PayloadService().SetBinTypes(p.ID, req.BinTypeIDs); err != nil {
		h.jsonError(w, "bin types: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var items []*domain.PayloadManifestItem
	for _, it := range req.Manifest {
		items = append(items, &domain.PayloadManifestItem{
			PartNumber:    it.PartNumber,
			PartsPerCycle: it.PartsPerCycle,
		})
	}
	if err := h.engine.PayloadService().ReplaceManifest(p.ID, items); err != nil {
		h.jsonError(w, "manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// {"status":"ok"} plus any "saved unverified" warnings. warnings is omitempty
	// so a clean save is byte-identical to the prior jsonSuccess response.
	h.jsonOK(w, struct {
		Status   string   `json:"status"`
		Warnings []string `json:"warnings,omitempty"`
	}{"ok", check.Warnings})
}

// apiListLoadSequences returns the registered advanced-load-sequence names for
// the payload-editor dropdown (the empty "normal load" option is added by the UI).
func (h *Handlers) apiListLoadSequences(w http.ResponseWriter, r *http.Request) {
	names, err := h.engine.PayloadService().ListLoadSequenceNames()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, map[string]any{"names": names})
}

// apiCheckLoadSequence re-runs config-time validation for a payload's selected
// load sequence on demand (the "Check" button). id is optional (0 = a payload
// not yet saved / with no nodes); sequence is the currently-selected name.
// Unlike save it never rejects — it always reports the verified / missing /
// warnings breakdown so the operator sees exactly which location is missing
// which key.
func (h *Handlers) apiCheckLoadSequence(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	seq := r.URL.Query().Get("sequence")
	check, err := h.engine.ValidateAdvancedLoadSequence(id, seq)
	if check == nil {
		// A nil check means a real server-side failure (DB error), not a
		// validation verdict — those come back on the check itself.
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, check)
}

func (h *Handlers) apiGetPayloadManifestTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	items, err := h.engine.PayloadService().ListManifest(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, items)
}

func (h *Handlers) apiSavePayloadManifestTemplate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PayloadID int64 `json:"payload_id"`
		Items     []struct {
			PartNumber    string `json:"part_number"`
			PartsPerCycle int64  `json:"parts_per_cycle"`
			Description   string `json:"description"`
		} `json:"items"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	var items []*domain.PayloadManifestItem
	for _, it := range req.Items {
		items = append(items, &domain.PayloadManifestItem{
			PartNumber:    it.PartNumber,
			PartsPerCycle: it.PartsPerCycle,
			Description:   it.Description,
		})
	}

	if err := h.engine.PayloadService().ReplaceManifest(req.PayloadID, items); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiGetPayloadBinTypes(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	binTypes, err := h.engine.PayloadService().ListBinTypes(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, binTypes)
}

func (h *Handlers) apiSavePayloadBinTypes(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PayloadID  int64   `json:"payload_id"`
		BinTypeIDs []int64 `json:"bin_type_ids"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if err := h.engine.PayloadService().SetBinTypes(req.PayloadID, req.BinTypeIDs); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}
