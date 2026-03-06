package www

import (
	"net/http"
	"strconv"

	"shingocore/store"
)

func (h *Handlers) handleBlueprintCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	manifest := r.FormValue("default_manifest_json")
	if manifest == "" {
		manifest = "{}"
	}

	uop, _ := strconv.Atoi(r.FormValue("uop_capacity"))

	bp := &store.Blueprint{
		Code:                r.FormValue("code"),
		Description:         r.FormValue("description"),
		UOPCapacity:         uop,
		DefaultManifestJSON: manifest,
	}

	if err := h.engine.DB().CreateBlueprint(bp); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/payloads", http.StatusSeeOther)
}

func (h *Handlers) handleBlueprintUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	bp, err := h.engine.DB().GetBlueprint(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	manifest := r.FormValue("default_manifest_json")
	if manifest == "" {
		manifest = "{}"
	}

	bp.Code = r.FormValue("code")
	bp.Description = r.FormValue("description")
	bp.UOPCapacity, _ = strconv.Atoi(r.FormValue("uop_capacity"))
	bp.DefaultManifestJSON = manifest

	if err := h.engine.DB().UpdateBlueprint(bp); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/payloads", http.StatusSeeOther)
}

func (h *Handlers) handleBlueprintDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.engine.DB().DeleteBlueprint(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/payloads", http.StatusSeeOther)
}

func (h *Handlers) apiCreateBlueprint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code        string  `json:"code"`
		Description string  `json:"description"`
		UOPCapacity int     `json:"uop_capacity"`
		BinTypeIDs  []int64 `json:"bin_type_ids"`
		Manifest    []struct {
			PartNumber string  `json:"part_number"`
			Quantity   int64   `json:"quantity"`
		} `json:"manifest"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	bp := &store.Blueprint{
		Code:                req.Code,
		Description:         req.Description,
		UOPCapacity:         req.UOPCapacity,
		DefaultManifestJSON: "{}",
	}
	if err := h.engine.DB().CreateBlueprint(bp); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if len(req.BinTypeIDs) > 0 {
		h.engine.DB().SetBlueprintBinTypes(bp.ID, req.BinTypeIDs)
	}
	if len(req.Manifest) > 0 {
		var items []*store.BlueprintManifestItem
		for _, it := range req.Manifest {
			items = append(items, &store.BlueprintManifestItem{
				PartNumber: it.PartNumber,
				Quantity:   it.Quantity,
			})
		}
		h.engine.DB().ReplaceBlueprintManifest(bp.ID, items)
	}

	h.jsonOK(w, bp)
}

func (h *Handlers) apiUpdateBlueprint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID          int64   `json:"id"`
		Code        string  `json:"code"`
		Description string  `json:"description"`
		UOPCapacity int     `json:"uop_capacity"`
		BinTypeIDs  []int64 `json:"bin_type_ids"`
		Manifest    []struct {
			PartNumber string  `json:"part_number"`
			Quantity   int64   `json:"quantity"`
		} `json:"manifest"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	bp, err := h.engine.DB().GetBlueprint(req.ID)
	if err != nil {
		h.jsonError(w, "not found", http.StatusNotFound)
		return
	}

	bp.Code = req.Code
	bp.Description = req.Description
	bp.UOPCapacity = req.UOPCapacity

	if err := h.engine.DB().UpdateBlueprint(bp); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.engine.DB().SetBlueprintBinTypes(bp.ID, req.BinTypeIDs)

	var items []*store.BlueprintManifestItem
	for _, it := range req.Manifest {
		items = append(items, &store.BlueprintManifestItem{
			PartNumber: it.PartNumber,
			Quantity:   it.Quantity,
		})
	}
	h.engine.DB().ReplaceBlueprintManifest(bp.ID, items)

	h.jsonSuccess(w)
}

func (h *Handlers) apiGetBlueprintManifest(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	items, err := h.engine.DB().ListBlueprintManifest(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, items)
}

func (h *Handlers) apiSaveBlueprintManifest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BlueprintID int64 `json:"blueprint_id"`
		Items       []struct {
			PartNumber  string  `json:"part_number"`
			Quantity    int64   `json:"quantity"`
			Description string  `json:"description"`
		} `json:"items"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	var items []*store.BlueprintManifestItem
	for _, it := range req.Items {
		items = append(items, &store.BlueprintManifestItem{
			PartNumber:  it.PartNumber,
			Quantity:    it.Quantity,
			Description: it.Description,
		})
	}

	if err := h.engine.DB().ReplaceBlueprintManifest(req.BlueprintID, items); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiGetBlueprintBinTypes(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	binTypes, err := h.engine.DB().ListBinTypesForBlueprint(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, binTypes)
}

func (h *Handlers) apiSaveBlueprintBinTypes(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BlueprintID int64   `json:"blueprint_id"`
		BinTypeIDs  []int64 `json:"bin_type_ids"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if err := h.engine.DB().SetBlueprintBinTypes(req.BlueprintID, req.BinTypeIDs); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}
