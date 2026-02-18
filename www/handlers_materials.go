package www

import (
	"net/http"
	"strconv"

	"warpath/store"
)

func (h *Handlers) handleMaterials(w http.ResponseWriter, r *http.Request) {
	materials, _ := h.engine.DB().ListMaterials()
	data := map[string]any{
		"Page":          "materials",
		"Materials":     materials,
		"Authenticated": h.isAuthenticated(r),
	}
	h.render(w, "materials.html", data)
}

func (h *Handlers) handleMaterialCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	mat := &store.Material{
		Code:        r.FormValue("code"),
		Description: r.FormValue("description"),
		Unit:        r.FormValue("unit"),
	}
	if mat.Unit == "" {
		mat.Unit = "ea"
	}

	if err := h.engine.DB().CreateMaterial(mat); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/materials", http.StatusSeeOther)
}

func (h *Handlers) handleMaterialUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid material id", http.StatusBadRequest)
		return
	}

	mat, err := h.engine.DB().GetMaterial(id)
	if err != nil {
		http.Error(w, "material not found", http.StatusNotFound)
		return
	}

	mat.Code = r.FormValue("code")
	mat.Description = r.FormValue("description")
	mat.Unit = r.FormValue("unit")

	if err := h.engine.DB().UpdateMaterial(mat); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/materials", http.StatusSeeOther)
}

func (h *Handlers) handleMaterialDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid material id", http.StatusBadRequest)
		return
	}

	if err := h.engine.DB().DeleteMaterial(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/materials", http.StatusSeeOther)
}
