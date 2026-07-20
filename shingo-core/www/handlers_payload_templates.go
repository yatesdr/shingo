package www

import (
	"net/http"
	"strconv"

	"shingocore/domain"
)

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
			PartNumber string `json:"part_number"`
			Quantity   int64  `json:"quantity"`
		} `json:"manifest"`
	}
	if !h.parseJSON(w, r, &req) {
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
				PartNumber: it.PartNumber,
				Quantity:   it.Quantity,
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
			PartNumber string `json:"part_number"`
			Quantity   int64  `json:"quantity"`
		} `json:"manifest"`
	}
	if !h.parseJSON(w, r, &req) {
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
			PartNumber: it.PartNumber,
			Quantity:   it.Quantity,
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
			PartNumber  string `json:"part_number"`
			Quantity    int64  `json:"quantity"`
			Description string `json:"description"`
		} `json:"items"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	var items []*domain.PayloadManifestItem
	for _, it := range req.Items {
		items = append(items, &domain.PayloadManifestItem{
			PartNumber:  it.PartNumber,
			Quantity:    it.Quantity,
			Description: it.Description,
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
