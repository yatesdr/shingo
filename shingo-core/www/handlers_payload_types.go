package www

import (
	"net/http"
	"strconv"

	"shingocore/store"
)

func (h *Handlers) handlePayloadStyleCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	manifest := r.FormValue("default_manifest_json")
	if manifest == "" {
		manifest = "{}"
	}

	uop, _ := strconv.Atoi(r.FormValue("uop_capacity"))
	widthMM, _ := strconv.ParseFloat(r.FormValue("width_mm"), 64)
	heightMM, _ := strconv.ParseFloat(r.FormValue("height_mm"), 64)
	depthMM, _ := strconv.ParseFloat(r.FormValue("depth_mm"), 64)
	weightKG, _ := strconv.ParseFloat(r.FormValue("weight_kg"), 64)

	ps := &store.PayloadStyle{
		Name:                r.FormValue("name"),
		Code:                r.FormValue("code"),
		Description:         r.FormValue("description"),
		FormFactor:          r.FormValue("form_factor"),
		UOPCapacity:         uop,
		WidthMM:             widthMM,
		HeightMM:            heightMM,
		DepthMM:             depthMM,
		WeightKG:            weightKG,
		DefaultManifestJSON: manifest,
	}

	if err := h.engine.DB().CreatePayloadStyle(ps); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/payloads", http.StatusSeeOther)
}

func (h *Handlers) handlePayloadStyleUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	ps, err := h.engine.DB().GetPayloadStyle(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	manifest := r.FormValue("default_manifest_json")
	if manifest == "" {
		manifest = "{}"
	}

	ps.Name = r.FormValue("name")
	ps.Code = r.FormValue("code")
	ps.Description = r.FormValue("description")
	ps.FormFactor = r.FormValue("form_factor")
	ps.UOPCapacity, _ = strconv.Atoi(r.FormValue("uop_capacity"))
	ps.WidthMM, _ = strconv.ParseFloat(r.FormValue("width_mm"), 64)
	ps.HeightMM, _ = strconv.ParseFloat(r.FormValue("height_mm"), 64)
	ps.DepthMM, _ = strconv.ParseFloat(r.FormValue("depth_mm"), 64)
	ps.WeightKG, _ = strconv.ParseFloat(r.FormValue("weight_kg"), 64)
	ps.DefaultManifestJSON = manifest

	if err := h.engine.DB().UpdatePayloadStyle(ps); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/payloads", http.StatusSeeOther)
}

func (h *Handlers) handlePayloadStyleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.engine.DB().DeletePayloadStyle(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/payloads", http.StatusSeeOther)
}
