package dispatch

import (
	"fmt"

	"shingocore/store"
)

// ReshuffleStep describes a single move in a reshuffle plan.
type ReshuffleStep struct {
	Sequence   int
	StepType   string // "unbury", "retrieve", "restock"
	InstanceID int64
	FromNode   *store.Node
	ToNode     *store.Node
}

// ReshufflePlan describes the full reshuffle needed to access a buried instance.
type ReshufflePlan struct {
	TargetInstance *store.PayloadInstance
	TargetSlot     *store.Node
	Lane           *store.Node
	ShuffleSlots   []*store.Node
	Steps          []ReshuffleStep
}

// PlanReshuffle creates a plan to unbury a target instance in a lane.
// Steps: move blockers front-to-back to shuffle slots, retrieve target, restock blockers deepest-first.
func PlanReshuffle(db *store.DB, target *store.PayloadInstance, targetSlot *store.Node, lane *store.Node, supermarketID int64) (*ReshufflePlan, error) {
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
		instance *store.PayloadInstance
		slot     *store.Node
		depth    int
	}

	var blockers []blocker
	for _, slot := range slots {
		depth, err := db.GetSlotDepth(slot.ID)
		if err != nil || depth >= targetDepth {
			continue
		}
		instances, err := db.ListInstancesByNode(slot.ID)
		if err != nil || len(instances) == 0 {
			continue
		}
		blockers = append(blockers, blocker{instance: instances[0], slot: slot, depth: depth})
	}

	// Find shuffle slots
	shuffleSlots, err := FindShuffleSlots(db, supermarketID, len(blockers))
	if err != nil {
		return nil, fmt.Errorf("find shuffle slots: %w", err)
	}

	plan := &ReshufflePlan{
		TargetInstance: target,
		TargetSlot:     targetSlot,
		Lane:           lane,
		ShuffleSlots:   shuffleSlots,
	}

	seq := 1

	// Step 1: Move blockers to shuffle slots (front-to-back order = shallowest first)
	for i, b := range blockers {
		plan.Steps = append(plan.Steps, ReshuffleStep{
			Sequence:   seq,
			StepType:   "unbury",
			InstanceID: b.instance.ID,
			FromNode:   b.slot,
			ToNode:     shuffleSlots[i],
		})
		seq++
	}

	// Step 2: Retrieve the target (this is the actual order delivery)
	plan.Steps = append(plan.Steps, ReshuffleStep{
		Sequence:   seq,
		StepType:   "retrieve",
		InstanceID: target.ID,
		FromNode:   targetSlot,
	})
	seq++

	// Step 3: Restock blockers back to lane (deepest-first = reverse order)
	for i := len(blockers) - 1; i >= 0; i-- {
		plan.Steps = append(plan.Steps, ReshuffleStep{
			Sequence:   seq,
			StepType:   "restock",
			InstanceID: blockers[i].instance.ID,
			FromNode:   shuffleSlots[i],
			ToNode:     blockers[i].slot,
		})
		seq++
	}

	return plan, nil
}

// FindShuffleSlots locates empty shuffle slots in the supermarket's SHF child.
func FindShuffleSlots(db *store.DB, supermarketID int64, count int) ([]*store.Node, error) {
	children, err := db.ListChildNodes(supermarketID)
	if err != nil {
		return nil, err
	}

	// Find the SHF child
	var shfNode *store.Node
	for _, c := range children {
		if c.NodeTypeCode == "SHF" {
			shfNode = c
			break
		}
	}
	if shfNode == nil {
		return nil, fmt.Errorf("supermarket %d has no shuffle row (SHF)", supermarketID)
	}

	shuffleChildren, err := db.ListChildNodes(shfNode.ID)
	if err != nil {
		return nil, err
	}

	var available []*store.Node
	for _, slot := range shuffleChildren {
		if !slot.Enabled {
			continue
		}
		cnt, _ := db.CountInstancesByNode(slot.ID)
		if cnt == 0 {
			available = append(available, slot)
			if len(available) >= count {
				break
			}
		}
	}

	if len(available) < count {
		return nil, fmt.Errorf("need %d shuffle slots but only %d available", count, len(available))
	}
	return available, nil
}
