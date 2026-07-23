package www

import (
	"net/http"
	"strconv"

	"shingocore/domain"
)

func (h *Handlers) handleBinTypeCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	widthIn, err := strconv.ParseFloat(r.FormValue("width_in"), 64)
	if err != nil && r.FormValue("width_in") != "" {
		http.Error(w, "invalid width_in", http.StatusBadRequest)
		return
	}
	heightIn, err := strconv.ParseFloat(r.FormValue("height_in"), 64)
	if err != nil && r.FormValue("height_in") != "" {
		http.Error(w, "invalid height_in", http.StatusBadRequest)
		return
	}

	bt := &domain.BinType{
		Code:        r.FormValue("code"),
		Description: r.FormValue("description"),
		WidthIn:     widthIn,
		HeightIn:    heightIn,
	}

	if err := h.engine.BinService().CreateBinType(bt); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/bins", http.StatusSeeOther)
}

func (h *Handlers) handleBinTypeUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	svc := h.engine.BinService()
	bt, err := svc.GetBinType(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	bt.Code = r.FormValue("code")
	bt.Description = r.FormValue("description")
	if w, err := strconv.ParseFloat(r.FormValue("width_in"), 64); err == nil || r.FormValue("width_in") == "" {
		bt.WidthIn = w
	}
	if h, err := strconv.ParseFloat(r.FormValue("height_in"), 64); err == nil || r.FormValue("height_in") == "" {
		bt.HeightIn = h
	}

	if err := svc.UpdateBinType(bt); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/bins", http.StatusSeeOther)
}

func (h *Handlers) handleBinTypeDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.engine.BinService().DeleteBinType(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/bins", http.StatusSeeOther)
}
