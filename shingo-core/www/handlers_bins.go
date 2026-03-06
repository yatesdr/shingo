package www

import (
	"fmt"
	"net/http"
	"strconv"

	"shingocore/store"
)

func (h *Handlers) handleBinTypeCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	widthIn, _ := strconv.ParseFloat(r.FormValue("width_in"), 64)
	heightIn, _ := strconv.ParseFloat(r.FormValue("height_in"), 64)

	bt := &store.BinType{
		Code:        r.FormValue("code"),
		Description: r.FormValue("description"),
		WidthIn:     widthIn,
		HeightIn:    heightIn,
	}

	if err := h.engine.DB().CreateBinType(bt); err != nil {
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

	bt, err := h.engine.DB().GetBinType(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	bt.Code = r.FormValue("code")
	bt.Description = r.FormValue("description")
	bt.WidthIn, _ = strconv.ParseFloat(r.FormValue("width_in"), 64)
	bt.HeightIn, _ = strconv.ParseFloat(r.FormValue("height_in"), 64)

	if err := h.engine.DB().UpdateBinType(bt); err != nil {
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

	if err := h.engine.DB().DeleteBinType(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/bins", http.StatusSeeOther)
}

func (h *Handlers) handleBins(w http.ResponseWriter, r *http.Request) {
	bins, _ := h.engine.DB().ListBins()
	binTypes, _ := h.engine.DB().ListBinTypes()
	nodes, _ := h.engine.DB().ListNodes()

	data := map[string]any{
		"Page":     "bins",
		"Bins":     bins,
		"BinTypes": binTypes,
		"Nodes":    nodes,
	}
	h.render(w, r, "bins.html", data)
}

func (h *Handlers) handleBinCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	binTypeID, err := strconv.ParseInt(r.FormValue("bin_type_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid bin type", http.StatusBadRequest)
		return
	}

	count, _ := strconv.Atoi(r.FormValue("quantity"))
	if count <= 0 {
		count = 1
	}

	label := r.FormValue("label_prefix")
	status := r.FormValue("status")
	if status == "" {
		status = "available"
	}

	var nodeID *int64
	if nStr := r.FormValue("node_id"); nStr != "" {
		if nid, err := strconv.ParseInt(nStr, 10, 64); err == nil {
			nodeID = &nid
		}
	}

	for i := 0; i < count; i++ {
		binLabel := label
		if count > 1 {
			binLabel = label + fmt.Sprintf("%04d", i+1)
		}
		b := &store.Bin{
			BinTypeID: binTypeID,
			Label:     binLabel,
			NodeID:    nodeID,
			Status:    status,
		}
		if err := h.engine.DB().CreateBin(b); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	http.Redirect(w, r, "/bins", http.StatusSeeOther)
}

func (h *Handlers) handleBinUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	b, err := h.engine.DB().GetBin(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	binTypeID, err := strconv.ParseInt(r.FormValue("bin_type_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid bin type", http.StatusBadRequest)
		return
	}

	b.BinTypeID = binTypeID
	b.Label = r.FormValue("label")
	b.Description = r.FormValue("description")
	b.Status = r.FormValue("status")
	b.NodeID = nil

	if nStr := r.FormValue("node_id"); nStr != "" {
		if nid, err := strconv.ParseInt(nStr, 10, 64); err == nil {
			b.NodeID = &nid
		}
	}

	if err := h.engine.DB().UpdateBin(b); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/bins", http.StatusSeeOther)
}

func (h *Handlers) handleBinDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.engine.DB().DeleteBin(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/bins", http.StatusSeeOther)
}

func (h *Handlers) apiBinAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     int64  `json:"id"`
		Action string `json:"action"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	b, err := h.engine.DB().GetBin(req.ID)
	if err != nil {
		h.jsonError(w, "bin not found", http.StatusNotFound)
		return
	}

	switch req.Action {
	case "maintenance":
		b.Status = "maintenance"
	case "retire":
		b.Status = "retired"
	case "activate":
		b.Status = "available"
	case "release":
		if err := h.engine.DB().ReleaseStagedBin(req.ID); err != nil {
			h.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.jsonSuccess(w)
		return
	default:
		h.jsonError(w, "unknown action: "+req.Action, http.StatusBadRequest)
		return
	}

	if err := h.engine.DB().UpdateBin(b); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiBinsByNode(w http.ResponseWriter, r *http.Request) {
	id, ok := h.parseIDParam(w, r, "id")
	if !ok {
		return
	}
	bins, err := h.engine.DB().ListBinsByNode(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, bins)
}
