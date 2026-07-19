package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// Property keys for the per-group reshuffle controls added by the
// complex-order buried-bin reshuffle scope (v6 §3.5). Stored on the
// NGRP via the existing node-property table; read via
// store.GetNodeProperty.
const (
	// PropReshuffleTargetNodes is a JSON array of direct-child node
	// names. Empty / unset → expose mode. Non-empty → target-node mode.
	PropReshuffleTargetNodes = "reshuffle_target_nodes"
)

// ReshuffleStep describes a single move in a reshuffle plan.
type ReshuffleStep struct {
	Sequence int
	StepType protocol.StepType // "unbury", "retrieve"
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

// blocker bundles a bin sitting in a slot shallower than the target
// with its slot reference and depth — used by both the legacy
// PlanReshuffle and the new dual-mode variants.
type reshuffleBlocker struct {
	bin   *bins.Bin
	slot  *nodes.Node
	depth int
}

// findBuriedBlockers returns every occupied lane slot shallower than
// targetDepth. Shared between PlanReshuffle, PlanReshuffleUnburyOnly,
// and PlanReshuffleToTarget.
func findBuriedBlockers(db *store.DB, lane *nodes.Node, targetDepth int) ([]reshuffleBlocker, error) {
	slots, err := db.ListLaneSlots(lane.ID)
	if err != nil {
		return nil, fmt.Errorf("list lane slots: %w", err)
	}

	var blockers []reshuffleBlocker
	for _, slot := range slots {
		depth, err := db.GetSlotDepth(slot.ID)
		if err != nil || depth >= targetDepth {
			continue
		}
		laneBins, err := db.ListBinsByNode(slot.ID)
		if err != nil || len(laneBins) == 0 {
			continue
		}
		blockers = append(blockers, reshuffleBlocker{bin: laneBins[0], slot: slot, depth: depth})
	}
	return blockers, nil
}

// PlanReshuffle creates a plan to unbury a target bin in a lane.
// Steps: move blockers front-to-back to shuffle slots, then retrieve the target.
// Blockers are NOT restocked — they lie where the unbury parked them (deepest-
// first parking keeps the lane bubble-free), and are ordinary findable inventory.
//
// Used by simple-retrieve reshuffles where the unburied bin is
// delivered to the parent retrieve's lineside DeliveryNode. Complex-
// order reshuffles use PlanReshuffleUnburyOnly or PlanReshuffleToTarget
// instead — see Step 3.5 of the buried-bin reshuffle scope.
func PlanReshuffle(db *store.DB, target *bins.Bin, targetSlot *nodes.Node, lane *nodes.Node, groupID int64) (*ReshufflePlan, error) {
	if targetSlot.ParentID == nil {
		return nil, fmt.Errorf("target slot has no parent lane")
	}

	targetDepth, err := db.GetSlotDepth(targetSlot.ID)
	if err != nil {
		return nil, fmt.Errorf("get target depth: %w", err)
	}

	blockers, err := findBuriedBlockers(db, lane, targetDepth)
	if err != nil {
		return nil, err
	}

	shuffleSlots, err := findShuffleSlots(db, lane.ID, groupID, len(blockers))
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
			StepType: protocol.StepUnbury,
			BinID:    b.bin.ID,
			FromNode: b.slot,
			ToNode:   shuffleSlots[i],
		})
		seq++
	}

	// Step 2: Retrieve the target (this is the actual order delivery)
	plan.Steps = append(plan.Steps, ReshuffleStep{
		Sequence: seq,
		StepType: protocol.StepRetrieve,
		BinID:    target.ID,
		FromNode: targetSlot,
	})

	// No restock step: blockers stay in the shuffle slots the unbury moved them
	// to. "Blockers lie" — deepest-first parking keeps the lane packed and
	// bubble-free, and a parked blocker is ordinary findable inventory.
	return plan, nil
}

// PlanReshuffleUnburyOnly creates a plan that only moves blockers out
// of the way, leaving the target bin in its original lane slot.
// Complex-order reshuffles use this variant in "expose mode" — the
// complex parent resumes after the compound completes and runs its
// original first pickup against the now-accessible slot.
//
// No retrieve step (the parent handles that) and no restock step: blockers lie
// where the unbury parked them. The old restore-blockers subsystem — which moved
// blockers back after pickup and left permanent air bubbles when off (the former
// KNOWN ISSUE here) — is gone; deepest-first shuffle-slot parking (findShuffleSlots)
// keeps the lane packed without it, so a parked blocker is ordinary findable
// inventory where it sits.
func PlanReshuffleUnburyOnly(db *store.DB, target *bins.Bin, targetSlot *nodes.Node, lane *nodes.Node, groupID int64) (*ReshufflePlan, error) {
	if targetSlot.ParentID == nil {
		return nil, fmt.Errorf("target slot has no parent lane")
	}

	targetDepth, err := db.GetSlotDepth(targetSlot.ID)
	if err != nil {
		return nil, fmt.Errorf("get target depth: %w", err)
	}

	blockers, err := findBuriedBlockers(db, lane, targetDepth)
	if err != nil {
		return nil, err
	}

	shuffleSlots, err := findShuffleSlots(db, lane.ID, groupID, len(blockers))
	if err != nil {
		return nil, fmt.Errorf("find shuffle slots: %w", err)
	}

	plan := &ReshufflePlan{
		TargetBin:    target,
		TargetSlot:   targetSlot,
		Lane:         lane,
		ShuffleSlots: shuffleSlots,
	}
	for i, b := range blockers {
		plan.Steps = append(plan.Steps, ReshuffleStep{
			Sequence: i + 1,
			StepType: protocol.StepUnbury,
			BinID:    b.bin.ID,
			FromNode: b.slot,
			ToNode:   shuffleSlots[i],
		})
	}
	return plan, nil
}

// PlanReshuffleToTarget creates a plan that unburies the blockers AND
// moves the target bin to a specific direct-child node of the group
// ("target-node mode"). The complex parent re-resolves against the
// group after the compound completes, finds the target bin at the
// configured target node, and dispatches normally.
//
// targetNode must be set explicitly so the retrieve step's
// DeliveryNode is non-empty — otherwise compound.go's fallback would
// default it to parentOrder.DeliveryNode, which is the last step's
// node for a complex parent (extractEndpoints), not the first dropoff.
func PlanReshuffleToTarget(db *store.DB, target *bins.Bin, targetSlot *nodes.Node, lane *nodes.Node, groupID int64, targetNode *nodes.Node) (*ReshufflePlan, error) {
	if targetSlot.ParentID == nil {
		return nil, fmt.Errorf("target slot has no parent lane")
	}
	if targetNode == nil {
		return nil, fmt.Errorf("target-node mode requires a non-nil target node")
	}

	targetDepth, err := db.GetSlotDepth(targetSlot.ID)
	if err != nil {
		return nil, fmt.Errorf("get target depth: %w", err)
	}

	blockers, err := findBuriedBlockers(db, lane, targetDepth)
	if err != nil {
		return nil, err
	}

	shuffleSlots, err := findShuffleSlots(db, lane.ID, groupID, len(blockers))
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
	for i, b := range blockers {
		plan.Steps = append(plan.Steps, ReshuffleStep{
			Sequence: seq,
			StepType: protocol.StepUnbury,
			BinID:    b.bin.ID,
			FromNode: b.slot,
			ToNode:   shuffleSlots[i],
		})
		seq++
	}
	plan.Steps = append(plan.Steps, ReshuffleStep{
		Sequence: seq,
		StepType: protocol.StepRetrieve,
		BinID:    target.ID,
		FromNode: targetSlot,
		ToNode:   targetNode,
	})
	return plan, nil
}

// ReshuffleTargetNodes parses the JSON array stored under the
// PropReshuffleTargetNodes property. It is a per-LANE override with a
// group fallback: a lane that sets its own targets wins, otherwise the
// group's value applies (mirrors the node→parent fallback used for
// staging_ttl). Pass laneID=0 to read the group value directly. Returns
// an empty slice when both are unset or malformed (treat malformed as
// expose mode rather than failing — the configurator validates on save).
func ReshuffleTargetNodes(db *store.DB, laneID, groupID int64) []string {
	raw := ""
	if laneID != 0 {
		raw = db.GetNodeProperty(laneID, PropReshuffleTargetNodes)
	}
	if raw == "" {
		raw = db.GetNodeProperty(groupID, PropReshuffleTargetNodes)
	}
	if raw == "" {
		return nil
	}
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

// findShuffleSlots locates empty accessible slots for temporary shuffle storage.
// Pass 1: direct physical children of the group (always accessible).
// Pass 2: accessible empty slots in regular lanes.
//
// Direct-child nodes named in the group's reshuffle_target_nodes
// property are skipped in both passes so the bin handoff destination
// for complex-order target-node mode reshuffles stays reserved. The
// exclusion applies to ALL reshuffle paths on the group (simple
// retrieve too) — they share this helper. Document on the admin
// page that configuring target nodes shrinks the shuffle pool for
// the whole group.
func findShuffleSlots(db *store.DB, laneID, groupID int64, count int) ([]*nodes.Node, error) {
	children, err := db.ListChildNodes(groupID)
	if err != nil {
		return nil, err
	}

	excluded := make(map[string]bool)
	for _, name := range ReshuffleTargetNodes(db, laneID, groupID) {
		excluded[name] = true
	}

	var available []*nodes.Node

	// Pass 1: direct physical children of the group (always accessible).
	// Reverse-iterate so any depth-carrying direct children are visited
	// deepest-first — matches the lane-FIFO invariant maintained in Pass 2.
	for i := len(children) - 1; i >= 0; i-- {
		c := children[i]
		if !c.Enabled || c.IsSynthetic {
			continue
		}
		if excluded[c.Name] {
			continue
		}
		if !shuffleSlotFree(db, c) {
			continue
		}
		available = append(available, c)
		if len(available) >= count {
			return available, nil
		}
	}

	// Pass 2: any empty accessible slot across all lanes.
	// ListLaneSlots returns slots ORDER BY depth ASC; we reverse-iterate so
	// the DEEPEST empty slot is taken first. Filling shallow-first violates
	// the lane FIFO invariant — a bin at depth 1 makes IsSlotAccessible
	// false for every deeper slot, even ones the plan picked as future
	// pickup/dropoff destinations. If ListLaneSlots' ORDER BY ever changes,
	// this reverse-iterate silently breaks.
	for _, c := range children {
		if !c.Enabled || c.NodeTypeCode != protocol.NodeClassLANE {
			continue
		}
		if excluded[c.Name] {
			continue
		}
		slots, _ := db.ListLaneSlots(c.ID)
		for i := len(slots) - 1; i >= 0; i-- {
			slot := slots[i]
			if !slot.Enabled {
				continue
			}
			if excluded[slot.Name] {
				continue
			}
			acc, _ := db.IsSlotAccessible(slot.ID)
			if !acc {
				continue
			}
			if !shuffleSlotFree(db, slot) {
				continue
			}
			available = append(available, slot)
			if len(available) >= count {
				return available, nil
			}
		}
	}

	if len(available) < count {
		return nil, fmt.Errorf("%w: need %d shuffle slots but only %d available", ErrNoShuffleSlot, count, len(available))
	}
	return available, nil
}

// shuffleSlotFree reports whether a dig may park a blocker in this node.
//
// Shuffle slots are a GROUP-scoped shared resource, but the lane lock is keyed on
// the lane being dug (planBuriedReshuffle → laneLock.TryLock(buried.LaneID)). Two
// digs in DIFFERENT lanes therefore take different locks, both proceed, and then
// compete for the same shuffle slots. This used to test "is the node empty RIGHT
// NOW" (CountBinsByNode == 0) and nothing else — so a slot with another dig's
// blocker already in flight to it looked free. Both digs picked it, the second
// blocker landed on the first, and ApplyArrival's EvictStaleGhostsTx threw the
// first bin to _TRANSIT. Observed on the houseserver sim 2026-07-13: lane 1 and
// lane 2 each unburied into SMN_008 + SMN_009 three seconds apart, orphaning two
// bins and leaving lane 1's restore compound with nothing to restock (D83a).
//
// CheckDropoffCapacity is the gate every OTHER dropoff in the system passes
// through, and it already tests exactly what was missing: occupied, OR an order
// in flight inbound. The unbury legs carry delivery_node, so they are counted --
// the information was always there, findShuffleSlots just never asked. Reusing the
// gate (rather than reserving shuffle slots) is deliberate: a real span/mouth
// reservation for the dig is Track 3's open entry gate, and this must not
// pre-empt that design.
//
// ClaimedBy is checked too, mirroring what the storage resolver already does for
// ordinary slots (group_resolver.go's "slot already claimed by another order's
// dispatch").
//
// Tightening this makes "no free shuffle slot" MORE frequent — which is safe only
// because that outcome now WAITS instead of failing terminally (ErrNoShuffleSlot,
// same commit). The two changes are a pair; do not keep one without the other.
func shuffleSlotFree(db *store.DB, n *nodes.Node) bool {
	if n.ClaimedBy != nil {
		return false
	}
	blocked, _ := CheckDropoffCapacity(db, n.Name, 0)
	return !blocked
}

// ErrNoShuffleSlot means the reshuffle has nowhere to park its blockers RIGHT
// NOW. This is congestion, not a fault: a shuffle slot frees the moment any
// other order clears one, so the order must WAIT and retry, never fail.
//
// It used to fail terminally — findShuffleSlots returned a bare error, planning
// mapped it to codeReshuffle, and codeReshuffle was not in Transient(). Sim order
// 21 on the 2026-07-10 houseserver run died exactly this way ("cannot plan
// reshuffle: need 1 slot, 0 available"), which is what surfaced it. That is
// inconsistent with the D18-Q4 wait-not-fail principle the simple path upholds,
// and D79's reshuffle-disposition rider assigned the fix to this fast-follow:
// once the scanner can spawn reshuffles on replay, a buried retrieve retries
// across ticks (waits for a slot) instead of one-shot-failing at intake.
var ErrNoShuffleSlot = errors.New("no free shuffle slot")
