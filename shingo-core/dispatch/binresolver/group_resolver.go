package binresolver

import (
	"errors"
	"fmt"
	"time"

	"shingocore/store"
)

// ErrBuried indicates the target bin exists but is blocked by shallower bins.
var ErrBuried = errors.New("bin is buried")

// StructuralError indicates a permanent resolution failure — the group
// structure cannot satisfy the request regardless of inventory changes.
type StructuralError struct {
	Group   string
	Payload string
	Reason  string
}

func (e *StructuralError) Error() string {
	return fmt.Sprintf("structural: %s (group=%s, payload=%s)",
		e.Reason, e.Group, e.Payload)
}

// BuriedError provides detail about a buried bin for reshuffle planning.
type BuriedError struct {
	Bin    *store.Bin
	Slot   *store.Node
	LaneID int64
}

func (e *BuriedError) Error() string {
	return fmt.Sprintf("bin %d is buried at slot %s in lane %d", e.Bin.ID, e.Slot.Name, e.LaneID)
}

func (e *BuriedError) Unwrap() error { return ErrBuried }

// Retrieval algorithm codes.
const (
	RetrieveFIFO = "FIFO" // strict FIFO: globally oldest bin, proactive reshuffle when buried is older
	RetrieveCOST = "COST" // cost-optimized: oldest accessible bin, reshuffle only when none accessible
	RetrieveFAVL = "FAVL" // first available unclaimed bin, no reshuffle
)

// Storage algorithm codes.
const (
	StoreLKND = "LKND" // Like Kind: consolidate matching payload codes, then emptiest
	StoreDPTH = "DPTH" // Depth First: pack back-to-front regardless of payload
)

// GroupResolver handles NGRP → LANE → Slot and NGRP → direct child resolution.
//
// DB is the narrow Store interface (satisfied by *store.DB); see
// store.go. This lets per-algorithm tests drive the resolver with a
// fake and avoid database fixtures.
type GroupResolver struct {
	DB       Store
	LaneLock *LaneLock
	DebugLog func(string, ...any)
}

func (r *GroupResolver) dbg(format string, args ...any) {
	if fn := r.DebugLog; fn != nil {
		fn(format, args...)
	}
}

// getGroupAlgorithm reads a property from the node group, returning defaultVal if unset.
func (r *GroupResolver) getGroupAlgorithm(groupID int64, key, defaultVal string) string {
	v := r.DB.GetNodeProperty(groupID, key)
	if v == "" {
		return defaultVal
	}
	return v
}

// ResolveRetrieve finds the best accessible bin across all lanes and direct children.
func (r *GroupResolver) ResolveRetrieve(group *store.Node, payloadCode string) (*ResolveResult, error) {
	algo := r.getGroupAlgorithm(group.ID, "retrieve_algorithm", RetrieveFIFO)
	switch algo {
	case RetrieveFAVL:
		return r.resolveRetrieveFAVL(group, payloadCode)
	case RetrieveCOST:
		return r.resolveRetrieveCOST(group, payloadCode)
	default:
		return r.resolveRetrieveFIFO(group, payloadCode)
	}
}

// resolveRetrieveFIFO picks the oldest accessible bin by timestamp, with buried-bin reshuffle.
func (r *GroupResolver) resolveRetrieveFIFO(group *store.Node, payloadCode string) (*ResolveResult, error) {
	children, err := r.DB.ListChildNodes(group.ID)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", group.Name, err)
	}

	var bestBin *store.Bin
	var bestNode *store.Node
	var bestTime time.Time

	for _, child := range children {
		if !child.Enabled {
			continue
		}

		if child.NodeTypeCode == "LANE" {
			if r.LaneLock != nil && r.LaneLock.IsLocked(child.ID) {
				continue
			}

			b, err := r.DB.FindSourceBinInLane(child.ID, payloadCode)
			if err != nil {
				r.dbg("FIFO: FindSourceBinInLane lane=%s: %v", child.Name, err)
				continue
			}

			bTime := b.CreatedAt
			if b.LoadedAt != nil {
				bTime = *b.LoadedAt
			}

			if bestBin == nil || bTime.Before(bestTime) {
				bestBin = b
				bestTime = bTime
				slot, err := r.DB.GetNode(*b.NodeID)
				if err != nil {
					r.dbg("FIFO: GetNode for bin %d slot: %v", b.ID, err)
				}
				bestNode = slot
			}
		} else if !child.IsSynthetic {
			bins, err := r.DB.ListBinsByNode(child.ID)
			if err != nil {
				r.dbg("FIFO: ListBinsByNode node=%s: %v", child.Name, err)
				continue
			}
			for _, b := range bins {
				if !isBinAvailableForRetrieve(b, payloadCode) {
					continue
				}
				bTime := b.CreatedAt
				if b.LoadedAt != nil {
					bTime = *b.LoadedAt
				}
				if bestBin == nil || bTime.Before(bestTime) {
					bestBin = b
					bestTime = bTime
					bestNode = child
				}
			}
		}
	}

	// Phase 2: Scan for the oldest buried bin across all lanes
	var oldestBuried *store.Bin
	var oldestBuriedSlot *store.Node
	var oldestBuriedLaneID int64
	var oldestBuriedTime time.Time

	for _, child := range children {
		if !child.Enabled || child.NodeTypeCode != "LANE" {
			continue
		}
		if r.LaneLock != nil && r.LaneLock.IsLocked(child.ID) {
			continue
		}
		buried, slot, err := r.DB.FindOldestBuriedBin(child.ID, payloadCode)
		if err != nil || buried == nil {
			continue
		}
		bTime := buried.CreatedAt
		if buried.LoadedAt != nil {
			bTime = *buried.LoadedAt
		}
		if oldestBuried == nil || bTime.Before(oldestBuriedTime) {
			oldestBuried = buried
			oldestBuriedSlot = slot
			oldestBuriedLaneID = child.ID
			oldestBuriedTime = bTime
		}
	}

	// Phase 3: If a buried bin is older than the best accessible, trigger reshuffle
	if oldestBuried != nil && (bestBin == nil || oldestBuriedTime.Before(bestTime)) {
		r.dbg("FIFO: buried bin %d (%s) is older than best accessible bin, triggering reshuffle in lane %d",
			oldestBuried.ID, oldestBuriedTime.Format(time.RFC3339), oldestBuriedLaneID)
		return nil, &BuriedError{Bin: oldestBuried, Slot: oldestBuriedSlot, LaneID: oldestBuriedLaneID}
	}

	if bestBin != nil {
		return &ResolveResult{Node: bestNode, Bin: bestBin}, nil
	}

	return nil, r.classifyEmptyGroup(group, children, payloadCode)
}

// resolveRetrieveCOST picks the oldest accessible bin by timestamp, with buried-bin reshuffle
// only when no accessible bins exist. This is the cost-optimized retrieval strategy.
func (r *GroupResolver) resolveRetrieveCOST(group *store.Node, payloadCode string) (*ResolveResult, error) {
	children, err := r.DB.ListChildNodes(group.ID)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", group.Name, err)
	}

	var bestBin *store.Bin
	var bestNode *store.Node
	var bestTime time.Time

	for _, child := range children {
		if !child.Enabled {
			continue
		}

		if child.NodeTypeCode == "LANE" {
			if r.LaneLock != nil && r.LaneLock.IsLocked(child.ID) {
				continue
			}

			b, err := r.DB.FindSourceBinInLane(child.ID, payloadCode)
			if err != nil {
				r.dbg("COST: FindSourceBinInLane lane=%s: %v", child.Name, err)
				continue
			}

			bTime := b.CreatedAt
			if b.LoadedAt != nil {
				bTime = *b.LoadedAt
			}

			if bestBin == nil || bTime.Before(bestTime) {
				bestBin = b
				bestTime = bTime
				slot, err := r.DB.GetNode(*b.NodeID)
				if err != nil {
					r.dbg("COST: GetNode for bin %d slot: %v", b.ID, err)
				}
				bestNode = slot
			}
		} else if !child.IsSynthetic {
			bins, err := r.DB.ListBinsByNode(child.ID)
			if err != nil {
				r.dbg("COST: ListBinsByNode node=%s: %v", child.Name, err)
				continue
			}
			for _, b := range bins {
				if !isBinAvailableForRetrieve(b, payloadCode) {
					continue
				}
				bTime := b.CreatedAt
				if b.LoadedAt != nil {
					bTime = *b.LoadedAt
				}
				if bestBin == nil || bTime.Before(bestTime) {
					bestBin = b
					bestTime = bTime
					bestNode = child
				}
			}
		}
	}

	if bestBin != nil {
		return &ResolveResult{Node: bestNode, Bin: bestBin}, nil
	}

	// No accessible bin found — check if any are buried in lanes
	for _, child := range children {
		if !child.Enabled || child.NodeTypeCode != "LANE" {
			continue
		}
		buried, slot, err := r.DB.FindBuriedBin(child.ID, payloadCode)
		if err == nil && buried != nil {
			return nil, &BuriedError{Bin: buried, Slot: slot, LaneID: child.ID}
		}
	}

	return nil, r.classifyEmptyGroup(group, children, payloadCode)
}

// resolveRetrieveFAVL returns the first available unclaimed bin — no timestamp comparison, no reshuffle.
func (r *GroupResolver) resolveRetrieveFAVL(group *store.Node, payloadCode string) (*ResolveResult, error) {
	children, err := r.DB.ListChildNodes(group.ID)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", group.Name, err)
	}

	for _, child := range children {
		if !child.Enabled {
			continue
		}

		if child.NodeTypeCode == "LANE" {
			if r.LaneLock != nil && r.LaneLock.IsLocked(child.ID) {
				continue
			}

			b, err := r.DB.FindSourceBinInLane(child.ID, payloadCode)
			if err != nil {
				r.dbg("FAVL: FindSourceBinInLane lane=%s: %v", child.Name, err)
				continue
			}
			slot, err := r.DB.GetNode(*b.NodeID)
			if err != nil {
				r.dbg("FAVL: GetNode for bin %d slot: %v", b.ID, err)
			}
			return &ResolveResult{Node: slot, Bin: b}, nil
		} else if !child.IsSynthetic {
			bins, err := r.DB.ListBinsByNode(child.ID)
			if err != nil {
				r.dbg("FAVL: ListBinsByNode node=%s: %v", child.Name, err)
				continue
			}
			for _, b := range bins {
				if !isBinAvailableForRetrieve(b, payloadCode) {
					continue
				}
				return &ResolveResult{Node: child, Bin: b}, nil
			}
		}
	}

	return nil, r.classifyEmptyGroup(group, children, payloadCode)
}

// classifyEmptyGroup determines whether a group resolution failure is
// structural (permanent) or transient (inventory may arrive).
//
// Intentionally looser than the resolution loop. The loop skips lanes for
// multiple reasons (locked, full, buried, payload mismatch). This helper
// only checks structural capability — not whether bins are available now.
// A false "transient" is safer than a false "structural".
//
// On any DB error during classification, returns transient.
func (r *GroupResolver) classifyEmptyGroup(
	group *store.Node, children []*store.Node, payloadCode string,
) error {
	hasEnabled := false
	for _, child := range children {
		if child.Enabled {
			hasEnabled = true
			break
		}
	}
	if !hasEnabled {
		return &StructuralError{
			Group: group.Name, Payload: payloadCode,
			Reason: "group has no enabled child nodes",
		}
	}

	if payloadCode != "" {
		hasCapable := false
		for _, child := range children {
			if !child.Enabled {
				continue
			}
			payloads, err := r.DB.GetEffectivePayloads(child.ID)
			if err != nil {
				r.dbg("classifyEmptyGroup: GetEffectivePayloads(%d) error: %v, "+
					"defaulting to transient", child.ID, err)
				return fmt.Errorf("no bin of requested payload in node group %s",
					group.Name)
			}
			if len(payloads) == 0 {
				hasCapable = true
				break
			}
			for _, p := range payloads {
				if p.Code == payloadCode {
					hasCapable = true
					break
				}
			}
			if hasCapable {
				break
			}
		}
		if !hasCapable {
			return &StructuralError{
				Group: group.Name, Payload: payloadCode,
				Reason: "no child node accepts this payload type",
			}
		}
	}

	return fmt.Errorf("no bin of requested payload in node group %s", group.Name)
}

// ResolveStore finds the best slot for storing a bin in a node group.
func (r *GroupResolver) ResolveStore(group *store.Node, payloadCode string, binTypeID *int64) (*ResolveResult, error) {
	algo := r.getGroupAlgorithm(group.ID, "store_algorithm", StoreLKND)
	switch algo {
	case StoreDPTH:
		return r.resolveStoreDPTH(group, payloadCode, binTypeID)
	default:
		return r.resolveStoreLKND(group, payloadCode, binTypeID)
	}
}

// resolveStoreLKND consolidates matching payload codes first, then picks the emptiest slot.
func (r *GroupResolver) resolveStoreLKND(group *store.Node, payloadCode string, binTypeID *int64) (*ResolveResult, error) {
	children, err := r.DB.ListChildNodes(group.ID)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", group.Name, err)
	}

	var candidates []storageCandidate

	for _, child := range children {
		if !child.Enabled {
			continue
		}

		if child.NodeTypeCode == "LANE" {
			if r.LaneLock != nil && r.LaneLock.IsLocked(child.ID) {
				continue
			}

			// Skip lanes with payload restrictions that don't match
			if payloadCode != "" {
				lanePayloads, _ := r.DB.GetEffectivePayloads(child.ID)
				if len(lanePayloads) > 0 {
					match := false
					for _, lp := range lanePayloads {
						if lp.Code == payloadCode {
							match = true
							break
						}
					}
					if !match {
						continue
					}
				}
			}

			// Skip lanes with bin type restrictions that don't match
			if binTypeID != nil {
				if !r.binTypeAllowed(child.ID, *binTypeID) {
					continue
				}
			}

			slot, err := r.DB.FindStoreSlotInLane(child.ID)
			if err != nil {
				r.dbg("LKND: FindStoreSlotInLane lane=%s: %v", child.Name, err)
				continue // lane is full
			}

			count, _ := r.DB.CountBinsInLane(child.ID)
			slots, _ := r.DB.ListLaneSlots(child.ID)

			hasMatch := false
			if payloadCode != "" {
				for _, s := range slots {
					bins, _ := r.DB.ListBinsByNode(s.ID)
					for _, b := range bins {
						if b.PayloadCode == payloadCode {
							hasMatch = true
							break
						}
					}
					if hasMatch {
						break
					}
				}
			}

			candidates = append(candidates, storageCandidate{node: slot, hasMatch: hasMatch, count: count})
		} else if !child.IsSynthetic {
			count, err := r.DB.CountBinsByNode(child.ID)
			if err != nil {
				r.dbg("LKND: CountBinsByNode node=%s: %v", child.Name, err)
				continue
			}
			inflight, _ := r.DB.CountActiveOrdersByDeliveryNode(child.Name)
			if count+inflight >= 1 {
				continue
			}

			// Skip nodes with bin type restrictions that don't match
			if binTypeID != nil {
				if !r.binTypeAllowed(child.ID, *binTypeID) {
					continue
				}
			}

			hasMatch := false
			if payloadCode != "" {
				bins, _ := r.DB.ListBinsByNode(child.ID)
				for _, b := range bins {
					if b.PayloadCode == payloadCode {
						hasMatch = true
						break
					}
				}
			}

			candidates = append(candidates, storageCandidate{node: child, hasMatch: hasMatch, count: count})
		}
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no available slot in node group %s", group.Name)
	}

	return &ResolveResult{Node: bestStorageCandidate(candidates)}, nil
}

// resolveStoreDPTH packs back-to-front regardless of payload. Prefers lanes over direct children.
func (r *GroupResolver) resolveStoreDPTH(group *store.Node, payloadCode string, binTypeID *int64) (*ResolveResult, error) {
	children, err := r.DB.ListChildNodes(group.ID)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", group.Name, err)
	}

	// First pass: try lanes (deepest empty slot)
	for _, child := range children {
		if !child.Enabled || child.NodeTypeCode != "LANE" {
			continue
		}
		if r.LaneLock != nil && r.LaneLock.IsLocked(child.ID) {
			continue
		}

		// Skip lanes with payload restrictions that don't match
		if payloadCode != "" {
			lanePayloads, _ := r.DB.GetEffectivePayloads(child.ID)
			if len(lanePayloads) > 0 {
				match := false
				for _, lp := range lanePayloads {
					if lp.Code == payloadCode {
						match = true
						break
					}
				}
				if !match {
					continue
				}
			}
		}

		// Skip lanes with bin type restrictions that don't match
		if binTypeID != nil {
			if !r.binTypeAllowed(child.ID, *binTypeID) {
				continue
			}
		}

		slot, err := r.DB.FindStoreSlotInLane(child.ID)
		if err != nil {
			r.dbg("DPTH: FindStoreSlotInLane lane=%s: %v", child.Name, err)
			continue // lane is full
		}
		return &ResolveResult{Node: slot}, nil
	}

	// Second pass: direct children
	for _, child := range children {
		if !child.Enabled || child.IsSynthetic {
			continue
		}

		// Skip nodes with bin type restrictions that don't match
		if binTypeID != nil {
			if !r.binTypeAllowed(child.ID, *binTypeID) {
				continue
			}
		}

		count, err := r.DB.CountBinsByNode(child.ID)
		if err != nil {
			r.dbg("DPTH: CountBinsByNode node=%s: %v", child.Name, err)
			continue
		}
		inflight, _ := r.DB.CountActiveOrdersByDeliveryNode(child.Name)
		if count+inflight < 1 {
			return &ResolveResult{Node: child}, nil
		}
	}

	return nil, fmt.Errorf("no available slot in node group %s", group.Name)
}

// binTypeAllowed checks whether a bin type is permitted at a node via effective bin types.
// Returns true if no restrictions are set (nil = all allowed) or if the bin type is in the set.
func (r *GroupResolver) binTypeAllowed(nodeID int64, binTypeID int64) bool {
	bts, err := r.DB.GetEffectiveBinTypes(nodeID)
	if err != nil || len(bts) == 0 {
		return true // no restrictions
	}
	for _, bt := range bts {
		if bt.ID == binTypeID {
			return true
		}
	}
	return false
}
