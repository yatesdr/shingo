package dispatch

import (
	"fmt"

	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// ReshuffleStep describes a single move in a reshuffle plan.
type ReshuffleStep struct {
	Sequence int
	StepType string // "unbury", "retrieve", "restock"
	BinID    int64
	FromNode *nodes.Node
	ToNode   *nodes.Node
}

// ReshufflePlan describes the full reshuffle needed to access a buried bin.
type ReshufflePlan struct {
	TargetBin    *bins.Bin
	TargetSlot   *nodes.Node
	Lane         *nodes.Node
	ShuffleSlots []*nodes.Node
	Steps        []ReshuffleStep
}

// PlanReshuffle creates a plan to unbury a target bin in a lane.
// Steps: move blockers front-to-back to shuffle slots, retrieve target, restock blockers deepest-first.
func PlanReshuffle(db *store.DB, target *bins.Bin, targetSlot *nodes.Node, lane *nodes.Node, groupID int64) (*ReshufflePlan, error) {
	if targetSlot.ParentID == nil {
		return nil, fmt.Errorf("target slot has no parent lane")
	}

	targetDepth, err := db.GetSlotDepth(targetSlot.ID)
	if err != nil {
		return nil, fmt.Errorf("get target depth: %w", err)
	}

	// Find all occupied slots shallower than target (blockers)
	slots, err := db.ListLaneSlots(lane.ID)
	if err != nil {
		return nil, fmt.Errorf("list lane slots: %w", err)
	}

	type blocker struct {
		bin   *bins.Bin
		slot  *nodes.Node
		depth int
	}

	var blockers []blocker
	for _, slot := range slots {
		depth, err := db.GetSlotDepth(slot.ID)
		if err != nil || depth >= targetDepth {
			continue
		}
		bins, err := db.ListBinsByNode(slot.ID)
		if err != nil || len(bins) == 0 {
			continue
		}
		blockers = append(blockers, blocker{bin: bins[0], slot: slot, depth: depth})
	}

	// Find shuffle slots
	shuffleSlots, err := findShuffleSlots(db, groupID, len(blockers))
	if err != nil {
		return nil, fmt.Errorf("find shuffle slots: %w", err)
	}

	plan := &ReshufflePlan{
		TargetBin:    target,
		TargetSlot:   targetSlot,
		Lane:         lane,
		ShuffleSlots: shuffleSlots,
	}

	seq := 1

	// Step 1: Move blockers to shuffle slots (front-to-back order = shallowest first)
	for i, b := range blockers {
		plan.Steps = append(plan.Steps, ReshuffleStep{
			Sequence: seq,
			StepType: "unbury",
			BinID:    b.bin.ID,
			FromNode: b.slot,
			ToNode:   shuffleSlots[i],
		})
		seq++
	}

	// Step 2: Retrieve the target (this is the actual order delivery)
	plan.Steps = append(plan.Steps, ReshuffleStep{
		Sequence: seq,
		StepType: "retrieve",
		BinID:    target.ID,
		FromNode: targetSlot,
	})
	seq++

	// Step 3: Restock blockers back to lane (deepest-first = reverse order)
	for i := len(blockers) - 1; i >= 0; i-- {
		plan.Steps = append(plan.Steps, ReshuffleStep{
			Sequence: seq,
			StepType: "restock",
			BinID:    blockers[i].bin.ID,
			FromNode: shuffleSlots[i],
			ToNode:   blockers[i].slot,
		})
		seq++
	}

	return plan, nil
}

// findShuffleSlots locates empty accessible slots for temporary shuffle storage.
// Pass 1: direct physical children of the group (always accessible).
// Pass 2: accessible empty slots in regular lanes.
func findShuffleSlots(db *store.DB, groupID int64, count int) ([]*nodes.Node, error) {
	children, err := db.ListChildNodes(groupID)
	if err != nil {
		return nil, err
	}

	var available []*nodes.Node

	// Pass 1: direct physical children of the group (always accessible)
	for _, c := range children {
		if !c.Enabled || c.IsSynthetic {
			continue
		}
		cnt, _ := db.CountBinsByNode(c.ID)
		if cnt == 0 {
			available = append(available, c)
			if len(available) >= count {
				return available, nil
			}
		}
	}

	// Pass 2: any empty accessible slot across all lanes
	for _, c := range children {
		if !c.Enabled || c.NodeTypeCode != "LANE" {
			continue
		}
		slots, _ := db.ListLaneSlots(c.ID)
		for _, slot := range slots {
			if !slot.Enabled {
				continue
			}
			acc, _ := db.IsSlotAccessible(slot.ID)
			if !acc {
				continue
			}
			cnt, _ := db.CountBinsByNode(slot.ID)
			if cnt == 0 {
				available = append(available, slot)
				if len(available) >= count {
					return available, nil
				}
			}
		}
	}

	if len(available) < count {
		return nil, fmt.Errorf("need %d shuffle slots but only %d available", count, len(available))
	}
	return available, nil
}
