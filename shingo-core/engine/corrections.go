package engine

import (
	"fmt"

	"shingocore/store"
)

// ApplyCorrectionRequest holds the parameters for applying an inventory correction.
type ApplyCorrectionRequest struct {
	CorrectionType string
	NodeID         int64
	InstanceID     int64
	CatID          string
	Description    string
	Quantity       float64
	Reason         string
	ManifestItemID int64
	Actor          string
}

// ApplyCorrection executes a correction (add_item, remove_item, adjust_qty),
// updates manifest items, records the correction, and emits events.
func (e *Engine) ApplyCorrection(req ApplyCorrectionRequest) (int64, error) {
	corr := &store.Correction{
		CorrectionType: req.CorrectionType,
		NodeID:         req.NodeID,
		InstanceID:     &req.InstanceID,
		CatID:          req.CatID,
		Description:    req.Description,
		Quantity:       req.Quantity,
		Reason:         req.Reason,
		Actor:          req.Actor,
	}

	switch req.CorrectionType {
	case "add_item":
		m := &store.ManifestItem{
			InstanceID: req.InstanceID,
			PartNumber: req.CatID,
			Quantity:   req.Quantity,
			Notes:      fmt.Sprintf("correction: %s", req.Reason),
		}
		if err := e.db.CreateManifestItem(m); err != nil {
			return 0, fmt.Errorf("create manifest item: %w", err)
		}
		corr.ManifestItemID = &m.ID
	case "remove_item":
		if err := e.db.DeleteManifestItem(req.ManifestItemID); err != nil {
			return 0, fmt.Errorf("delete manifest item: %w", err)
		}
		corr.ManifestItemID = &req.ManifestItemID
	case "adjust_qty":
		m := &store.ManifestItem{ID: req.ManifestItemID, Quantity: req.Quantity, PartNumber: req.CatID}
		if err := e.db.UpdateManifestItem(m); err != nil {
			return 0, fmt.Errorf("update manifest item: %w", err)
		}
		corr.ManifestItemID = &req.ManifestItemID
	}

	if err := e.db.CreateCorrection(corr); err != nil {
		return 0, fmt.Errorf("save correction: %w", err)
	}

	e.Events.Emit(Event{Type: EventCorrectionApplied, Payload: CorrectionAppliedEvent{
		CorrectionID:   corr.ID,
		CorrectionType: req.CorrectionType,
		NodeID:         req.NodeID,
		Reason:         req.Reason,
		Actor:          req.Actor,
	}})

	e.Events.Emit(Event{Type: EventPayloadChanged, Payload: PayloadChangedEvent{
		NodeID:     req.NodeID,
		Action:     req.CorrectionType,
		InstanceID: req.InstanceID,
	}})

	return corr.ID, nil
}
