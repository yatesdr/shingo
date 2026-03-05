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

// SupermarketResolver handles two-level SUP → LAN → Slot resolution.
type SupermarketResolver struct {
	DB       *store.DB
	LaneLock *LaneLock
}

// ResolveRetrieve finds the best accessible FIFO instance across all lanes in a supermarket.
func (r *SupermarketResolver) ResolveRetrieve(supermarket *store.Node, styleID *int64) (*ResolveResult, error) {
	lanes, err := r.DB.ListChildNodes(supermarket.ID)
	if err != nil {
		return nil, fmt.Errorf("list lanes of %s: %w", supermarket.Name, err)
	}

	var styleCode string
	if styleID != nil {
		ps, err := r.DB.GetPayloadStyle(*styleID)
		if err == nil {
			styleCode = ps.Name
		}
	}

	// Search for accessible instance across all lanes, pick oldest loaded_at
	var bestInstance *store.PayloadInstance
	var bestNode *store.Node
	var bestTime time.Time

	for _, lane := range lanes {
		if !lane.Enabled {
			continue
		}
		// Only process LAN-type children
		if lane.NodeTypeCode != "LAN" {
			continue
		}
		// Skip locked lanes
		if r.LaneLock != nil && r.LaneLock.IsLocked(lane.ID) {
			continue
		}

		inst, err := r.DB.FindSourceInstanceInLane(lane.ID, styleCode)
		if err != nil {
			continue
		}

		// Pick the oldest by loaded_at (FIFO)
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
	}

	if bestInstance != nil {
		return &ResolveResult{Node: bestNode, Instance: bestInstance}, nil
	}

	// No accessible instance found — check if any are buried
	for _, lane := range lanes {
		if !lane.Enabled || lane.NodeTypeCode != "LAN" {
			continue
		}
		buried, slot, err := r.DB.FindBuriedInstance(lane.ID, styleCode)
		if err == nil && buried != nil {
			return nil, &BuriedError{Instance: buried, Slot: slot, LaneID: lane.ID}
		}
	}

	return nil, fmt.Errorf("no instance of requested style in supermarket %s", supermarket.Name)
}

// ResolveStore finds the best slot for storing an instance in a supermarket.
// Prefers consolidation (lanes already containing matching style), then deepest available slot.
func (r *SupermarketResolver) ResolveStore(supermarket *store.Node, styleID *int64) (*ResolveResult, error) {
	lanes, err := r.DB.ListChildNodes(supermarket.ID)
	if err != nil {
		return nil, fmt.Errorf("list lanes of %s: %w", supermarket.Name, err)
	}

	type candidate struct {
		node     *store.Node
		hasMatch bool
		count    int
		capacity int
	}

	var candidates []candidate
	for _, lane := range lanes {
		if !lane.Enabled || lane.NodeTypeCode != "LAN" {
			continue
		}
		// Skip locked lanes
		if r.LaneLock != nil && r.LaneLock.IsLocked(lane.ID) {
			continue
		}

		slot, err := r.DB.FindStoreSlotInLane(lane.ID, 0)
		if err != nil {
			continue // lane is full
		}

		count, _ := r.DB.CountInstancesInLane(lane.ID)
		slots, _ := r.DB.ListLaneSlots(lane.ID)
		capacity := len(slots)

		hasMatch := false
		if styleID != nil {
			// Check if lane already has instances of this style
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
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no available slot in supermarket %s", supermarket.Name)
	}

	// Prefer consolidation, then emptiest lane
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
