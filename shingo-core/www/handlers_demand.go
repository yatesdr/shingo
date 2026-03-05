package www

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// handleDemand renders the demand page.
func (h *Handlers) handleDemand(w http.ResponseWriter, r *http.Request) {
	demands, _ := h.engine.DB().ListDemands()
	data := map[string]any{
		"Page":          "demand",
		"Demands": demands,
	}
	h.render(w, r, "demand.html", data)
}

// --- Demand API ---

func (h *Handlers) apiListDemands(w http.ResponseWriter, r *http.Request) {
	demands, err := h.engine.DB().ListDemands()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, demands)
}

func (h *Handlers) apiCreateDemand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CatID       string  `json:"cat_id"`
		Description string  `json:"description"`
		DemandQty   float64 `json:"demand_qty"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.CatID == "" {
		h.jsonError(w, "cat_id is required", http.StatusBadRequest)
		return
	}
	id, err := h.engine.DB().CreateDemand(req.CatID, req.Description, req.DemandQty)
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
		CatID       string  `json:"cat_id"`
		Description string  `json:"description"`
		DemandQty   float64 `json:"demand_qty"`
		ProducedQty float64 `json:"produced_qty"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if err := h.engine.DB().UpdateDemand(id, req.CatID, req.Description, req.DemandQty, req.ProducedQty); err != nil {
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
		Description string  `json:"description"`
		DemandQty   float64 `json:"demand_qty"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if err := h.engine.DB().UpdateDemandAndResetProduced(id, req.Description, req.DemandQty); err != nil {
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
	if err := h.engine.DB().DeleteDemand(id); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiApplyAllDemands(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Rows []struct {
			ID          int64   `json:"id"`
			Description string  `json:"description"`
			DemandQty   float64 `json:"demand_qty"`
		} `json:"rows"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	for _, row := range req.Rows {
		if err := h.engine.DB().UpdateDemandAndResetProduced(row.ID, row.Description, row.DemandQty); err != nil {
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
		ProducedQty float64 `json:"produced_qty"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.ProducedQty < 0 {
		h.jsonError(w, "produced_qty must be >= 0", http.StatusBadRequest)
		return
	}
	if err := h.engine.DB().SetProduced(id, req.ProducedQty); err != nil {
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
	if err := h.engine.DB().ClearProduced(id); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiClearAllProduced(w http.ResponseWriter, r *http.Request) {
	if err := h.engine.DB().ClearAllProduced(); err != nil {
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
	demand, err := h.engine.DB().GetDemand(id)
	if err != nil {
		h.jsonError(w, "demand not found", http.StatusNotFound)
		return
	}
	entries, err := h.engine.DB().ListProductionLog(demand.CatID, 100)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, entries)
}
