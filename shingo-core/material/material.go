package material

import (
	"errors"

	"shingocore/store/cms"
	"shingocore/store/nodes"
)

// errCMSBoundaryCycle is returned by FindCMSBoundary when the parent
// chain revisits a node. It reaches the caller as an error and not as
// a nil node: a malformed tree is a failure to locate the boundary,
// which is a different answer from "this node has no boundary above
// it".
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
	// RobotID and OrderID name who moved the bin. They arrive on the event
	// rather than being read from the bin, because the bin's claim is already
	// released by the time the event fires — reading bin.ClaimedBy here is what
	// made cms_transactions.order_id NULL for every ordinary delivery.
	RobotID string
	OrderID int64
}

// CMSStoreroomProperty is the node property that makes a node a CMS boundary
// and carries the storeroom code CMS knows it by ("SM01", "MAN", "DOCK").
//
// ONE PROPERTY, TWO ANSWERS, NO DEFAULT. Presence means "this is a boundary"
// and its value is where; absence means "not a boundary". A node is never a
// boundary because of where it sits in the tree.
const CMSStoreroomProperty = "cms_storeroom"

// FindCMSBoundary walks up the parent chain from nodeID and returns the nearest
// synthetic ancestor (or self) tagged with cms_storeroom, together with that
// storeroom's code.
//
// FAIL-CLOSED AT EVERY DEPTH. The predicate this replaces defaulted parentless
// synthetic nodes ON — enabled unless the property said "false" — and child
// synthetic nodes OFF. Nothing wrote that property in production, so the
// default was the whole behaviour, and it made every parentless synthetic node
// a CMS boundary: _TRANSIT, every node group, and every per-robot carrier node
// (_ROBOT:*). A bin picked up by a robot crossed from its real storeroom into
// "the robot", and shingo booked the transfer.
//
// Returns (nil, "", nil) when the walk reaches a root without finding a tagged
// ancestor. Returns (nil, "", err) when a Store call fails or the walk detects
// a cycle — "the lookup failed" is not "there is no boundary here", and a
// caller that cannot tell them apart emits zero transactions for a real move.
func FindCMSBoundary(s Store, nodeID int64) (*nodes.Node, string, error) {
	visited := make(map[int64]bool)
	currentID := nodeID
	for {
		if visited[currentID] {
			return nil, "", errCMSBoundaryCycle
		}
		visited[currentID] = true

		node, err := s.GetNode(currentID)
		if err != nil {
			return nil, "", err
		}

		if node.IsSynthetic {
			code, err := s.GetNodePropertyOrError(node.ID, CMSStoreroomProperty)
			if err != nil {
				return nil, "", err
			}
			if code != "" {
				return node, code, nil
			}
		}

		if node.ParentID == nil {
			return nil, "", nil
		}
		currentID = *node.ParentID
	}
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
	// The storeroom code is stamped onto the row here, where the walk already
	// found it. Carrying it forward rather than re-deriving it downstream is
	// what lets the wire layer be a pure struct-to-struct map: a translator
	// that had to look up "which storeroom is node 41" would need the node tree
	// and a database, and would stop being testable as a table of inputs.
	var srcBoundary, dstBoundary *nodes.Node
	var srcStoreroom, dstStoreroom string
	if ev.FromNodeID != 0 {
		b, code, err := FindCMSBoundary(s, ev.FromNodeID)
		if err != nil {
			return nil, err
		}
		srcBoundary, srcStoreroom = b, code
	}
	if ev.ToNodeID != 0 {
		b, code, err := FindCMSBoundary(s, ev.ToNodeID)
		if err != nil {
			return nil, err
		}
		dstBoundary, dstStoreroom = b, code
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

	// An unparseable manifest is a failure to answer the question, not an
	// answer of "no parts". Discarding the error here reported every
	// corrupt manifest as an empty bin and emitted zero CMS rows for a
	// real physical move.
	parsed, err := bin.ParseManifest()
	if err != nil {
		return nil, err
	}
	if parsed == nil || len(parsed.Items) == 0 {
		return nil, nil
	}

	// THE COUNT IS DERIVED, NOT READ. The manifest lists which parts are in
	// the carrier; how many of each is uop_remaining x the template's
	// parts_per_cycle, computed here at emission. The bin manifest used to
	// carry a stored qty and it was a full-bin nominal that nothing rewrote
	// as production drew the bin down — so a bin at 5 of 24 shipped 24 to
	// the storeroom ledger.
	perCycle, err := partsPerCycle(s, bin.PayloadCode)
	if err != nil {
		return nil, err
	}
	if perCycle == nil {
		// No template to count by. Skipping is deliberate: a movement row
		// with a guessed quantity is worse than no row, because it is
		// indistinguishable from a measured one once it reaches CMS.
		return nil, nil
	}

	// The order comes from the EVENT, not from bin.ClaimedBy. This used to read
	// the claim, and the claim is released by ApplyArrival before the event
	// fires — so order_id was NULL on every ordinary AMR delivery, a column
	// written by one path and true only on the paths nobody looked at.
	var orderID *int64
	if ev.OrderID != 0 {
		id := ev.OrderID
		orderID = &id
	}

	var txns []*cms.Transaction

	// Source boundary: bin leaving → negative delta.
	if srcBoundary != nil {
		for _, m := range parsed.Items {
			count := int64(bin.UOPRemaining) * perCycle[m.CatID]
			if count <= 0 {
				continue
			}
			txns = append(txns, &cms.Transaction{
				NodeID:      srcBoundary.ID,
				NodeName:    srcBoundary.Name,
				Storeroom:   srcStoreroom,
				CatID:       m.CatID,
				Delta:       -count,
				BinID:       &bin.ID,
				BinLabel:    bin.Label,
				PayloadCode: bin.PayloadCode,
				SourceType:  "movement",
				OrderID:     orderID,
				RobotID:     ev.RobotID,
			})
		}
	}

	// Dest boundary: bin arriving → positive delta.
	if dstBoundary != nil {
		for _, m := range parsed.Items {
			count := int64(bin.UOPRemaining) * perCycle[m.CatID]
			if count <= 0 {
				continue
			}
			txns = append(txns, &cms.Transaction{
				NodeID:      dstBoundary.ID,
				NodeName:    dstBoundary.Name,
				Storeroom:   dstStoreroom,
				CatID:       m.CatID,
				Delta:       count,
				BinID:       &bin.ID,
				BinLabel:    bin.Label,
				PayloadCode: bin.PayloadCode,
				SourceType:  "movement",
				OrderID:     orderID,
				RobotID:     ev.RobotID,
			})
		}
	}

	if len(txns) == 0 {
		return nil, nil
	}
	return txns, nil
}

// partsPerCycle returns the payload template's per-cycle ratio keyed by part
// number, or nil when the payload has no template.
//
// nil and an empty map are DIFFERENT ANSWERS and the caller acts on the
// difference: nil means "there is no template, so no count can be derived",
// while an empty map means "the template exists and lists no parts". Returning
// an empty map for both would turn a missing template into a bin that
// legitimately holds nothing.
//
// A bin with no payload_code has no template by construction; that is a bare
// carrier, and it returns nil rather than an error.
func partsPerCycle(s Store, payloadCode string) (map[string]int64, error) {
	if payloadCode == "" {
		return nil, nil
	}
	p, err := s.GetPayloadByCode(payloadCode)
	if err != nil {
		// Not found is not an error worth failing a movement over — Edge can
		// drive a direct load with a code Core has no template for. Errors
		// that are not "no such payload" reach the caller through the same
		// return, which is the conservative reading: an unreadable template
		// must not silently become a zero-quantity move.
		return nil, err
	}
	if p == nil {
		return nil, nil
	}
	items, err := s.ListPayloadManifest(p.ID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(items))
	for _, it := range items {
		out[it.PartNumber] = it.PartsPerCycle
	}
	return out, nil
}
