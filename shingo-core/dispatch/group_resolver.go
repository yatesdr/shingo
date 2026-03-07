package dispatch

import (
	"errors"
	"fmt"
	"time"

	"shingocore/store"
)

// ErrBuried indicates the target bin exists but is blocked by shallower bins.
var ErrBuried = errors.New("bin is buried")

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
	RetrieveFIFO = "FIFO" // oldest loaded/created timestamp, buried-bin reshuffle
	RetrieveFAVL = "FAVL" // first available unclaimed bin, no reshuffle
)

// Storage algorithm codes.
const (
	StoreLKND = "LKND" // Like Kind: consolidate matching payload codes, then emptiest
	StoreDPTH = "DPTH" // Depth First: pack back-to-front regardless of payload
)

// GroupResolver handles NGRP → LANE → Slot and NGRP → direct child resolution.
type GroupResolver struct {
	DB       *store.DB
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

	return nil, fmt.Errorf("no bin of requested payload in node group %s", group.Name)
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

	return nil, fmt.Errorf("no bin of requested payload in node group %s", group.Name)
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
