package engine

import (
	"encoding/json"
	"fmt"

	"shingocore/store/bins"
	"shingocore/store/inventory"
)

// ApplyCorrectionRequest holds the parameters for applying an inventory correction.
type ApplyCorrectionRequest struct {
	CorrectionType string
	NodeID         int64
	BinID          int64
	CatID          string
	Description    string
	Quantity       int64
	Reason         string
	Actor          string
}

// ApplyCorrection executes a correction (add_item, remove_item, adjust_qty),
// updates the bin's manifest JSON, records the correction, and emits events.
func (e *Engine) ApplyCorrection(req ApplyCorrectionRequest) (int64, error) {
	corr := &inventory.Correction{
		CorrectionType: req.CorrectionType,
		NodeID:         req.NodeID,
		BinID:          &req.BinID,
		CatID:          req.CatID,
		Description:    req.Description,
		Quantity:       req.Quantity,
		Reason:         req.Reason,
		Actor:          req.Actor,
	}

	// Get bin and parse its manifest
	bin, err := e.db.GetBin(req.BinID)
	if err != nil {
		return 0, fmt.Errorf("get bin: %w", err)
	}
	manifest, err := bin.ParseManifest()
	if err != nil {
		return 0, fmt.Errorf("parse bin manifest: %w", err)
	}

	switch req.CorrectionType {
	case "add_item":
		manifest.Items = append(manifest.Items, bins.ManifestEntry{
			CatID:    req.CatID,
			Quantity: req.Quantity,
			Notes:    fmt.Sprintf("correction: %s", req.Reason),
		})
	case "remove_item":
		var filtered []bins.ManifestEntry
		removed := false
		for _, item := range manifest.Items {
			if !removed && item.CatID == req.CatID {
				removed = true
				continue
			}
			filtered = append(filtered, item)
		}
		manifest.Items = filtered
	case "adjust_qty":
		for i := range manifest.Items {
			if manifest.Items[i].CatID == req.CatID {
				manifest.Items[i].Quantity = req.Quantity
				break
			}
		}
	}

	// Save updated manifest
	manifestJSON, _ := json.Marshal(manifest)
	if err := e.binManifest.SetForProduction(req.BinID, string(manifestJSON), bin.PayloadCode, bin.UOPRemaining); err != nil {
		return 0, fmt.Errorf("update bin manifest: %w", err)
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

	e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
		NodeID:  req.NodeID,
		Action:  req.CorrectionType,
		BinID:   req.BinID,
	}})

	return corr.ID, nil
}

// BatchCorrectionRequest holds the parameters for a batch manifest correction.
type BatchCorrectionRequest struct {
	BinID  int64
	NodeID int64
	Reason string
	Actor  string
	Items  []BatchCorrectionItem
}

// BatchCorrectionItem represents a single manifest item in a batch correction.
type BatchCorrectionItem struct {
	CatID    string
	Quantity int64
}

// ApplyBatchCorrection diffs submitted items against current manifest and applies
// all changes, recording corrections.
func (e *Engine) ApplyBatchCorrection(req BatchCorrectionRequest) error {
	bin, err := e.db.GetBin(req.BinID)
	if err != nil {
		return fmt.Errorf("get bin: %w", err)
	}
	oldManifest, err := bin.ParseManifest()
	if err != nil {
		return fmt.Errorf("parse bin manifest: %w", err)
	}
	oldItems := oldManifest.Items

	// Build new manifest from submitted items
	newItems := make([]bins.ManifestEntry, len(req.Items))
	for i, item := range req.Items {
		newItems[i] = bins.ManifestEntry{
			CatID:    item.CatID,
			Quantity: item.Quantity,
		}
	}

	// Build corrections by diffing old vs new
	var corrections []*inventory.Correction
	oldQty := make(map[string]int64)
	for _, m := range oldItems {
		oldQty[m.CatID] += m.Quantity
	}
	newQty := make(map[string]int64)
	for _, m := range newItems {
		newQty[m.CatID] += m.Quantity
	}

	allCatIDs := make(map[string]bool)
	for k := range oldQty {
		allCatIDs[k] = true
	}
	for k := range newQty {
		allCatIDs[k] = true
	}

	for catID := range allCatIDs {
		oq := oldQty[catID]
		nq := newQty[catID]
		if oq == nq {
			continue
		}
		corrType := "adjust_qty"
		if oq == 0 {
			corrType = "add_item"
		} else if nq == 0 {
			corrType = "remove_item"
		}
		corrections = append(corrections, &inventory.Correction{
			CorrectionType: corrType,
			NodeID:         req.NodeID,
			BinID:          &req.BinID,
			CatID:          catID,
			Description:    fmt.Sprintf("was: qty %d", oq),
			Quantity:       nq,
			Reason:         req.Reason,
			Actor:          req.Actor,
		})
	}

	if len(corrections) == 0 {
		return nil // no changes
	}

	// Save the new manifest on the bin
	newManifest := bins.Manifest{Items: newItems}
	manifestJSON, _ := json.Marshal(newManifest)
	if err := e.binManifest.SetForProduction(req.BinID, string(manifestJSON), bin.PayloadCode, bin.UOPRemaining); err != nil {
		return fmt.Errorf("update bin manifest: %w", err)
	}

	// Record corrections
	if err := e.db.ApplyBinManifestChanges(req.BinID, corrections); err != nil {
		return fmt.Errorf("apply corrections: %w", err)
	}

	// Record CMS adjustment transactions
	e.RecordCorrectionTransactions(req.BinID, req.NodeID, oldItems, newItems, req.Reason)

	e.Events.Emit(Event{Type: EventCorrectionApplied, Payload: CorrectionAppliedEvent{
		CorrectionType: "batch",
		NodeID:         req.NodeID,
		Reason:         req.Reason,
		Actor:          req.Actor,
	}})

	e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
		NodeID: req.NodeID,
		Action: "batch_correction",
		BinID:  req.BinID,
	}})

	return nil
}
