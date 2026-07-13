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

	// PropReshuffleRestoreBlockers is "on" or "off". When "on", after
	// the parent picks up the target bin, blockers are moved back to
	// their original lane slots via a synthetic-parent restock
	// compound.
	PropReshuffleRestoreBlockers = "reshuffle_restore_blockers"
)

// ReshuffleStep describes a single move in a reshuffle plan.
type ReshuffleStep struct {
	Sequence int
	StepType protocol.StepType // "unbury", "retrieve", "restock"
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

// restockDestinations packs blockers back into the lane deepest-first (no bubbles)
// by slot rotation, instead of returning each to its original slot. Post-unbury +
// retrieve the lane's empty slots are exactly the blockers' original slots (depths
// 1..N-1) plus the target's slot (depth N). Assigning each blocker the next-deeper
// slot fills depths 2..N and leaves depth 1 (the mouth) empty — physically realistic
// (a robot places into the reachable mouth; it can't skip past an empty slot) and
// bubble-free. FIFO is unaffected: FindSourceFIFO keys on loaded_at age, not slot
// depth, so moving a bin to a different slot doesn't change its pick order.
//
// blockers are shallowest-first (findBuriedBlockers via ListLaneSlots depth ASC).
// Returns one destination node per blocker, indexed to match the blockers slice.
func restockDestinations(blockers []reshuffleBlocker, targetSlot *nodes.Node) []*nodes.Node {
	n := len(blockers)
	if n == 0 {
		return nil
	}
	dests := make([]*nodes.Node, n)
	// The deepest blocker (index n-1, depth N-1) restocks to the target's slot (depth N).
	dests[n-1] = targetSlot
	// Each shallower blocker restocks one deeper than itself — i.e. to the slot of
	// the next-deeper blocker. After this, depths 2..N are filled, depth 1 is empty.
	for i := 0; i < n-1; i++ {
		dests[i] = blockers[i+1].slot
	}
	return dests
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
// Steps: move blockers front-to-back to shuffle slots, retrieve target, restock blockers deepest-first.
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
	seq++

	// Step 3: Restock blockers back to the lane deepest-first (slot rotation — no
	// bubbles). restockDestinations packs each blocker one slot deeper than itself so
	// the lane ends with depths 2..N filled and the mouth (depth 1) empty.
	restockDests := restockDestinations(blockers, targetSlot)
	for i := len(blockers) - 1; i >= 0; i-- {
		plan.Steps = append(plan.Steps, ReshuffleStep{
			Sequence: seq,
			StepType: protocol.StepRestock,
			BinID:    blockers[i].bin.ID,
			FromNode: shuffleSlots[i],
			ToNode:   restockDests[i],
		})
		seq++
	}

	return plan, nil
}

// ╔═══════════════════════════════════════════════════════════════════════════╗
// ║  !! KNOWN ISSUE -- READ BEFORE REFACTORING THE RESHUFFLE PATH !!           ║
// ║                                                                           ║
// ║  This complex-order path moves blockers OUT to shuffle slots and, by      ║
// ║  DEFAULT, NEVER restocks them: reshuffle_restore_blockers defaults OFF,   ║
// ║  so (per ReshuffleRestoreBlockersEnabled) "blockers stay in shuffle slots ║
// ║  and lane geometry shifts." Over a running loop that leaves permanent     ║
// ║  AIR BUBBLES / drifted lane geometry — supermarkets that never re-compact.║
// ║                                                                           ║
// ║  The simple-retrieve path (PlanReshuffle) does NOT have this problem: it  ║
// ║  restocks blockers deepest-first via restockDestinations (bubble-free).   ║
// ║  The ASYMMETRY is the bug. When you refactor this: make the complex path  ║
// ║  recompact deepest-first too (reuse restockDestinations), or make         ║
// ║  restore-blockers the default — so BOTH paths keep lanes packed.          ║
// ║  Surfaced empirically on the dev sim (air bubbles in every supermarket).  ║
// ║  See GitHub-root shingo-sim-approach-2026-07-11.md.                       ║
// ╚═══════════════════════════════════════════════════════════════════════════╝
//
// PlanReshuffleUnburyOnly creates a plan that only moves blockers out
// of the way, leaving the target bin in its original lane slot.
// Complex-order reshuffles use this variant in "expose mode" — the
// complex parent resumes after the compound completes and runs its
// original first pickup against the now-accessible slot.
//
// No retrieve step (the parent handles that), no restock step (the
// optional restore-blockers behavior is governed by a separate per-
// group toggle and emits its own synthetic-parent compound after the
// parent picks up).
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

// ReshuffleRestoreBlockersEnabled reports whether the restore-blockers
// toggle is on. Per-LANE override with group fallback: an explicit "on"
// or "off" on the lane wins; if the lane is unset (inherit) the group's
// value applies. Default off — blockers stay in shuffle slots and lane
// geometry shifts. Pass laneID=0 to read the group value directly.
func ReshuffleRestoreBlockersEnabled(db *store.DB, laneID, groupID int64) bool {
	if laneID != 0 {
		switch db.GetNodeProperty(laneID, PropReshuffleRestoreBlockers) {
		case "on":
			return true
		case "off":
			return false
		}
	}
	return db.GetNodeProperty(groupID, PropReshuffleRestoreBlockers) == "on"
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
