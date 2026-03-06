package engine

import (
	"shingocore/store"
)

// FindCMSBoundary walks up the parent chain from nodeID to find the nearest
// synthetic ancestor (or self) that has CMS transaction logging enabled.
// Default: parentless synthetic nodes are enabled unless property is "false".
// Child synthetic nodes are disabled unless property is explicitly "true".
func (e *Engine) FindCMSBoundary(nodeID int64) *store.Node {
	visited := make(map[int64]bool)
	currentID := nodeID
	for {
		if visited[currentID] {
			return nil
		}
		visited[currentID] = true

		node, err := e.db.GetNode(currentID)
		if err != nil {
			return nil
		}

		if node.IsSynthetic {
			prop := e.db.GetNodeProperty(node.ID, "log_cms_transactions")
			if node.ParentID == nil {
				if prop != "false" {
					return node
				}
			} else {
				if prop == "true" {
					return node
				}
			}
		}

		if node.ParentID == nil {
			return nil
		}
		currentID = *node.ParentID
	}
}

// txnType returns "increase" or "decrease" based on the sign of delta.
func txnType(delta int64) string {
	if delta >= 0 {
		return "increase"
	}
	return "decrease"
}

// RecordMovementTransactions logs CMS transactions when a payload moves between
// different CMS boundaries. Delta is signed: negative = leaving, positive = arriving.
// QtyBefore/QtyAfter reflect the boundary-level total for each CATID.
func (e *Engine) RecordMovementTransactions(ev PayloadChangedEvent) {
	var srcBoundary, dstBoundary *store.Node
	if ev.FromNodeID != 0 {
		srcBoundary = e.FindCMSBoundary(ev.FromNodeID)
	}
	if ev.ToNodeID != 0 {
		dstBoundary = e.FindCMSBoundary(ev.ToNodeID)
	}

	srcID := int64(0)
	dstID := int64(0)
	if srcBoundary != nil {
		srcID = srcBoundary.ID
	}
	if dstBoundary != nil {
		dstID = dstBoundary.ID
	}
	if srcID == dstID {
		return
	}

	manifest, err := e.db.ListManifestItems(ev.PayloadID)
	if err != nil || len(manifest) == 0 {
		return
	}

	payload, err := e.db.GetPayload(ev.PayloadID)
	if err != nil {
		return
	}

	var orderID *int64
	if payload.ClaimedBy != nil && *payload.ClaimedBy != 0 {
		orderID = payload.ClaimedBy
	}
	binLabel := payload.BinLabel

	var txns []*store.CMSTransaction

	// Source boundary: bin leaving → negative delta.
	// Bin has already moved, so current total excludes it.
	// qty_after = current total, qty_before = current + manifest qty.
	if srcBoundary != nil {
		for _, m := range manifest {
			qty := m.Quantity
			if qty <= 0 {
				continue
			}
			delta := -qty
			qtyAfter := e.db.SumCatIDAtBoundary(srcBoundary.ID, m.PartNumber)
			qtyBefore := qtyAfter + qty
			txns = append(txns, &store.CMSTransaction{
				NodeID:        srcBoundary.ID,
				NodeName:      srcBoundary.Name,
				TxnType:       txnType(delta),
				CatID:         m.PartNumber,
				Delta:         delta,
				QtyBefore:     qtyBefore,
				QtyAfter:      qtyAfter,
				PayloadID:     ev.PayloadID,
				BinID:         payload.BinID,
				BinLabel:      binLabel,
				BlueprintCode: payload.BlueprintCode,
				SourceType:    "movement",
				OrderID:       orderID,
				Notes:         "auto-log",
			})
		}
	}

	// Dest boundary: bin arriving → positive delta.
	// Bin has already moved, so current total includes it.
	// qty_after = current total, qty_before = current - manifest qty.
	if dstBoundary != nil {
		for _, m := range manifest {
			qty := m.Quantity
			if qty <= 0 {
				continue
			}
			delta := qty
			qtyAfter := e.db.SumCatIDAtBoundary(dstBoundary.ID, m.PartNumber)
			qtyBefore := qtyAfter - qty
			if qtyBefore < 0 {
				qtyBefore = 0
			}
			txns = append(txns, &store.CMSTransaction{
				NodeID:        dstBoundary.ID,
				NodeName:      dstBoundary.Name,
				TxnType:       txnType(delta),
				CatID:         m.PartNumber,
				Delta:         delta,
				QtyBefore:     qtyBefore,
				QtyAfter:      qtyAfter,
				PayloadID:     ev.PayloadID,
				BinID:         payload.BinID,
				BinLabel:      binLabel,
				BlueprintCode: payload.BlueprintCode,
				SourceType:    "movement",
				OrderID:       orderID,
				Notes:         "auto-log",
			})
		}
	}

	if len(txns) == 0 {
		return
	}

	if err := e.db.CreateCMSTransactions(txns); err != nil {
		e.logFn("engine: cms transactions: %v", err)
		return
	}

	e.Events.Emit(Event{Type: EventCMSTransaction, Payload: CMSTransactionEvent{Transactions: txns}})
}

// RecordCorrectionTransactions logs CMS adjustment transactions when a manifest
// is edited. Delta is signed: positive = increase, negative = decrease.
// QtyBefore/QtyAfter reflect the boundary-level total for each CATID.
// If the payload has no CMS boundary, logs against the actual node.
func (e *Engine) RecordCorrectionTransactions(payloadID, nodeID int64, oldManifest, newManifest []*store.ManifestItem, reason string) {
	boundary := e.FindCMSBoundary(nodeID)
	var boundaryID int64
	var boundaryName string
	if boundary != nil {
		boundaryID = boundary.ID
		boundaryName = boundary.Name
	} else {
		boundaryID = nodeID
		node, err := e.db.GetNode(nodeID)
		if err != nil {
			return
		}
		boundaryName = node.Name
	}

	oldQty := make(map[string]int64)
	for _, m := range oldManifest {
		oldQty[m.PartNumber] += m.Quantity
	}
	newQty := make(map[string]int64)
	for _, m := range newManifest {
		newQty[m.PartNumber] += m.Quantity
	}

	payload, err := e.db.GetPayload(payloadID)
	if err != nil {
		return
	}
	binLabel := payload.BinLabel

	var txns []*store.CMSTransaction

	allCatIDs := make(map[string]bool)
	for k := range oldQty {
		allCatIDs[k] = true
	}
	for k := range newQty {
		allCatIDs[k] = true
	}

	for catID := range allCatIDs {
		delta := newQty[catID] - oldQty[catID]
		if delta == 0 {
			continue
		}
		qtyAfter := e.db.SumCatIDAtBoundary(boundaryID, catID)
		qtyBefore := qtyAfter - delta
		if qtyBefore < 0 {
			qtyBefore = 0
		}
		txns = append(txns, &store.CMSTransaction{
			NodeID:        boundaryID,
			NodeName:      boundaryName,
			TxnType:       txnType(delta),
			CatID:         catID,
			Delta:         delta,
			QtyBefore:     qtyBefore,
			QtyAfter:      qtyAfter,
			PayloadID:     payloadID,
			BinID:         payload.BinID,
			BinLabel:      binLabel,
			BlueprintCode: payload.BlueprintCode,
			SourceType:    "correction",
			Notes:         reason,
		})
	}

	if len(txns) == 0 {
		return
	}

	if err := e.db.CreateCMSTransactions(txns); err != nil {
		e.logFn("engine: cms correction transactions: %v", err)
		return
	}

	e.Events.Emit(Event{Type: EventCMSTransaction, Payload: CMSTransactionEvent{Transactions: txns}})
}
