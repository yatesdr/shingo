package www

import (
	"encoding/json"
	"net/http"

	"shingoedge/engine"
	"shingoedge/store"
)

// --- Material Slots Admin ---

func (h *Handlers) apiListSlots(w http.ResponseWriter, r *http.Request) {
	slots, err := h.engine.DB().ListSlots()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, slots)
}

func (h *Handlers) apiListSlotsByStyle(w http.ResponseWriter, r *http.Request) {
	styleID, err := parseID(r, "styleID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid style ID")
		return
	}
	slots, err := h.engine.DB().ListSlotsByStyle(styleID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, slots)
}

func (h *Handlers) apiCreateSlot(w http.ResponseWriter, r *http.Request) {
	var input store.MaterialSlotInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := h.engine.DB().CreateSlot(input)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("slot-created")
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdateSlot(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var input store.MaterialSlotInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.DB().UpdateSlot(id, input); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("slot-updated")
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeleteSlot(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	if err := h.engine.DB().DeleteSlot(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("slot-deleted")
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiSlotCount(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		PieceCount float64 `json:"piece_count"`
		Reset      bool    `json:"reset"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	p, err := h.engine.DB().GetSlot(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "material slot not found")
		return
	}

	// Look up UOP capacity from catalog (single source of truth)
	bp, err := h.engine.DB().GetPayloadCatalogByCode(p.PayloadCode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "payload catalog entry not found for code: "+p.PayloadCode)
		return
	}

	var prodUnits int
	var status string

	if req.Reset {
		if err := h.engine.DB().ResetSlot(id, bp.UOPCapacity); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		prodUnits = bp.UOPCapacity
		status = "active"
	} else {
		// Multiplier is always 1 — piece count = production units
		prodUnits = int(req.PieceCount)
		if prodUnits < 0 {
			prodUnits = 0
		}

		status = p.Status
		if prodUnits == 0 {
			status = "empty"
		} else if prodUnits <= p.ReorderPoint {
			status = "replenishing"
		} else {
			status = "active"
		}

		if err := h.engine.DB().UpdateSlotRemaining(id, prodUnits, status); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	h.eng.Events.Emit(engine.Event{
		Type: engine.EventSlotUpdated,
		Payload: engine.SlotUpdatedEvent{
			PayloadID:    id,
			JobStyleID:   p.StyleID,
			Location:     p.Location,
			OldRemaining: p.Remaining,
			NewRemaining: prodUnits,
			Status:       status,
		},
	})

	writeJSON(w, map[string]interface{}{
		"status":           "ok",
		"production_units": prodUnits,
		"payload_status":   status,
	})
}

// --- Public Slot Read (for operator displays) ---

func (h *Handlers) apiListSlotsByProcessPublic(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "processID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process ID")
		return
	}
	process, err := h.engine.DB().GetProcess(processID)
	if err != nil {
		writeError(w, http.StatusNotFound, "process not found")
		return
	}
	if process.ActiveStyleID == nil {
		writeJSON(w, []interface{}{})
		return
	}
	slots, err := h.engine.DB().ListSlotsByStyle(*process.ActiveStyleID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, slots)
}
