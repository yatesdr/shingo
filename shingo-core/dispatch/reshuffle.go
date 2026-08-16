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
// with its slot reference — used by both the legacy PlanReshuffle and
// the new dual-mode variants.
type reshuffleBlocker struct {
	bin  *bins.Bin
	slot *nodes.Node
}

// findBuriedBlockers returns the bins occupying the slots in front of
// targetSlotID, shallowest first — the dig list. Shared between
// PlanReshuffle, PlanReshuffleUnburyOnly, PlanReshuffleToTarget, and the
// lane gate's retrieve classifier.
//
// "In front of" is store/nodes.BlockersInFrontOf and nothing else. This used
// to walk ListLaneSlots comparing GetSlotDepth against a targetDepth the
// callers passed in, which was a SECOND implementation of the reachability
// predicate and it did not agree with the SQL one: GetSlotDepth reports 0 for
// a NULL depth, so a depth-less occupied child of the lane was in front of
// everything here and invisible everywhere else. Asking the one definition
// removes the disagreement rather than patching this side of it.
//
// It also stops taking the lane as a parameter. Every caller passed the
// target slot's own parent — the set is derived from the slot's lane now, so
// the two can no longer be handed in inconsistently.
//
// A read error is returned, never swallowed. Callers must fail closed: an
// unreadable lane is treated as blocked, because refusing to dig is
// recoverable and digging into a lane you could not read is not.
func findBuriedBlockers(db *store.DB, targetSlotID int64) ([]reshuffleBlocker, error) {
	slots, err := db.BlockersInFrontOf(targetSlotID)
	if err != nil {
		return nil, fmt.Errorf("blockers in front of slot %d: %w", targetSlotID, err)
	}

	blockers := make([]reshuffleBlocker, 0, len(slots))
	for _, slot := range slots {
		laneBins, err := db.ListBinsByNode(slot.ID)
		if err != nil {
			return nil, fmt.Errorf("list bins at blocker slot %s: %w", slot.Name, err)
		}
		if len(laneBins) == 0 {
			continue // the bin left between the scan and this read — no longer a blocker
		}
		blockers = append(blockers, reshuffleBlocker{bin: laneBins[0], slot: slot})
	}
	return blockers, nil
}

// planUnbury is the excavation, which is all three planners agree on: check the
// slot has a lane, list what is in front of the target, find somewhere to park
// each of those, and emit one unbury step per blocker, shallowest first.
//
// The three exported planners were three copies of this with different tails —
// nothing, a retrieve, or a retrieve to a named node — and the copies had already
// started to drift: two counted their sequence with a running `seq` and the third
// with `i + 1`, which agreed only because the loop was the first thing in the
// plan. That is the kind of difference nobody chooses.
//
// It returns the next sequence number so a caller that appends knows where to
// carry on. Blockers are NOT restocked by any of the three — they lie where the
// unbury parked them. Deepest-first parking (findShuffleSlots) keeps the lane
// packed without a restock, and a parked blocker is ordinary findable inventory
// where it sits. The old restore-blockers subsystem, which moved them back and
// left permanent air bubbles when it was off, is gone.
func planUnbury(db *store.DB, target *bins.Bin, targetSlot, lane *nodes.Node, groupID int64) (*ReshufflePlan, int, error) {
	if targetSlot.ParentID == nil {
		return nil, 0, fmt.Errorf("target slot has no parent lane")
	}

	blockers, err := findBuriedBlockers(db, targetSlot.ID)
	if err != nil {
		return nil, 0, err
	}

	shuffleSlots, err := findShuffleSlots(db, lane.ID, groupID, len(blockers))
	if err != nil {
		return nil, 0, fmt.Errorf("find shuffle slots: %w", err)
	}

	plan := &ReshufflePlan{
		TargetBin:    target,
		TargetSlot:   targetSlot,
		Lane:         lane,
		ShuffleSlots: shuffleSlots,
	}
	seq := 1
	// Front-to-back order = shallowest first, which is the order a robot can
	// physically take them out in.
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
	return plan, seq, nil
}

// PlanReshuffle creates a plan to unbury a target bin in a lane and then retrieve
// it: the excavation, then the delivery.
//
// The retrieve step's ToNode is deliberately left nil — compound.go backfills the
// parent retrieve's lineside DeliveryNode, which for a simple retrieve IS the
// destination. Complex-order reshuffles use PlanReshuffleUnburyOnly or
// PlanReshuffleToTarget instead, because a complex parent's DeliveryNode is its
// LAST step's node and that fallback would send the bin to the wrong place.
func PlanReshuffle(db *store.DB, target *bins.Bin, targetSlot *nodes.Node, lane *nodes.Node, groupID int64) (*ReshufflePlan, error) {
	plan, seq, err := planUnbury(db, target, targetSlot, lane, groupID)
	if err != nil {
		return nil, err
	}
	plan.Steps = append(plan.Steps, ReshuffleStep{
		Sequence: seq,
		StepType: protocol.StepRetrieve,
		BinID:    target.ID,
		FromNode: targetSlot,
	})
	return plan, nil
}

// PlanReshuffleUnburyOnly creates a plan that only moves blockers out of the way,
// leaving the target bin in its original lane slot — "expose mode". The complex
// parent resumes after the compound completes and runs its original first pickup
// against the now-accessible slot, so the parent owns the retrieve and this plan
// must not.
func PlanReshuffleUnburyOnly(db *store.DB, target *bins.Bin, targetSlot *nodes.Node, lane *nodes.Node, groupID int64) (*ReshufflePlan, error) {
	plan, _, err := planUnbury(db, target, targetSlot, lane, groupID)
	return plan, err
}

// PlanReshuffleToTarget unburies the blockers AND moves the target bin to a
// specific direct-child node of the group ("target-node mode"). The complex
// parent re-resolves against the group after the compound completes, finds the
// target bin at the configured target node, and dispatches normally.
//
// targetNode must be set explicitly so the retrieve step's DeliveryNode is
// non-empty — otherwise compound.go's fallback would default it to
// parentOrder.DeliveryNode, which is the last step's node for a complex parent
// (extractEndpoints), not the first dropoff.
func PlanReshuffleToTarget(db *store.DB, target *bins.Bin, targetSlot *nodes.Node, lane *nodes.Node, groupID int64, targetNode *nodes.Node) (*ReshufflePlan, error) {
	// Before the reads, so a caller that forgot the node is told so rather than
	// finding out after a lane walk.
	if targetNode == nil {
		return nil, fmt.Errorf("target-node mode requires a non-nil target node")
	}
	plan, seq, err := planUnbury(db, target, targetSlot, lane, groupID)
	if err != nil {
		return nil, err
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

	// A GATED DIG MAY NOT PARK ITS BLOCKER IN A DIFFERENT GATED LANE, because
	// spliceLaneWait refuses a plan that touches two of them — one wait per plan,
	// and releasing per-wait is machinery the transform deliberately does not
	// build (lane_gate_dispatch.go rule 2).
	//
	// Found on the lane-stress rig 2026-08-09, within minutes of it coming up:
	// every dig out of a marked lane whose blocker landed in the marked empty
	// lane failed at the splice, which failed the parent, which failed the
	// two-robot swap the parent was supplying, which cancelled the evac. One
	// unexpressible plan, and the line was starved. Nothing self-clears either --
	// both marks stay where they are, so the re-plan picks the same slot and
	// fails the same way.
	//
	// This is the same shape as the dug-lane exclusion below, and lands here for
	// the same reason: a slot the plan cannot legally use is not a candidate, and
	// plan-time is where a candidate list belongs. The alternative -- letting the
	// splice refuse and dispositioning the refusal better -- treats the symptom;
	// the dig never wanted that slot, it wanted A slot.
	//
	// Running out because of this WAITS. ErrNoShuffleSlot is transient and
	// retries, which is exactly the disposition the last tightening of this
	// function relied on (see shuffleSlotFree). A dig that can only reach gated
	// lanes waits for an ungated slot to free rather than dying.
	//
	// Only when the DUG lane is itself gated: a plan touching one gated lane is
	// fine, and so is one touching the same gated lane twice.
	dugLaneGated := db.GetNodeProperty(laneID, PropLaneGatePoint) != ""

	var available []*nodes.Node

	// A candidate whose reachability could not be READ is not a candidate — fail
	// closed — but it is not a fault either, and the difference matters
	// here more than anywhere else. planBuriedReshuffle maps a bare error from
	// this function to codeReshuffle, which is TERMINAL; only ErrNoShuffleSlot
	// is transient. So an unreadable candidate is skipped and counted, and if
	// the pool then comes up short the shortfall is reported as congestion —
	// which waits and retries — rather than as geometry, which kills the order.
	// Silently skipping was safe; silently skipping and then reporting a pool
	// size as if it were the true one is what this stops.
	unreadable := 0

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
		// NEVER park a blocker back into the lane it is being dug out of. laneID
		// was a parameter of this function that Pass 2 never compared against
		// anything — it was read once, at the top, for the reshuffle_target_nodes
		// override — so the loop happily offered the dug lane's own free slots. On
		// a lane holding [empty, blocker, target] that means moving the blocker
		// from depth 2 to depth 1, leaving it in front of the target the dig
		// exists to uncover.
		//
		// Survivable today only because the dig holds the lane exclusively, so
		// nothing else competes for those slots. It stops being survivable the
		// moment lane concurrency is relaxed, which is why it lands before that
		// rather than with it.
		if c.ID == laneID {
			continue
		}
		if dugLaneGated && db.GetNodeProperty(c.ID, PropLaneGatePoint) != "" {
			continue // the splice cannot express two gated lanes on one plan
		}
		if excluded[c.Name] {
			continue
		}
		slots, err := db.ListLaneSlots(c.ID)
		if err != nil {
			unreadable++
			continue
		}
		for i := len(slots) - 1; i >= 0; i-- {
			slot := slots[i]
			if !slot.Enabled {
				continue
			}
			if excluded[slot.Name] {
				continue
			}
			acc, err := db.IsSlotAccessible(slot.ID)
			if err != nil {
				unreadable++
				continue
			}
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
		detail := ""
		if unreadable > 0 {
			detail = fmt.Sprintf(" (%d candidate(s) unreadable, treated as unusable)", unreadable)
		}
		return nil, fmt.Errorf("%w: need %d shuffle slots but only %d available%s", ErrNoShuffleSlot, count, len(available), detail)
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
// bins and leaving lane 1's restore compound with nothing to restock.
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
// inconsistent with the wait-not-fail rule the simple path upholds, and the fix
// had to wait for the scanner: once it can spawn reshuffles on replay, a buried
// retrieve retries across ticks (waits for a slot) instead of one-shot-failing at
// intake.
var ErrNoShuffleSlot = errors.New("no free shuffle slot")
