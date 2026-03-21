package www

import (
	"net/http"
	"strconv"

	"shingocore/store"
)

func (h *Handlers) handlePayloadCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	uop, _ := strconv.Atoi(r.FormValue("uop_capacity"))

	p := &store.Payload{
		Code:        r.FormValue("code"),
		Description: r.FormValue("description"),
		UOPCapacity: uop,
	}

	if err := h.engine.DB().CreatePayload(p); err != nil {
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

	p, err := h.engine.DB().GetPayload(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	p.Code = r.FormValue("code")
	p.Description = r.FormValue("description")
	p.UOPCapacity, _ = strconv.Atoi(r.FormValue("uop_capacity"))

	if err := h.engine.DB().UpdatePayload(p); err != nil {
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

	if err := h.engine.DB().DeletePayload(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/payloads", http.StatusSeeOther)
}

func (h *Handlers) apiCreatePayloadTemplate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code        string  `json:"code"`
		Description string  `json:"description"`
		UOPCapacity int     `json:"uop_capacity"`
		BinTypeIDs  []int64 `json:"bin_type_ids"`
		Manifest    []struct {
			PartNumber string `json:"part_number"`
			Quantity   int64  `json:"quantity"`
		} `json:"manifest"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	p := &store.Payload{
		Code:        req.Code,
		Description: req.Description,
		UOPCapacity: req.UOPCapacity,
	}
	if err := h.engine.DB().CreatePayload(p); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if len(req.BinTypeIDs) > 0 {
		h.engine.DB().SetPayloadBinTypes(p.ID, req.BinTypeIDs)
	}
	if len(req.Manifest) > 0 {
		var items []*store.PayloadManifestItem
		for _, it := range req.Manifest {
			items = append(items, &store.PayloadManifestItem{
				PartNumber: it.PartNumber,
				Quantity:   it.Quantity,
			})
		}
		h.engine.DB().ReplacePayloadManifest(p.ID, items)
	}

	h.jsonOK(w, p)
}

func (h *Handlers) apiUpdatePayloadTemplate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID          int64   `json:"id"`
		Code        string  `json:"code"`
		Description string  `json:"description"`
		UOPCapacity int     `json:"uop_capacity"`
		BinTypeIDs  []int64 `json:"bin_type_ids"`
		Manifest    []struct {
			PartNumber string `json:"part_number"`
			Quantity   int64  `json:"quantity"`
		} `json:"manifest"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	p, err := h.engine.DB().GetPayload(req.ID)
	if err != nil {
		h.jsonError(w, "not found", http.StatusNotFound)
		return
	}

	p.Code = req.Code
	p.Description = req.Description
	p.UOPCapacity = req.UOPCapacity

	if err := h.engine.DB().UpdatePayload(p); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.engine.DB().SetPayloadBinTypes(p.ID, req.BinTypeIDs)

	var items []*store.PayloadManifestItem
	for _, it := range req.Manifest {
		items = append(items, &store.PayloadManifestItem{
			PartNumber: it.PartNumber,
			Quantity:   it.Quantity,
		})
	}
	h.engine.DB().ReplacePayloadManifest(p.ID, items)

	h.jsonSuccess(w)
}

func (h *Handlers) apiGetPayloadManifestTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	items, err := h.engine.DB().ListPayloadManifest(id)
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

	var items []*store.PayloadManifestItem
	for _, it := range req.Items {
		items = append(items, &store.PayloadManifestItem{
			PartNumber:  it.PartNumber,
			Quantity:    it.Quantity,
			Description: it.Description,
		})
	}

	if err := h.engine.DB().ReplacePayloadManifest(req.PayloadID, items); err != nil {
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
	binTypes, err := h.engine.DB().ListBinTypesForPayload(id)
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
	if err := h.engine.DB().SetPayloadBinTypes(req.PayloadID, req.BinTypeIDs); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}
