package www

import (
	"fmt"
	"net/http"
	"strconv"

	"shingocore/store"
)

func (h *Handlers) handleInventory(w http.ResponseWriter, r *http.Request) {
	instances, _ := h.engine.DB().ListInstances()
	styles, _ := h.engine.DB().ListPayloadStyles()
	nodes, _ := h.engine.DB().ListNodes()

	data := map[string]any{
		"Page":           "inventory",
		"Instances":      instances,
		"PayloadStyles":  styles,
		"Nodes":          nodes,
	}
	h.render(w, r, "inventory.html", data)
}

func (h *Handlers) apiInstanceAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     int64  `json:"id"`
		Action string `json:"action"`
		Reason string `json:"reason"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	inst, err := h.engine.DB().GetInstance(req.ID)
	if err != nil {
		h.jsonError(w, "instance not found", http.StatusNotFound)
		return
	}

	switch req.Action {
	case "flag":
		inst.Status = "flagged"
	case "maintenance":
		inst.Status = "maintenance"
	case "retire":
		inst.Status = "retired"
	case "activate":
		inst.Status = "available"
	default:
		h.jsonError(w, "unknown action: "+req.Action, http.StatusBadRequest)
		return
	}

	inst.Notes = req.Reason
	if err := h.engine.DB().UpdateInstance(inst); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiBulkRegisterInstances(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StyleID   int64  `json:"style_id"`
		Count     int    `json:"count"`
		TagPrefix string `json:"tag_prefix"`
		Status    string `json:"status"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	if req.Count <= 0 || req.Count > 100 {
		h.jsonError(w, "count must be 1-100", http.StatusBadRequest)
		return
	}
	if req.Status == "" {
		req.Status = "empty"
	}

	var created []int64
	for i := 0; i < req.Count; i++ {
		inst := &store.PayloadInstance{
			StyleID: req.StyleID,
			Status:  req.Status,
		}
		if req.TagPrefix != "" {
			inst.TagID = fmt.Sprintf("%s%04d", req.TagPrefix, i+1)
		}
		if err := h.engine.DB().CreateInstance(inst); err != nil {
			h.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		created = append(created, inst.ID)
	}
	h.jsonOK(w, map[string]any{"created": len(created), "ids": created})
}

func (h *Handlers) apiListInstanceEvents(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	events, err := h.engine.DB().ListInstanceEvents(id, 50)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, events)
}
