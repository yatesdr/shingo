package www

import (
	"net/http"
	"strconv"

	"shingocore/store"
)

func (h *Handlers) handlePayloadTypes(w http.ResponseWriter, r *http.Request) {
	types, _ := h.engine.DB().ListPayloadTypes()
	data := map[string]any{
		"Page":          "payload_types",
		"PayloadTypes":  types,
		"Authenticated": h.isAuthenticated(r),
	}
	h.render(w, "payload_types.html", data)
}

func (h *Handlers) handlePayloadTypeCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	manifest := r.FormValue("default_manifest_json")
	if manifest == "" {
		manifest = "{}"
	}

	pt := &store.PayloadType{
		Name:                r.FormValue("name"),
		Description:         r.FormValue("description"),
		FormFactor:          r.FormValue("form_factor"),
		DefaultManifestJSON: manifest,
	}

	if err := h.engine.DB().CreatePayloadType(pt); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/payload-types", http.StatusSeeOther)
}

func (h *Handlers) handlePayloadTypeUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	pt, err := h.engine.DB().GetPayloadType(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	manifest := r.FormValue("default_manifest_json")
	if manifest == "" {
		manifest = "{}"
	}

	pt.Name = r.FormValue("name")
	pt.Description = r.FormValue("description")
	pt.FormFactor = r.FormValue("form_factor")
	pt.DefaultManifestJSON = manifest

	if err := h.engine.DB().UpdatePayloadType(pt); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/payload-types", http.StatusSeeOther)
}

func (h *Handlers) handlePayloadTypeDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.engine.DB().DeletePayloadType(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/payload-types", http.StatusSeeOther)
}
