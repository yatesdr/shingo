package www

import (
	"net/http"
	"strconv"

	"shingocore/store"
)

func (h *Handlers) apiListPayloadStyles(w http.ResponseWriter, r *http.Request) {
	styles, err := h.engine.DB().ListPayloadStyles()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, styles)
}

func (h *Handlers) handlePayloads(w http.ResponseWriter, r *http.Request) {
	instances, _ := h.engine.DB().ListInstances()
	styles, _ := h.engine.DB().ListPayloadStyles()
	nodes, _ := h.engine.DB().ListNodes()

	// Build compatible nodes map: style_id -> [node names]
	compatNodes := make(map[int64][]string)
	for _, ps := range styles {
		nodeList, _ := h.engine.DB().ListNodesForPayloadStyle(ps.ID)
		for _, n := range nodeList {
			compatNodes[ps.ID] = append(compatNodes[ps.ID], n.Name)
		}
	}

	data := map[string]any{
		"Page":          "payloads",
		"Instances":     instances,
		"PayloadStyles": styles,
		"Nodes":         nodes,
		"CompatNodes":   compatNodes,
	}
	h.render(w, r, "payloads.html", data)
}

func (h *Handlers) handleInstanceCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	styleID, err := strconv.ParseInt(r.FormValue("style_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid payload style", http.StatusBadRequest)
		return
	}

	p := &store.PayloadInstance{
		StyleID: styleID,
		TagID:   r.FormValue("tag_id"),
		Status:  r.FormValue("status"),
		Notes:   r.FormValue("notes"),
	}

	if nodeStr := r.FormValue("node_id"); nodeStr != "" {
		if nid, err := strconv.ParseInt(nodeStr, 10, 64); err == nil {
			p.NodeID = &nid
		}
	}

	if p.Status == "" {
		p.Status = "empty"
	}

	if err := h.engine.DB().CreateInstance(p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/payloads", http.StatusSeeOther)
}

func (h *Handlers) handleInstanceUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	p, err := h.engine.DB().GetInstance(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	styleID, err := strconv.ParseInt(r.FormValue("style_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid payload style", http.StatusBadRequest)
		return
	}

	p.StyleID = styleID
	p.TagID = r.FormValue("tag_id")
	p.Status = r.FormValue("status")
	p.Notes = r.FormValue("notes")
	p.NodeID = nil

	if nodeStr := r.FormValue("node_id"); nodeStr != "" {
		if nid, err := strconv.ParseInt(nodeStr, 10, 64); err == nil {
			p.NodeID = &nid
		}
	}

	if err := h.engine.DB().UpdateInstance(p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/payloads", http.StatusSeeOther)
}

func (h *Handlers) handleInstanceDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.engine.DB().DeleteInstance(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/payloads", http.StatusSeeOther)
}

func (h *Handlers) apiListInstances(w http.ResponseWriter, r *http.Request) {
	instances, err := h.engine.DB().ListInstances()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, instances)
}

func (h *Handlers) apiGetInstance(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	p, err := h.engine.DB().GetInstance(id)
	if err != nil {
		h.jsonError(w, "not found", http.StatusNotFound)
		return
	}
	h.jsonOK(w, p)
}

func (h *Handlers) apiListManifest(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	items, err := h.engine.DB().ListManifestItems(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, items)
}

func (h *Handlers) apiCreateManifestItem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InstanceID     int64   `json:"instance_id"`
		PartNumber     string  `json:"part_number"`
		Quantity       float64 `json:"quantity"`
		ProductionDate string  `json:"production_date"`
		LotCode        string  `json:"lot_code"`
		Notes          string  `json:"notes"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	m := &store.ManifestItem{
		InstanceID:     req.InstanceID,
		PartNumber:     req.PartNumber,
		Quantity:       req.Quantity,
		ProductionDate: req.ProductionDate,
		LotCode:        req.LotCode,
		Notes:          req.Notes,
	}
	if err := h.engine.DB().CreateManifestItem(m); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, m)
}

func (h *Handlers) apiUpdateManifestItem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID             int64   `json:"id"`
		PartNumber     string  `json:"part_number"`
		Quantity       float64 `json:"quantity"`
		ProductionDate string  `json:"production_date"`
		LotCode        string  `json:"lot_code"`
		Notes          string  `json:"notes"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	m := &store.ManifestItem{
		ID:             req.ID,
		PartNumber:     req.PartNumber,
		Quantity:       req.Quantity,
		ProductionDate: req.ProductionDate,
		LotCode:        req.LotCode,
		Notes:          req.Notes,
	}
	if err := h.engine.DB().UpdateManifestItem(m); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiDeleteManifestItem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if err := h.engine.DB().DeleteManifestItem(req.ID); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}
