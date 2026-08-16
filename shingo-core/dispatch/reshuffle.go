package dispatch

import (
	"errors"
	"fmt"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
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
//
// LANE AND SHUFFLESLOTS ARE GONE, and they were write-only for their whole life:
// every planner filled them and nothing anywhere read either one (verified by
// grep — the ShuffleSlots hits are testdb.CompoundScenario's field of the same
// name, a different struct). They were not merely unused, they were an INVITATION
// to re-derive facts that live somewhere better:
//
//   - the LANE a dig holds is the reservation row itself, which is what
//     LaneLock.UnlockByOwner now returns. Carrying a second copy on the plan
//     would have handed the teardown path a stale answer after a re-plan — the
//     exact archaeology that made the old unlock walk wrong.
//   - the SHUFFLE SLOTS a dig picked are on its steps, as each unbury's ToNode.
//     A parallel slice of the same nodes is a second spelling that can fall out
//     of step with the steps it describes.
//
// Both are cheap to restore if a reader ever appears; neither should be restored
// as a convenience.
type ReshufflePlan struct {
	// TargetBin is the bin the dig exists to reach, and it is NIL for a dig that
	// exists to reach a SLOT (PlanLaneMouthClear). Every reader must handle that:
	// the one production reader is CreateCompoundOrder's history detail, which
	// says so.
	TargetBin  *bins.Bin
	TargetSlot *nodes.Node
	Steps      []ReshuffleStep
}

// blocker bundles a bin sitting in a slot shallower than the target
// with its slot reference — used by both the legacy PlanReshuffle and
// the new dual-mode variants.
type reshuffleBlocker struct {
	bin  *bins.Bin
	slot *nodes.Node
}

// findBuriedBlockers returns the bins occupying the slots in front of
// targetSlotID, shallowest first — the dig list. Shared between every planner
// and the lane gate's retrieve classifier.
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

// planUnbury is the excavation, which is what every planner agrees on: check the
// slot has a lane, list what is in front of the target, find somewhere to park
// each of those, and emit one unbury step per blocker, shallowest first.
//
// The exported planners were copies of this with different tails — nothing, or a
// retrieve — and the copies had already started to drift: some counted their
// sequence with a running `seq` and one with `i + 1`, which agreed only because
// the loop was the first thing in the plan. That is the kind of difference
// nobody chooses.
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
		TargetBin:  target,
		TargetSlot: targetSlot,
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
// destination. Complex-order reshuffles use PlanReshuffleUnburyOnly instead,
// because a complex parent's DeliveryNode is its LAST step's node and that
// fallback would send the bin to the wrong place.
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

// PlanLaneMouthClear plans the excavation that makes targetSlot reachable and
// STOPS THERE: this dig exists to open a path, not to fetch anything.
//
// The other two planners are both built around a target BIN somebody wants —
// PlanReshuffle retrieves it, PlanReshuffleUnburyOnly exposes it for a parent
// that will come back for it. Window 3's dig has no such bin. What is wanted is the SLOT, and the order that
// wants it is already standing at the lane's mark with a robot under it. So the
// plan is planUnbury and nothing after it, and TargetBin stays nil.
//
// AN EMPTY EXCAVATION IS AN ERROR, NOT A NO-OP PLAN. A plan with no steps would
// still create a parent, take the lane's dig lock, complete on the next tick and
// release it — a lot of machinery for a lane nothing was blocking. The caller has
// already established that the lane IS blocked, so no blockers here means the
// lane moved underneath us between the two reads, and the right answer is to do
// nothing and re-ask on the next pass.
func PlanLaneMouthClear(db *store.DB, targetSlot, lane *nodes.Node, groupID int64) (*ReshufflePlan, error) {
	plan, _, err := planUnbury(db, nil, targetSlot, lane, groupID)
	if err != nil {
		return nil, err
	}
	if len(plan.Steps) == 0 {
		return nil, fmt.Errorf("%w: slot %s", ErrNothingInTheWay, targetSlot.Name)
	}
	return plan, nil
}

// ErrNothingInTheWay means a path-clearing dig was asked for against a slot that
// nothing is in front of. Transient by nature — the lane changed between the
// decision and the plan — so callers do nothing and re-ask, exactly as they do
// for ErrNoShuffleSlot.
var ErrNothingInTheWay = errors.New("nothing is in front of the target slot")

// findShuffleSlots locates empty accessible slots for temporary shuffle storage.
// Pass 1: direct physical children of the group (always accessible).
// Pass 2: accessible empty slots in regular lanes.
//
// It used to narrow the pool on CONFIGURATION — reserving a handoff destination
// for the deleted target-node mode (tombstone: complex_reshuffle.go). Every
// remaining exclusion is about what a dig may physically do.
func findShuffleSlots(db *store.DB, laneID, groupID int64, count int) ([]*nodes.Node, error) {
	children, err := db.ListChildNodes(groupID)
	if err != nil {
		return nil, err
	}

	// A GATED DIG DOES NOT PARK ITS BLOCKER IN A DIFFERENT GATED LANE.
	//
	// ── THE REASON THIS WAS ADDED IS GONE; THE EXCLUSION IS KEPT ANYWAY ───
	//
	// It was added because spliceLaneWait REFUSED a plan touching two gated lanes,
	// so such a leg could not be dispatched at all. Multi-gate plans are built now
	// (lane_gate_dispatch.go rule 2), and that leg would dispatch cleanly: a wait
	// at each mark, each released by its own lane.
	//
	// What survives the change is a different objection, and it is about the DIG
	// rather than about the plan. A dig holds its lane EXCLUSIVELY for the whole
	// excavation. Sending one of its legs to dwell at another lane's mark makes
	// the dug lane's exclusive hold last as long as a SECOND lane's congestion —
	// a wait, lawful and self-clearing, but one that keeps a whole corridor shut
	// while it lasts and blocks every unrelated order aimed at it. Parking in an
	// ungated slot costs the dig nothing and takes no second lane hostage.
	//
	// So this is now a CONSERVATISM rather than an impossibility, and it is worth
	// saying which, because the next person to widen the shuffle pool will come
	// looking here: the constraint that forced it has been lifted, and lifting
	// this too is a real option with a measurable cost on both sides. What it
	// buys is pool width, which the dig cascade (F-10) is sensitive to. What it
	// risks is lane-hold duration. Neither is guessed at cheaply — measure it.
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

	// NEVER RE-BURY A BIN AN EXPOSE HOLD IS PROTECTING — NOT EVEN YOUR OWN.
	//
	// This is F-19, and it is the one that stopped the lane-stress plant dead.
	//
	// A dig that finishes in EXPOSE mode does not release its lane. The lock is
	// TRANSFERRED to the complex parent and held until that parent walks back and
	// picks up the bin it just uncovered (compound.go extendLaneLockForExposeMode),
	// with the promise written down as "closes the post-compound / pre-pickup
	// re-burial window". The lock kept other robots OUT. It did nothing about the
	// slots the excavation had just emptied, which this function went on handing
	// out as shuffle space — so the window it named stayed wide open.
	//
	// So the parent's NEXT dig parked its blockers into the lane its LAST dig had
	// just emptied, and because Pass 2 fills DEEPEST-FIRST it packed from the back
	// forward and entombed the exposed bin under a full lane. Measured 2026-08-10:
	// order 1 ran three generations and twelve legs in eight minutes, dug bins
	// 2/3/4 out twice, never picked anything up, and took another lane lock each
	// time. Eight of twenty-two lanes ended up held and the plant stopped creating
	// orders entirely.
	//
	// ── WHY THIS IS PER-SLOT AND NOT "SKIP DIG-LOCKED LANES" ──────────────
	//
	// The blunt rule was tried first and TestCrossFlow_TwoDigsOneLane caught it.
	// A lane whose dig is still RUNNING is a legitimate place to park: its target
	// is buried anyway, the mouth slot is genuinely free, and refusing it starves a
	// dig that had somewhere to go — wait-not-fail broken in the other direction.
	// The two cases look identical from the lane and differ entirely in what is
	// being protected:
	//
	//	dig still excavating -> target still buried  -> parking changes nothing
	//	dig finished, EXPOSED -> target reachable NOW -> parking undoes the whole dig
	//
	// pending_lane_extensions is exactly the second case written down, and
	// ExpectedFromNodeID is the slot being protected. So the exclusion is only the
	// slots that would sit IN FRONT of it — shallower in that lane — and the rest of
	// the lane stays available.
	//
	// "NOT EVEN YOUR OWN" IS THE LOAD-BEARING HALF. Every other ownership test here
	// exempts the owner — ownsDig, the claim CAS, admission — and exempting it here
	// is precisely what let a parent bury its own prize. Owner-blind on purpose.
	//
	// ── THIS IS NOT THE DIG-LOCK QUESTION, AND MUST NOT BE FOLDED INTO IT ──
	//
	// reservations.DigAsker.ExcludedBy now gives the dig LOCK one spelling across
	// admission and sourcing, and the obvious next step — route this reader
	// through it too — is wrong twice over, so it is refused here in writing.
	//
	// Different FACT: the lock is a mouth row on a lane; this reads
	// pending_lane_extensions, which exists only for a dig that has already
	// finished in expose mode. A lane can carry either without the other.
	//
	// Different ANSWER for the owner: the dig-lock question exempts the asker,
	// this one must not, per the paragraph above. Unifying them would hand the
	// exemption to exactly the caller the exemption breaks.
	//
	// And the blunt version was already tried and reverted:
	// TestCrossFlow_TwoDigsOneLane_BsLegWinsTheRace fails if a lane whose dig is
	// still RUNNING is refused as parking, because that lane's target is buried
	// anyway and refusing it starves a dig that had somewhere to go.
	protectedDepth := map[int64]int{} // lane -> depth of a bin an expose hold protects
	exts, hErr := db.ListPendingLaneExtensions()
	if hErr != nil {
		// Cannot tell what is protected, so cannot safely offer anything. Reported
		// as congestion (which waits and retries) rather than geometry (which kills
		// the order) — the same disposition every other shortfall here takes.
		return nil, fmt.Errorf("%w: could not read expose holds: %v", ErrNoShuffleSlot, hErr)
	}
	for _, e := range exts {
		slot, sErr := db.GetNode(e.ExpectedFromNodeID)
		if sErr != nil || slot == nil || slot.Depth == nil || slot.ParentID == nil {
			continue
		}
		if cur, seen := protectedDepth[*slot.ParentID]; !seen || *slot.Depth > cur {
			protectedDepth[*slot.ParentID] = *slot.Depth
		}
	}

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
		// anything — it was read once, at the top, for the deleted config override
		// — so the loop happily offered the dug lane's own free slots. On
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
			continue // a dig leg dwelling at a second mark holds two lanes at once
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
			// IN FRONT OF A PROTECTED, ALREADY-EXPOSED BIN. Strictly shallower is
			// the whole test: deeper slots in the same lane are behind it and
			// cannot bury it, so they stay usable. See protectedDepth above.
			if d, guarded := protectedDepth[c.ID]; guarded && slot.Depth != nil && *slot.Depth < d {
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
