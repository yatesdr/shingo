// handlers_api_replenishment.go — API for the replenishment admin page.
//
// PUT /api/replenishment/cell-reorder — update a style_node_claim's
// reorder_point / source / auto_reorder.
//
// That is the whole surface. The loader-threshold half of this file
// (upsert / delete / calculate / calculate-and-apply / override /
// recalculate-all) was deleted: Core owns the loader UOP threshold, and the
// Edge write path terminated in SendClaimSync(), a no-op stub retired when
// Core took ownership of the loader aggregate. Those endpoints accepted
// input, persisted it to an Edge-only table nothing read, and reported
// success. See engine/replenishment_admin.go for the full rationale.
//
// The cell half is live and genuinely Edge-owned: handleConsumeTick reads
// reorder_point / auto_reorder on every PLC tick to fire auto-reorder.

package www

import (
	"encoding/json"
	"net/http"

	"shingoedge/engine"
)

type apiCellReorderReq struct {
	ClaimID      int64  `json:"claim_id"`
	ReorderPoint int    `json:"reorder_point"`
	Source       string `json:"source"`
	AutoReorder  bool   `json:"auto_reorder"`
}

func (h *Handlers) apiUpdateCellReorder(w http.ResponseWriter, r *http.Request) {
	var req apiCellReorderReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	// WHO MADE THIS EDIT — the handler's answer, from the authenticated
	// session, never the body's (owner ruling R1). This is the third door that
	// writes a claim and it used to stamp nothing, which the store reads as
	// "a person on the desktop" with no name; a flow saved at Press 400 then
	// read "saved from the desktop" on the set-up card after an engineer
	// nudged its reorder point.
	calledBy, _ := h.sessions.getUser(r)
	if err := h.orchestration.UpdateCellReorder(engine.CellReorderInput{
		ClaimID:      req.ClaimID,
		ReorderPoint: req.ReorderPoint,
		Source:       req.Source,
		AutoReorder:  req.AutoReorder,
		CalledBy:     calledBy,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
