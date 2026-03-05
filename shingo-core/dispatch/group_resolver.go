package dispatch

import (
	"errors"
	"fmt"
	"time"

	"shingocore/store"
)

// ErrBuried indicates the target instance exists but is blocked by shallower instances.
var ErrBuried = errors.New("instance is buried")

// BuriedError provides detail about a buried instance for reshuffle planning.
type BuriedError struct {
	Instance *store.PayloadInstance
	Slot     *store.Node
	LaneID   int64
}

func (e *BuriedError) Error() string {
	return fmt.Sprintf("instance %d is buried at slot %s in lane %d", e.Instance.ID, e.Slot.Name, e.LaneID)
}

func (e *BuriedError) Unwrap() error { return ErrBuried }

// GroupResolver handles NGRP → LANE → Slot and NGRP → direct child resolution.
type GroupResolver struct {
	DB       *store.DB
	LaneLock *LaneLock
}

// ResolveRetrieve finds the best accessible FIFO instance across all lanes and direct children in a node group.
func (r *GroupResolver) ResolveRetrieve(group *store.Node, styleID *int64) (*ResolveResult, error) {
	children, err := r.DB.ListChildNodes(group.ID)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", group.Name, err)
	}

	var styleCode string
	if styleID != nil {
		ps, err := r.DB.GetPayloadStyle(*styleID)
		if err == nil {
			styleCode = ps.Name
		}
	}

	// Search for accessible instance across all lanes and direct children, pick oldest loaded_at
	var bestInstance *store.PayloadInstance
	var bestNode *store.Node
	var bestTime time.Time

	for _, child := range children {
		if !child.Enabled {
			continue
		}

		if child.NodeTypeCode == "LANE" {
			// Skip locked lanes
			if r.LaneLock != nil && r.LaneLock.IsLocked(child.ID) {
				continue
			}

			inst, err := r.DB.FindSourceInstanceInLane(child.ID, styleCode)
			if err != nil {
				continue
			}

			instTime := inst.CreatedAt
			if inst.LoadedAt != nil {
				instTime = *inst.LoadedAt
			}

			if bestInstance == nil || instTime.Before(bestTime) {
				bestInstance = inst
				bestTime = instTime
				slot, _ := r.DB.GetNode(*inst.NodeID)
				bestNode = slot
			}
		} else if !child.IsSynthetic {
			// Direct physical child — always accessible
			instances, err := r.DB.ListInstancesByNode(child.ID)
			if err != nil {
				continue
			}
			for _, inst := range instances {
				if inst.ClaimedBy != nil || inst.Status != "available" {
					continue
				}
				if styleID != nil && inst.StyleID != *styleID {
					continue
				}
				instTime := inst.CreatedAt
				if inst.LoadedAt != nil {
					instTime = *inst.LoadedAt
				}
				if bestInstance == nil || instTime.Before(bestTime) {
					bestInstance = inst
					bestTime = instTime
					bestNode = child
				}
			}
		}
	}

	if bestInstance != nil {
		return &ResolveResult{Node: bestNode, Instance: bestInstance}, nil
	}

	// No accessible instance found — check if any are buried in lanes
	for _, child := range children {
		if !child.Enabled || child.NodeTypeCode != "LANE" {
			continue
		}
		buried, slot, err := r.DB.FindBuriedInstance(child.ID, styleCode)
		if err == nil && buried != nil {
			return nil, &BuriedError{Instance: buried, Slot: slot, LaneID: child.ID}
		}
	}

	return nil, fmt.Errorf("no instance of requested style in node group %s", group.Name)
}

// ResolveStore finds the best slot for storing an instance in a node group.
// Considers both lane slots and direct children. Prefers consolidation, then emptiest.
func (r *GroupResolver) ResolveStore(group *store.Node, styleID *int64) (*ResolveResult, error) {
	children, err := r.DB.ListChildNodes(group.ID)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", group.Name, err)
	}

	type candidate struct {
		node     *store.Node
		hasMatch bool
		count    int
		capacity int
	}

	var candidates []candidate

	for _, child := range children {
		if !child.Enabled {
			continue
		}

		if child.NodeTypeCode == "LANE" {
			// Skip locked lanes
			if r.LaneLock != nil && r.LaneLock.IsLocked(child.ID) {
				continue
			}

			// Skip lanes with payload style restrictions that don't match
			if styleID != nil {
				laneStyles, _ := r.DB.GetEffectivePayloadStyles(child.ID)
				if len(laneStyles) > 0 {
					match := false
					for _, ls := range laneStyles {
						if ls.ID == *styleID {
							match = true
							break
						}
					}
					if !match {
						continue
					}
				}
			}

			slot, err := r.DB.FindStoreSlotInLane(child.ID, 0)
			if err != nil {
				continue // lane is full
			}

			count, _ := r.DB.CountInstancesInLane(child.ID)
			slots, _ := r.DB.ListLaneSlots(child.ID)
			capacity := len(slots)

			hasMatch := false
			if styleID != nil {
				for _, s := range slots {
					instances, _ := r.DB.ListInstancesByNode(s.ID)
					for _, inst := range instances {
						if inst.StyleID == *styleID {
							hasMatch = true
							break
						}
					}
					if hasMatch {
						break
					}
				}
			}

			candidates = append(candidates, candidate{node: slot, hasMatch: hasMatch, count: count, capacity: capacity})
		} else if !child.IsSynthetic && child.Capacity > 0 {
			// Direct physical child
			count, err := r.DB.CountInstancesByNode(child.ID)
			if err != nil {
				continue
			}
			if count >= child.Capacity {
				continue
			}

			hasMatch := false
			if styleID != nil {
				instances, _ := r.DB.ListInstancesByNode(child.ID)
				for _, inst := range instances {
					if inst.StyleID == *styleID {
						hasMatch = true
						break
					}
				}
			}

			candidates = append(candidates, candidate{node: child, hasMatch: hasMatch, count: count, capacity: child.Capacity})
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
