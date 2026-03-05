package www

import (
	"net/http"
	"strconv"

	"shingocore/store"
)

func (h *Handlers) handleNodeTypeCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	nt := &store.NodeType{
		Code:        r.FormValue("code"),
		Name:        r.FormValue("name"),
		Description: r.FormValue("description"),
		IsSynthetic: r.FormValue("is_synthetic") == "on",
	}

	if err := h.engine.DB().CreateNodeType(nt); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}

func (h *Handlers) handleNodeTypeUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	nt, err := h.engine.DB().GetNodeType(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	nt.Code = r.FormValue("code")
	nt.Name = r.FormValue("name")
	nt.Description = r.FormValue("description")
	nt.IsSynthetic = r.FormValue("is_synthetic") == "on"

	if err := h.engine.DB().UpdateNodeType(nt); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}

func (h *Handlers) handleNodeTypeDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.engine.DB().DeleteNodeType(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}
