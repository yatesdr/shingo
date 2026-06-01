package www

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// handleDemand renders the demand page.
func (h *Handlers) handleDemand(w http.ResponseWriter, r *http.Request) {
	demands, _ := h.engine.DemandService().List()
	payloads, _ := h.engine.PayloadService().List()
	data := map[string]any{
		"Page":     "demand",
		"Demands":  demands,
		"Payloads": payloads,
	}
	h.render(w, r, "demand.html", data)
}

// --- Demand API ---

// validateCatID rejects a cat_id that is empty or not a known payload code.
// The payloads catalog is authoritative: produced counts arrive from the
// edge keyed by payload code, so a demand must reference a real part for its
// produced tally to ever advance. Returns false and writes the error
// response when invalid.
func (h *Handlers) validateCatID(w http.ResponseWriter, catID string) bool {
	if catID == "" {
		h.jsonError(w, "cat_id is required", http.StatusBadRequest)
		return false
	}
	p, err := h.engine.PayloadService().GetByCode(catID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && p == nil) {
		h.jsonError(w, "cat_id '"+catID+"' is not a known payload — create it under Payloads first", http.StatusBadRequest)
		return false
	}
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return false
	}
	return true
}

func (h *Handlers) apiListDemands(w http.ResponseWriter, r *http.Request) {
	demands, err := h.engine.DemandService().List()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, demands)
}

func (h *Handlers) apiCreateDemand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CatID       string `json:"cat_id"`
		Description string `json:"description"`
		DemandQty   int64  `json:"demand_qty"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if !h.validateCatID(w, req.CatID) {
		return
	}
	id, err := h.engine.DemandService().Create(req.CatID, req.Description, req.DemandQty)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdateDemand(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req struct {
		CatID       string `json:"cat_id"`
		Description string `json:"description"`
		DemandQty   int64  `json:"demand_qty"`
		ProducedQty int64  `json:"produced_qty"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if !h.validateCatID(w, req.CatID) {
		return
	}
	if err := h.engine.DemandService().Update(id, req.CatID, req.Description, req.DemandQty, req.ProducedQty); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiApplyDemand(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req struct {
		Description string `json:"description"`
		DemandQty   int64  `json:"demand_qty"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if err := h.engine.DemandService().UpdateAndResetProduced(id, req.Description, req.DemandQty); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiDeleteDemand(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.engine.DemandService().Delete(id); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiApplyAllDemands(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Rows []struct {
			ID          int64  `json:"id"`
			Description string `json:"description"`
			DemandQty   int64  `json:"demand_qty"`
		} `json:"rows"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	for _, row := range req.Rows {
		if err := h.engine.DemandService().UpdateAndResetProduced(row.ID, row.Description, row.DemandQty); err != nil {
			h.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiSetDemandProduced(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req struct {
		ProducedQty int64 `json:"produced_qty"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.ProducedQty < 0 {
		h.jsonError(w, "produced_qty must be >= 0", http.StatusBadRequest)
		return
	}
	if err := h.engine.DemandService().SetProduced(id, req.ProducedQty); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiClearDemandProduced(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.engine.DemandService().ClearProduced(id); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiClearAllProduced(w http.ResponseWriter, r *http.Request) {
	if err := h.engine.DemandService().ClearAllProduced(); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiDemandLog(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	demand, err := h.engine.DemandService().Get(id)
	if err != nil {
		h.jsonError(w, "demand not found", http.StatusNotFound)
		return
	}
	entries, err := h.engine.DemandService().ListProductionLog(demand.CatID, 100)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, entries)
}
