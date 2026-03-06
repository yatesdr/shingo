package engine

import (
	"fmt"

	"shingocore/store"
)

// ApplyCorrectionRequest holds the parameters for applying an inventory correction.
type ApplyCorrectionRequest struct {
	CorrectionType string
	NodeID         int64
	PayloadID     int64
	CatID          string
	Description    string
	Quantity       int64
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
		PayloadID:     &req.PayloadID,
		CatID:          req.CatID,
		Description:    req.Description,
		Quantity:       req.Quantity,
		Reason:         req.Reason,
		Actor:          req.Actor,
	}

	switch req.CorrectionType {
	case "add_item":
		m := &store.ManifestItem{
			PayloadID: req.PayloadID,
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
		PayloadID: req.PayloadID,
	}})

	return corr.ID, nil
}

// BatchCorrectionRequest holds the parameters for a batch manifest correction.
type BatchCorrectionRequest struct {
	PayloadID int64
	NodeID    int64
	Reason    string
	Actor     string
	Items     []BatchCorrectionItem
}

// BatchCorrectionItem represents a single manifest item in a batch correction.
type BatchCorrectionItem struct {
	ID       int64  // 0 = new item
	CatID    string
	Quantity int64
}

// ApplyBatchCorrection diffs submitted items against current manifest and applies
// all adds, updates, and deletes in a single transaction.
func (e *Engine) ApplyBatchCorrection(req BatchCorrectionRequest) error {
	current, err := e.db.ListManifestItems(req.PayloadID)
	if err != nil {
		return fmt.Errorf("list manifest items: %w", err)
	}

	currentByID := make(map[int64]*store.ManifestItem, len(current))
	for _, m := range current {
		currentByID[m.ID] = m
	}

	submittedIDs := make(map[int64]bool)

	var adds []*store.ManifestItem
	var updates []*store.ManifestItem
	var corrections []*store.Correction

	for _, item := range req.Items {
		if item.ID == 0 {
			// New item
			adds = append(adds, &store.ManifestItem{
				PayloadID:  req.PayloadID,
				PartNumber: item.CatID,
				Quantity:   item.Quantity,
				Notes:      fmt.Sprintf("correction: %s", req.Reason),
			})
			corrections = append(corrections, &store.Correction{
				CorrectionType: "add_item",
				NodeID:         req.NodeID,
				PayloadID:      &req.PayloadID,
				CatID:          item.CatID,
				Quantity:        item.Quantity,
				Reason:         req.Reason,
				Actor:          req.Actor,
			})
		} else {
			submittedIDs[item.ID] = true
			existing, ok := currentByID[item.ID]
			if !ok {
				continue
			}
			if existing.PartNumber != item.CatID || existing.Quantity != item.Quantity {
				updates = append(updates, &store.ManifestItem{
					ID:         item.ID,
					PartNumber: item.CatID,
					Quantity:   item.Quantity,
				})
				corrections = append(corrections, &store.Correction{
					CorrectionType: "adjust_qty",
					NodeID:         req.NodeID,
					PayloadID:      &req.PayloadID,
					ManifestItemID: &item.ID,
					CatID:          item.CatID,
					Description:    fmt.Sprintf("was: %s qty %d", existing.PartNumber, existing.Quantity),
					Quantity:        item.Quantity,
					Reason:         req.Reason,
					Actor:          req.Actor,
				})
			}
		}
	}

	// Items in current but not in submitted → delete
	var deleteIDs []int64
	for _, m := range current {
		if !submittedIDs[m.ID] {
			deleteIDs = append(deleteIDs, m.ID)
			corrections = append(corrections, &store.Correction{
				CorrectionType: "remove_item",
				NodeID:         req.NodeID,
				PayloadID:      &req.PayloadID,
				ManifestItemID: &m.ID,
				CatID:          m.PartNumber,
				Quantity:        m.Quantity,
				Reason:         req.Reason,
				Actor:          req.Actor,
			})
		}
	}

	if len(adds) == 0 && len(updates) == 0 && len(deleteIDs) == 0 {
		return nil // no changes
	}

	// Snapshot old manifest for CMS transaction delta calculation
	oldManifest := make([]*store.ManifestItem, len(current))
	copy(oldManifest, current)

	if err := e.db.ApplyBatchManifestChanges(req.PayloadID, adds, updates, deleteIDs, corrections); err != nil {
		return fmt.Errorf("apply batch changes: %w", err)
	}

	// Record CMS adjustment transactions
	newManifest, _ := e.db.ListManifestItems(req.PayloadID)
	e.RecordCorrectionTransactions(req.PayloadID, req.NodeID, oldManifest, newManifest, req.Reason)

	e.Events.Emit(Event{Type: EventCorrectionApplied, Payload: CorrectionAppliedEvent{
		CorrectionType: "batch",
		NodeID:         req.NodeID,
		Reason:         req.Reason,
		Actor:          req.Actor,
	}})

	e.Events.Emit(Event{Type: EventPayloadChanged, Payload: PayloadChangedEvent{
		NodeID:    req.NodeID,
		Action:    "batch_correction",
		PayloadID: req.PayloadID,
	}})

	return nil
}
