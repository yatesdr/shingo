package dispatch

import (
	"errors"
	"fmt"
	"time"

	"shingocore/store"
)

// ErrBuried indicates the target payload exists but is blocked by shallower payloads.
var ErrBuried = errors.New("payload is buried")

// BuriedError provides detail about a buried payload for reshuffle planning.
type BuriedError struct {
	Payload *store.Payload
	Slot    *store.Node
	LaneID  int64
}

func (e *BuriedError) Error() string {
	return fmt.Sprintf("payload %d is buried at slot %s in lane %d", e.Payload.ID, e.Slot.Name, e.LaneID)
}

func (e *BuriedError) Unwrap() error { return ErrBuried }

// Retrieval algorithm codes.
const (
	RetrieveFIFO = "FIFO" // oldest loaded/created timestamp, buried-payload reshuffle
	RetrieveFAVL = "FAVL" // first available unclaimed payload, no reshuffle
)

// Storage algorithm codes.
const (
	StoreLKND = "LKND" // Like Kind: consolidate matching blueprints, then emptiest
	StoreDPTH = "DPTH" // Depth First: pack back-to-front regardless of blueprint
)

// GroupResolver handles NGRP → LANE → Slot and NGRP → direct child resolution.
type GroupResolver struct {
	DB       *store.DB
	LaneLock *LaneLock
}

// getGroupAlgorithm reads a property from the node group, returning defaultVal if unset.
func (r *GroupResolver) getGroupAlgorithm(groupID int64, key, defaultVal string) string {
	v := r.DB.GetNodeProperty(groupID, key)
	if v == "" {
		return defaultVal
	}
	return v
}

// ResolveRetrieve finds the best accessible payload across all lanes and direct children.
func (r *GroupResolver) ResolveRetrieve(group *store.Node, blueprintID *int64) (*ResolveResult, error) {
	algo := r.getGroupAlgorithm(group.ID, "retrieve_algorithm", RetrieveFIFO)
	switch algo {
	case RetrieveFAVL:
		return r.resolveRetrieveFAVL(group, blueprintID)
	default:
		return r.resolveRetrieveFIFO(group, blueprintID)
	}
}

// resolveRetrieveFIFO picks the oldest accessible payload by timestamp, with buried-payload reshuffle.
func (r *GroupResolver) resolveRetrieveFIFO(group *store.Node, blueprintID *int64) (*ResolveResult, error) {
	children, err := r.DB.ListChildNodes(group.ID)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", group.Name, err)
	}

	var blueprintCode string
	if blueprintID != nil {
		bp, err := r.DB.GetBlueprint(*blueprintID)
		if err == nil {
			blueprintCode = bp.Code
		}
	}

	var bestPayload *store.Payload
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

			p, err := r.DB.FindSourcePayloadInLane(child.ID, blueprintCode)
			if err != nil {
				continue
			}

			pTime := p.CreatedAt
			if p.LoadedAt != nil {
				pTime = *p.LoadedAt
			}

			if bestPayload == nil || pTime.Before(bestTime) {
				bestPayload = p
				bestTime = pTime
				slot, _ := r.DB.GetNode(*p.NodeID)
				bestNode = slot
			}
		} else if !child.IsSynthetic {
			payloads, err := r.DB.ListPayloadsByNode(child.ID)
			if err != nil {
				continue
			}
			for _, p := range payloads {
				if p.ClaimedBy != nil || p.Status != "available" || p.BinStatus == "staged" {
					continue
				}
				if blueprintID != nil && p.BlueprintID != *blueprintID {
					continue
				}
				pTime := p.CreatedAt
				if p.LoadedAt != nil {
					pTime = *p.LoadedAt
				}
				if bestPayload == nil || pTime.Before(bestTime) {
					bestPayload = p
					bestTime = pTime
					bestNode = child
				}
			}
		}
	}

	if bestPayload != nil {
		return &ResolveResult{Node: bestNode, Payload: bestPayload}, nil
	}

	// No accessible payload found — check if any are buried in lanes
	for _, child := range children {
		if !child.Enabled || child.NodeTypeCode != "LANE" {
			continue
		}
		buried, slot, err := r.DB.FindBuriedPayload(child.ID, blueprintCode)
		if err == nil && buried != nil {
			return nil, &BuriedError{Payload: buried, Slot: slot, LaneID: child.ID}
		}
	}

	return nil, fmt.Errorf("no payload of requested blueprint in node group %s", group.Name)
}

// resolveRetrieveFAVL returns the first available unclaimed payload — no timestamp comparison, no reshuffle.
func (r *GroupResolver) resolveRetrieveFAVL(group *store.Node, blueprintID *int64) (*ResolveResult, error) {
	children, err := r.DB.ListChildNodes(group.ID)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", group.Name, err)
	}

	var blueprintCode string
	if blueprintID != nil {
		bp, err := r.DB.GetBlueprint(*blueprintID)
		if err == nil {
			blueprintCode = bp.Code
		}
	}

	for _, child := range children {
		if !child.Enabled {
			continue
		}

		if child.NodeTypeCode == "LANE" {
			if r.LaneLock != nil && r.LaneLock.IsLocked(child.ID) {
				continue
			}

			p, err := r.DB.FindSourcePayloadInLane(child.ID, blueprintCode)
			if err != nil {
				continue
			}
			slot, _ := r.DB.GetNode(*p.NodeID)
			return &ResolveResult{Node: slot, Payload: p}, nil
		} else if !child.IsSynthetic {
			payloads, err := r.DB.ListPayloadsByNode(child.ID)
			if err != nil {
				continue
			}
			for _, p := range payloads {
				if p.ClaimedBy != nil || p.Status != "available" || p.BinStatus == "staged" {
					continue
				}
				if blueprintID != nil && p.BlueprintID != *blueprintID {
					continue
				}
				return &ResolveResult{Node: child, Payload: p}, nil
			}
		}
	}

	return nil, fmt.Errorf("no payload of requested blueprint in node group %s", group.Name)
}

// ResolveStore finds the best slot for storing a payload in a node group.
func (r *GroupResolver) ResolveStore(group *store.Node, blueprintID *int64, binTypeID *int64) (*ResolveResult, error) {
	algo := r.getGroupAlgorithm(group.ID, "store_algorithm", StoreLKND)
	switch algo {
	case StoreDPTH:
		return r.resolveStoreDPTH(group, blueprintID, binTypeID)
	default:
		return r.resolveStoreLKND(group, blueprintID, binTypeID)
	}
}

// resolveStoreLKND consolidates matching blueprints first, then picks the emptiest slot.
func (r *GroupResolver) resolveStoreLKND(group *store.Node, blueprintID *int64, binTypeID *int64) (*ResolveResult, error) {
	children, err := r.DB.ListChildNodes(group.ID)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", group.Name, err)
	}

	type candidate struct {
		node     *store.Node
		hasMatch bool
		count    int
	}

	var candidates []candidate

	for _, child := range children {
		if !child.Enabled {
			continue
		}

		if child.NodeTypeCode == "LANE" {
			if r.LaneLock != nil && r.LaneLock.IsLocked(child.ID) {
				continue
			}

			// Skip lanes with blueprint restrictions that don't match
			if blueprintID != nil {
				laneBlueprints, _ := r.DB.GetEffectiveBlueprints(child.ID)
				if len(laneBlueprints) > 0 {
					match := false
					for _, lb := range laneBlueprints {
						if lb.ID == *blueprintID {
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

			slot, err := r.DB.FindStoreSlotInLane(child.ID, 0)
			if err != nil {
				continue // lane is full
			}

			count, _ := r.DB.CountBinsInLane(child.ID)
			slots, _ := r.DB.ListLaneSlots(child.ID)

			hasMatch := false
			if blueprintID != nil {
				for _, s := range slots {
					payloads, _ := r.DB.ListPayloadsByNode(s.ID)
					for _, p := range payloads {
						if p.BlueprintID == *blueprintID {
							hasMatch = true
							break
						}
					}
					if hasMatch {
						break
					}
				}
			}

			candidates = append(candidates, candidate{node: slot, hasMatch: hasMatch, count: count})
		} else if !child.IsSynthetic {
			count, err := r.DB.CountBinsByNode(child.ID)
			if err != nil {
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
			if blueprintID != nil {
				payloads, _ := r.DB.ListPayloadsByNode(child.ID)
				for _, p := range payloads {
					if p.BlueprintID == *blueprintID {
						hasMatch = true
						break
					}
				}
			}

			candidates = append(candidates, candidate{node: child, hasMatch: hasMatch, count: count})
		}
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no available slot in node group %s", group.Name)
	}

	// Prefer consolidation, then emptiest
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.hasMatch && !best.hasMatch {
			best = c
		} else if c.hasMatch == best.hasMatch && c.count < best.count {
			best = c
		}
	}

	return &ResolveResult{Node: best.node}, nil
}

// resolveStoreDPTH packs back-to-front regardless of blueprint. Prefers lanes over direct children.
func (r *GroupResolver) resolveStoreDPTH(group *store.Node, blueprintID *int64, binTypeID *int64) (*ResolveResult, error) {
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

		// Skip lanes with blueprint restrictions that don't match
		if blueprintID != nil {
			laneBlueprints, _ := r.DB.GetEffectiveBlueprints(child.ID)
			if len(laneBlueprints) > 0 {
				match := false
				for _, lb := range laneBlueprints {
					if lb.ID == *blueprintID {
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

		slot, err := r.DB.FindStoreSlotInLane(child.ID, 0)
		if err != nil {
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
