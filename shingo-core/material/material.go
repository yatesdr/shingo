package material

import (
	"errors"

	"shingocore/store/bins"
	"shingocore/store/cms"
	"shingocore/store/nodes"
)

// errCMSBoundaryCycle is returned by FindCMSBoundary when the parent
// chain revisits a node. The engine wrapper logs this and falls back
// to "no boundary" so callers see the same nil-node behaviour they
// got when this logic lived on *Engine.
var errCMSBoundaryCycle = errors.New("cms boundary: parent chain cycle")

// MovementEvent is the minimal bin-movement payload the material
// package needs in order to log a movement between CMS boundaries.
// Mirrors the subset of engine.BinUpdatedEvent used by the old
// RecordMovementTransactions — BinID, FromNodeID, ToNodeID — so the
// engine wrapper can build one from its BinUpdatedEvent without
// pulling in the whole engine type.
type MovementEvent struct {
	BinID      int64
	FromNodeID int64
	ToNodeID   int64
}

// FindCMSBoundary walks up the parent chain from nodeID to find the
// nearest synthetic ancestor (or self) that has CMS transaction
// logging enabled via the "log_cms_transactions" node property.
//
// Defaults:
//   - parentless (root) synthetic nodes are enabled unless the
//     property is explicitly "false";
//   - child synthetic nodes are disabled unless the property is
//     explicitly "true".
//
// Returns (nil, nil) if the walk reaches a root without finding a
// logging boundary. Returns (nil, err) if a Store call fails or the
// walk detects a cycle, so callers can distinguish "no boundary
// here" from "something went wrong on the way up".
func FindCMSBoundary(s Store, nodeID int64) (*nodes.Node, error) {
	visited := make(map[int64]bool)
	currentID := nodeID
	for {
		if visited[currentID] {
			return nil, errCMSBoundaryCycle
		}
		visited[currentID] = true

		node, err := s.GetNode(currentID)
		if err != nil {
			return nil, err
		}

		if node.IsSynthetic {
			prop := s.GetNodeProperty(node.ID, "log_cms_transactions")
			if node.ParentID == nil {
				if prop != "false" {
					return node, nil
				}
			} else {
				if prop == "true" {
					return node, nil
				}
			}
		}

		if node.ParentID == nil {
			return nil, nil
		}
		currentID = *node.ParentID
	}
}

// txnType returns "increase" or "decrease" based on the sign of
// delta. Zero is treated as an increase; callers that want to skip
// zero-delta rows filter them out before calling this.
func txnType(delta int64) string {
	if delta >= 0 {
		return "increase"
	}
	return "decrease"
}

// BuildMovementTransactions returns the CMS transaction rows that
// should be recorded when a bin moves between two nodes whose CMS
// boundaries differ. Negative deltas represent the bin leaving the
// source boundary; positive deltas represent arrival at the
// destination.
//
// Returns a nil slice when nothing needs to be recorded (source and
// destination resolve to the same boundary, the bin has no manifest
// items, or neither endpoint has a boundary). The caller should
// persist and emit for a non-nil slice; a nil slice and nil error
// means "no-op, carry on".
func BuildMovementTransactions(s Store, ev MovementEvent) ([]*cms.Transaction, error) {
	var srcBoundary, dstBoundary *nodes.Node
	if ev.FromNodeID != 0 {
		b, err := FindCMSBoundary(s, ev.FromNodeID)
		if err != nil {
			return nil, err
		}
		srcBoundary = b
	}
	if ev.ToNodeID != 0 {
		b, err := FindCMSBoundary(s, ev.ToNodeID)
		if err != nil {
			return nil, err
		}
		dstBoundary = b
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
		return nil, nil
	}

	bin, err := s.GetBin(ev.BinID)
	if err != nil {
		return nil, err
	}

	parsed, _ := bin.ParseManifest()
	if parsed == nil || len(parsed.Items) == 0 {
		return nil, nil
	}

	var orderID *int64
	if bin.ClaimedBy != nil && *bin.ClaimedBy != 0 {
		orderID = bin.ClaimedBy
	}

	var txns []*cms.Transaction

	// Source boundary: bin leaving → negative delta.
	if srcBoundary != nil {
		totals := s.SumCatIDsAtBoundary(srcBoundary.ID)
		for _, m := range parsed.Items {
			if m.Quantity <= 0 {
				continue
			}
			delta := -m.Quantity
			qtyAfter := totals[m.CatID]
			qtyBefore := qtyAfter + m.Quantity
			txns = append(txns, &cms.Transaction{
				NodeID:      srcBoundary.ID,
				NodeName:    srcBoundary.Name,
				TxnType:     txnType(delta),
				CatID:       m.CatID,
				Delta:       delta,
				QtyBefore:   qtyBefore,
				QtyAfter:    qtyAfter,
				BinID:       &bin.ID,
				BinLabel:    bin.Label,
				PayloadCode: bin.PayloadCode,
				SourceType:  "movement",
				OrderID:     orderID,
				Notes:       "auto-log",
			})
		}
	}

	// Dest boundary: bin arriving → positive delta.
	if dstBoundary != nil {
		totals := s.SumCatIDsAtBoundary(dstBoundary.ID)
		for _, m := range parsed.Items {
			if m.Quantity <= 0 {
				continue
			}
			delta := m.Quantity
			qtyAfter := totals[m.CatID]
			qtyBefore := qtyAfter - m.Quantity
			if qtyBefore < 0 {
				qtyBefore = 0
			}
			txns = append(txns, &cms.Transaction{
				NodeID:      dstBoundary.ID,
				NodeName:    dstBoundary.Name,
				TxnType:     txnType(delta),
				CatID:       m.CatID,
				Delta:       delta,
				QtyBefore:   qtyBefore,
				QtyAfter:    qtyAfter,
				BinID:       &bin.ID,
				BinLabel:    bin.Label,
				PayloadCode: bin.PayloadCode,
				SourceType:  "movement",
				OrderID:     orderID,
				Notes:       "auto-log",
			})
		}
	}

	if len(txns) == 0 {
		return nil, nil
	}
	return txns, nil
}

// BuildCorrectionTransactions returns the CMS adjustment rows that
// should be recorded when a bin's manifest is edited in place. Old
// and new manifests are diffed by CatID; only non-zero deltas
// produce rows. Reason is copied into CMSTransaction.Notes.
//
// If no boundary is found, the correction is still logged against
// nodeID itself (falling back to the node's own name), mirroring the
// behaviour of the old engine method — corrections at a node never
// silently drop, even if the tree isn't set up for CMS logging.
//
// Returns a nil slice when there are no non-zero deltas.
func BuildCorrectionTransactions(s Store, binID, nodeID int64, oldManifest, newManifest []bins.ManifestEntry, reason string) ([]*cms.Transaction, error) {
	boundary, err := FindCMSBoundary(s, nodeID)
	if err != nil {
		return nil, err
	}
	var boundaryID int64
	var boundaryName string
	if boundary != nil {
		boundaryID = boundary.ID
		boundaryName = boundary.Name
	} else {
		boundaryID = nodeID
		node, err := s.GetNode(nodeID)
		if err != nil {
			return nil, err
		}
		boundaryName = node.Name
	}

	oldQty := make(map[string]int64)
	for _, m := range oldManifest {
		oldQty[m.CatID] += m.Quantity
	}
	newQty := make(map[string]int64)
	for _, m := range newManifest {
		newQty[m.CatID] += m.Quantity
	}

	bin, err := s.GetBin(binID)
	if err != nil {
		return nil, err
	}

	allCatIDs := make(map[string]bool)
	for k := range oldQty {
		allCatIDs[k] = true
	}
	for k := range newQty {
		allCatIDs[k] = true
	}

	totals := s.SumCatIDsAtBoundary(boundaryID)

	var txns []*cms.Transaction
	for catID := range allCatIDs {
		delta := newQty[catID] - oldQty[catID]
		if delta == 0 {
			continue
		}
		qtyAfter := totals[catID]
		qtyBefore := qtyAfter - delta
		if qtyBefore < 0 {
			qtyBefore = 0
		}
		txns = append(txns, &cms.Transaction{
			NodeID:      boundaryID,
			NodeName:    boundaryName,
			TxnType:     txnType(delta),
			CatID:       catID,
			Delta:       delta,
			QtyBefore:   qtyBefore,
			QtyAfter:    qtyAfter,
			BinID:       &bin.ID,
			BinLabel:    bin.Label,
			PayloadCode: bin.PayloadCode,
			SourceType:  "correction",
			Notes:       reason,
		})
	}

	if len(txns) == 0 {
		return nil, nil
	}
	return txns, nil
}
