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
	materials, _ := h.engine.DB().ListMaterials()

	data := map[string]any{
		"Page":          "corrections",
		"Corrections":   corrections,
		"Nodes":         nodes,
		"Materials":     materials,
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
	materialID, _ := strconv.ParseInt(r.FormValue("material_id"), 10, 64)
	quantity, _ := strconv.ParseFloat(r.FormValue("quantity"), 64)
	reason := r.FormValue("reason")
	actor := h.getUsername(r)
	if actor == "" {
		actor = "admin"
	}

	corr := &store.Correction{
		CorrectionType: corrType,
		NodeID:         nodeID,
		MaterialID:     &materialID,
		Quantity:        quantity,
		Reason:         reason,
		Actor:          actor,
	}

	switch corrType {
	case "add":
		invID, err := h.engine.NodeState().AddInventory(nodeID, materialID, quantity, false, nil, fmt.Sprintf("correction: %s", reason))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		corr.InventoryID = &invID
	case "remove":
		invID, _ := strconv.ParseInt(r.FormValue("inventory_id"), 10, 64)
		if err := h.engine.NodeState().RemoveInventory(invID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		corr.InventoryID = &invID
	case "adjust":
		invID, _ := strconv.ParseInt(r.FormValue("inventory_id"), 10, 64)
		if err := h.engine.NodeState().AdjustQuantity(invID, quantity); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		corr.InventoryID = &invID
	case "move":
		invID, _ := strconv.ParseInt(r.FormValue("inventory_id"), 10, 64)
		toNodeID, _ := strconv.ParseInt(r.FormValue("to_node_id"), 10, 64)
		if err := h.engine.NodeState().MoveInventory(invID, toNodeID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		corr.InventoryID = &invID
	}

	h.engine.DB().CreateCorrection(corr)

	h.engine.Events.Emit(engine.Event{Type: engine.EventCorrectionApplied, Payload: engine.CorrectionAppliedEvent{
		CorrectionID:   corr.ID,
		CorrectionType: corrType,
		NodeID:         nodeID,
		Reason:         reason,
		Actor:          actor,
	}})

	h.engine.Events.Emit(engine.Event{Type: engine.EventInventoryChanged, Payload: engine.InventoryChangedEvent{
		NodeID:   nodeID,
		Action:   corrType,
		Quantity: quantity,
	}})

	http.Redirect(w, r, "/corrections", http.StatusSeeOther)
}
