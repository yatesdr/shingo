package www

import (
	"fmt"
	"net/http"
	"strconv"

	"warpath/engine"
	"warpath/store"
)

func (h *Handlers) handleCorrections(w http.ResponseWriter, r *http.Request) {
	corrections, _ := h.engine.DB().ListCorrections(100)
	nodes, _ := h.engine.DB().ListNodes()
	payloads, _ := h.engine.DB().ListPayloads()

	data := map[string]any{
		"Page":          "corrections",
		"Corrections":   corrections,
		"Nodes":         nodes,
		"Payloads":      payloads,
		"Authenticated": h.isAuthenticated(r),
	}
	h.render(w, "corrections.html", data)
}

func (h *Handlers) handleCorrectionCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	corrType := r.FormValue("correction_type")
	nodeID, _ := strconv.ParseInt(r.FormValue("node_id"), 10, 64)
	payloadID, _ := strconv.ParseInt(r.FormValue("payload_id"), 10, 64)
	catID := r.FormValue("cat_id")
	description := r.FormValue("description")
	quantity, _ := strconv.ParseFloat(r.FormValue("quantity"), 64)
	reason := r.FormValue("reason")
	actor := h.getUsername(r)
	if actor == "" {
		actor = "admin"
	}

	corr := &store.Correction{
		CorrectionType: corrType,
		NodeID:         nodeID,
		PayloadID:      &payloadID,
		CatID:          catID,
		Description:    description,
		Quantity:       quantity,
		Reason:         reason,
		Actor:          actor,
	}

	switch corrType {
	case "add_item":
		m := &store.ManifestItem{
			PayloadID:  payloadID,
			PartNumber: catID,
			Quantity:   quantity,
			Notes:      fmt.Sprintf("correction: %s", reason),
		}
		if err := h.engine.DB().CreateManifestItem(m); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		corr.ManifestItemID = &m.ID
	case "remove_item":
		miID, _ := strconv.ParseInt(r.FormValue("manifest_item_id"), 10, 64)
		if err := h.engine.DB().DeleteManifestItem(miID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		corr.ManifestItemID = &miID
	case "adjust_qty":
		miID, _ := strconv.ParseInt(r.FormValue("manifest_item_id"), 10, 64)
		m := &store.ManifestItem{ID: miID, Quantity: quantity, PartNumber: catID}
		if err := h.engine.DB().UpdateManifestItem(m); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		corr.ManifestItemID = &miID
	}

	h.engine.DB().CreateCorrection(corr)

	h.engine.Events.Emit(engine.Event{Type: engine.EventCorrectionApplied, Payload: engine.CorrectionAppliedEvent{
		CorrectionID:   corr.ID,
		CorrectionType: corrType,
		NodeID:         nodeID,
		Reason:         reason,
		Actor:          actor,
	}})

	h.engine.Events.Emit(engine.Event{Type: engine.EventPayloadChanged, Payload: engine.PayloadChangedEvent{
		NodeID:    nodeID,
		Action:    corrType,
		PayloadID: payloadID,
	}})

	http.Redirect(w, r, "/corrections", http.StatusSeeOther)
}
