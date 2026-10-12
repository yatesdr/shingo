package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/dispatch/binresolver"
	"shingocore/dispatch/plan"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// lane_gate_choose.go — C2c (Lane C): a gate-staged store's drop is chosen
// across its group at release, by the one chooser.
//
// The rule (owner, 2026-10-08 §2): the plan targets the group; the oracle
// chooses the drop across the whole node group at every release, by the
// group's configured store algorithm, hard rules first and no scoring. The
// orc's rulings in advance bound it (BRIEF-unification-C2c, Lane C):
//
//	1  one spelling — plan.Planner.ChooseDrop — at acquiring and at release
//	2  between plan and release the drop holds paper (a pending slot row tagged
//	   groupRoom, D13 F1), taken by rank or yielding to a committed robot
//	3  at release the hand takes the chosen slot as armor, and a refused mouth is
//	   the next candidate, not a wait, while the group has one
//	6  a relay is not re-aimed; a re-aimed mid-trip drop gates its later entries
//
// D13 Q-NAMED keeps a drop with no group stamp lane-scoped: the same chooser,
// asked of its own lane only.
//
// THIS REPLACED THREE CHOOSERS (DESIGN-C2c §1 #10-#12): the in-lane re-bind
// (rebindGatedDropoff, which asked FindStoreSlotInLaneExcluding and skipped the
// payload, carrier, empties-only and level fences: §0 finding 4), the widen
// (widenDropoffToGroup, which asked with the order's payload and no carrier —
// a carried empty asked as the full, §0.5(a) — and did not set aside the lane
// it was widening off, so a group whose like-kind answer was the walled lane
// left the robot standing beside a sibling with room, §0.5(b)), and the refused
// widen (widenRefusedDropoff / refusalCanWiden, which re-aimed only three named
// refusal causes). Pinned: TestC2cPin_OneChooserNoWidenArm,
// TestWiden_ACarriedEmptyIsAskedAsAnEmpty, TestWiden_AWidenNeverAnswersTheLaneItWalled.

// gatedGroupDrop reports whether the drop at step i is a group drop the gate
// chooses at release (C2c rulings 2 and 3): stamped with its group, standing in
// a LANE of it (a laned group is always marked, nodes.UnmarkedLanes, so it has
// a waiting spot), not a relay (ruling 6), and not a no-demand order (which the
// widen and redirectStoreOffDugLane both left alone). The hand leaves its slot
// as paper (handFromComplexPlan) and the release asks the whole group. Every
// other drop keeps today's moment: a flat group's (ruling 5), a person-named
// slot's (D13 Q-NAMED) and a relay's.
//
// slot is the drop's current node; a slot whose parent is the group itself is a
// flat child.
func gatedGroupDrop(order *orders.Order, steps []resolvedStep, i int, slot *nodes.Node) bool {
	if i < 0 || i >= len(steps) || steps[i].Action != protocol.ActionDropoff || steps[i].Group == "" {
		return false
	}
	if aPersonNamedTheDrop(order) {
		return false
	}
	if slot == nil || slot.ParentName == "" || slot.ParentName == steps[i].Group {
		return false
	}
	return !relaysInPlan(steps, i)
}

// aPersonNamedTheDrop reports whether order's drop is one a person named: a
// no-demand order (a bin move, a spot order, a redirect — a door with a person
// at it). Lane-scoped, unchanged (RULINGS-C2c-run-through §3.2: "a slot a
// person named (no-demand / person-named)"): Core does not re-choose what
// somebody chose. The one spelling of the exception, for the complex step's
// group drop (gatedGroupDrop) and the plain one (plainGroupDrop).
func aPersonNamedTheDrop(order *orders.Order) bool {
	return order != nil && order.OriginClass == protocol.OriginClassNoDemand
}

// plainGroupDrop reports whether a PLAIN order's drop onto a slot of lane is a
// group drop (RULINGS-C2c-run-through §3.2, the owner at 19:57: "why wouldnt
// they all pick across a group?"): the slot stands in a LANE of a group, Core
// chose it — no person named it — and the order is nobody's leg (a dig leg's
// parking is the dig's own walk, D13 Q-SCOPE).
//
// WHERE THE PLAIN PATH KEEPS THE GROUP IT CHOSE FROM. A plain order has no plan
// until its create, and its delivery_node names the slot, so the group is the
// one the slot stands in: lane.ParentName, read off the lane row the storage
// question already reads (storageDropoffOf). It is written down twice, each in
// a write the path already made: the slot row's tag (claimStoreSlot reserves it
// as reservations.GroupRoomTag, the group's paper, in the INSERT it always
// made) and the drop step's Group in the plan the gated create persists
// (dispatchToFleetCore, in the UPDATE buildGatedCreate always made).
func plainGroupDrop(order *orders.Order, lane *nodes.Node) bool {
	return lane != nil && lane.NodeTypeCode == protocol.NodeClassLANE && lane.ParentName != "" &&
		order != nil && order.ParentOrderID == nil && !order.Coordinated && !aPersonNamedTheDrop(order)
}

// GroupIDForLane is the group whose gate a change in the lane concerns (C2c
// ruling 4), or 0. For the engine's lane-gate subscribers.
func (d *Dispatcher) GroupIDForLane(laneID int64) int64 { return d.groupOfLane(laneID) }

// GateKeysForNode is the lane a node stands in (0 when it is not a lane slot)
// and the group whose gate a change at it concerns: the lane's parent for a
// lane slot, the node's own parent for a flat child of a group (groupOfNode's
// walk), and for a lane named as a node, its parent. One LaneForNode read for a
// lane slot.
func (d *Dispatcher) GateKeysForNode(nodeID int64) (laneID, groupID int64) {
	if nodeID == 0 {
		return 0, 0
	}
	if lane, err := d.db.LaneForNode(nodeID); err == nil && lane != nil {
		return lane.ID, groupIDOf(lane)
	}
	return 0, d.groupOfNode(nodeID)
}

// takeGroupRoomByRank takes slot from the stranger whose groupRoom paper is on
// it, for order at acquiring, when order's demand outranks the holder's and the
// holder was never sent (C2c ruling 2: "taken by rank"; DESIGN-C2c §7.2
// generalises the dig leg's door to any outranking chooser). The rank is
// re-checked under lock, the holder's row deleted, its wait written naming the
// taker (slot-taken), the slot reserved as this order's paper — one
// transaction (store.TakeGroupRoomByRank). Reports whether the slot is now held.
func (d *Dispatcher) takeGroupRoomByRank(order *orders.Order, slot *nodes.Node) bool {
	named := order.ID
	if order.ParentOrderID != nil {
		named = *order.ParentOrderID
	}
	note := store.HolderNote{
		Reason: FormatQueueSentence(protocol.QueueWaitingForSlot, QueueParams{TakenByOrderID: named}),
		Code:   protocol.QueueWaitingForSlot,
		Cause:  string(CauseSlotTaken),
		Actor:  "dispatcher",
	}
	holder, err := d.db.TakeGroupRoomByRank(slot.ID, order.ID, named, note)
	if err != nil {
		if !errors.Is(err, store.ErrNoSlotTake) {
			log.Printf("dispatch: order %d could not take %s by rank: %v", order.ID, slot.Name, err)
		}
		return false
	}
	log.Printf("dispatch: order %d took %s from order %d's paper by rank — its demand outranks the holder's "+
		"never-sent plan; order %d is told and re-plans on its next pass", order.ID, slot.Name, holder, holder)
	if h, gErr := d.db.GetOrder(holder); gErr == nil && h != nil {
		d.NotifyWaitChanged(h)
	}
	return true
}

// groupIDOf is the group a lane stands in, read off the row in hand: the key a
// wait on that lane is stamped with (WaitGroup).
func groupIDOf(lane *nodes.Node) int64 {
	if lane == nil || lane.ParentID == nil {
		return 0
	}
	return *lane.ParentID
}

// relaysInPlan reports whether a step after i picks up from the very node the
// drop at i names: a relay (ruling 6). Re-aiming one half would send the robot
// to fetch from a slot it never filled, and no paired-index writer moves both.
func relaysInPlan(steps []resolvedStep, i int) bool {
	if i < 0 || i >= len(steps) {
		return true // unreadable answers yes, the side that does not re-aim
	}
	at := steps[i].Node
	for j := i + 1; j < len(steps); j++ {
		if steps[j].Action == protocol.ActionPickup && steps[j].Node == at {
			return true
		}
	}
	return false
}

// gateStagedForGroup returns every order dwelling at one of THIS group's lane
// waits (C2c ruling 4), and the group's lanes by id.
//
// A wait belongs to the group its WaitGroup names, or — for a plan written
// before that field — to the parent of its WaitLane (DESIGN-C2c §7.8, the one
// reader rule). The group's lanes are read once, and only when somebody is
// waiting at all; each candidate is then built exactly as the per-lane
// derivation builds it (gateCandidateAt), against the lane its wait names.
func (d *Dispatcher) gateStagedForGroup(groupID int64) ([]gateCandidate, map[int64]*nodes.Node, error) {
	active, err := d.db.ActiveGateCandidates()
	if err != nil {
		return nil, nil, err
	}
	type waiting struct {
		c     *orders.GateCandidate
		steps []resolvedStep
		w     resolvedStep
	}
	var ws []waiting
	for _, c := range active {
		steps, w, ok := waitingAtLane(c)
		if !ok || (w.WaitGroup != 0 && w.WaitGroup != groupID) {
			continue
		}
		ws = append(ws, waiting{c: c, steps: steps, w: w})
	}
	if len(ws) == 0 {
		return nil, nil, nil
	}
	kids, err := d.db.ListChildNodes(groupID)
	if err != nil {
		return nil, nil, err
	}
	lanes := make(map[int64]*nodes.Node, len(kids))
	for _, k := range kids {
		if k != nil && k.NodeTypeCode == protocol.NodeClassLANE {
			lanes[k.ID] = k
		}
	}
	var out []gateCandidate
	for _, x := range ws {
		lane := lanes[x.w.WaitLane]
		if lane == nil {
			continue // waiting at somebody else's group
		}
		cand, ok, cErr := d.gateCandidateAt(x.c, x.steps, x.w, lane)
		if cErr != nil {
			return nil, nil, cErr
		}
		if ok {
			out = append(out, cand)
		}
	}
	return out, lanes, nil
}

// releaseStoreCandidate is a gate-staged STORE's release, at all three moments
// (C2c ruling 1: "called at acquiring and at release"; DESIGN-C2c §7.3): the
// group pass, the valve at create (P0's premise correction 2: the valve is a
// release and calls the same chooser) and the own-dig resume.
//
//  1. ChooseDrop over the group: the hard rules, then the group's algorithm,
//     with the lanes of the plan's later pickups set aside. A committed robot,
//     so a never-sent plan's paper is offered to it (ruling 2).
//  2. The chosen lane's admission (gateEntryVerdict: the physical questions,
//     then the Tier 1/2 ordering — lane facts asked after the choice).
//  3. A refusal sets that lane aside and asks again (ruling 3); the group
//     running out is a wait, under the FIRST refusal's cause, which names the
//     lane the group preferred.
//  4. Admitted: the release hand (releaseChosenDrop), then the tail.
//
// ownLane is a lane admitted without asking: the lane the order's own dig just
// opened for it (releaseAfterOwnDig: "the dig having opened the lane for this
// order is the admission"). 0 asks every lane. propose is the pass's heal
// prompt, nil where no heal is decided (the valve, the resume). reads is the
// pass's one read of the group (store.GroupPassReads, RULINGS-C2c-run-through
// §3.3): the chooser asks through it, and the release writes its take into it,
// so the next waiter is answered with no statement; nil asks the store.
//
// It reports whether the order went in, the lane its append left it waiting at
// (0 for none), and whether the release failed after an admission.
func (d *Dispatcher) releaseStoreCandidate(c gateCandidate, group *nodes.Node, lanes map[int64]*nodes.Node,
	propose func(gateCandidate, *nodes.Node, QueueCause), ownLane int64, reads *store.GroupPassReads,
	what string) (in bool, next int64, failed bool) {
	chooser := d.pure
	if reads != nil {
		chooser = d.pure.Over(reads)
	}
	// THE DOUBLE-APPEND GUARD, FIRST. Called only under the group's mutex, and it
	// re-reads the row and re-checks IsGateStaged: wait_index is the durable
	// witness (the append advances it only after the fleet took the segment), so a
	// pass that raced this one reloads, sees it moved, and touches nothing.
	fresh, err := d.db.GetOrder(c.order.ID)
	if err != nil || fresh == nil {
		return false, 0, false
	}
	if !IsGateStaged(fresh) {
		d.dbg("lane gate: order %d no longer gate-staged (wait_index=%d status=%s) — another pass released it",
			fresh.ID, fresh.WaitIndex, fresh.Status)
		return false, 0, false
	}
	var steps []resolvedStep
	if uErr := json.Unmarshal([]byte(fresh.StepsJSON), &steps); uErr != nil ||
		c.entryIndex < 0 || c.entryIndex >= len(steps) || steps[c.entryIndex].Action != protocol.ActionDropoff {
		log.Printf("lane gate: order %d's step %d is not the drop its gate speaks for (%v)", fresh.ID, c.entryIndex, uErr)
		d.setQueueReason(fresh, protocol.QueueWaitingForSlot, CauseGateReleaseFailed, QueueParams{Lane: c.lane.Name})
		return false, 0, true
	}
	need := d.dropNeedFor(fresh, steps, c)
	// ONE ASK, WALKED IN ORDER (DESIGN-C2c §7.3: the group's slot state read once
	// per release). The chooser answers the group's whole order; a lane that
	// refuses sends the drop to the next answer without a second read.
	need.Ranked = need.Lane == nil
	choice, cErr := chooser.ChooseDrop(group, need)
	if cErr != nil {
		return d.waitWithNoCandidate(fresh, c, group, cErr, propose), 0, false
	}
	var (
		first     *GateVerdict
		firstLane *nodes.Node
		laneSeen  bool
	)
	for _, ch := range append([]plan.DropChoice{choice}, choice.Alternatives...) {
		lane := lanes[ch.LaneID]
		if lane == nil && ch.LaneID != 0 {
			lane, _ = d.db.GetNode(ch.LaneID)
			if lane == nil {
				continue // a lane that could not be read is no candidate
			}
		}
		laneSeen = true
		if lane == nil {
			// A FLAT CHILD OF THE GROUP IS A CANDIDATE LIKE ANY OTHER
			// (RULINGS-C2c-run-through §3.2: "the chooser considers every child of
			// the group"). It used to be passed over because the robot stands at a
			// lane's wait. That wait is the group's spot (WaitGroup), and a flat
			// child is a place beside the lanes, not in one: there is no corridor
			// to admit it to and no lane to occupy, so the release takes the child
			// as armor and appends the tail with no lane question.
			return d.releaseChosenDrop(fresh, c, steps, nil, ch, reads, what)
		}
		if lane.ID != ownLane {
			v, vErr := d.gateEntryVerdict(lane, fresh, ch.Slot, false)
			if vErr != nil {
				// CauseAdmissionError, not a lane cause: an undetermined answer is
				// Core declining. No heal on no evidence.
				log.Printf("lane gate: classifier error for order %d on lane %s: %v", fresh.ID, lane.Name, vErr)
				d.setQueueReason(fresh, protocol.QueueWaitingForSlot, CauseAdmissionError,
					QueueParams{Lane: lane.Name})
				return false, 0, false
			}
			if !v.Admitted() {
				if need.Lane != nil {
					// ASKED OF ITS OWN LANE ONLY (Q-NAMED, a relay): there is no next
					// candidate, so the refusal is the wait, and the heal reads the
					// physics on it, as the per-lane evaluator always did.
					d.recordLaneWait(fresh, lane, v)
					d.dbg("lane gate: order %d still held at %s (%s)", fresh.ID, lane.Name, v.Cause())
					if propose != nil {
						propose(c, lane, v.Cause())
					}
					return false, 0, false
				}
				if first == nil {
					vv := v
					first, firstLane = &vv, lane
				}
				// A REFUSED MOUTH IS THE NEXT CANDIDATE, NOT A WAIT (ruling 3).
				d.dbg("lane gate: order %d refused at %s (%s) — the group's next answer",
					fresh.ID, lane.Name, v.Cause())
				continue
			}
		}
		return d.releaseChosenDrop(fresh, c, steps, lane, ch, reads, what)
	}
	if first != nil {
		// THE GROUP RAN OUT AFTER A REFUSAL: a wait, and the row says why in the
		// words of the lane the group preferred (F2, F5's blocker).
		d.recordLaneWait(fresh, firstLane, *first)
		d.dbg("lane gate: order %d still held at group %s (%s at %s; no other lane can take it)",
			fresh.ID, group.Name, first.Cause(), firstLane.Name)
		return false, 0, false
	}
	if !laneSeen {
		cErr = fmt.Errorf("no available slot in node group %s: no lane of it offers one", group.Name)
	}
	return d.waitWithNoCandidate(fresh, c, group, cErr, propose), 0, false
}

// waitWithNoCandidate is a store's wait when its group offered it nothing at
// all. Heal first holds here, and only here (D13 F3): a wall in front of the
// pencilled slot is a reason to dig when no lane of the group can take the drop.
//
// THE ROW SAYS WHY IN THE PENCILLED LANE'S WORDS when that lane refuses the
// order outright (a dig holds it, a robot is in it), and says Core could not
// tell when it could not read it — the per-lane evaluator asked that lane's
// admission first and wrote either. Otherwise the lane is simply full:
// gate-rebind-unavailable, as the re-bind wrote. It reports false: not released.
func (d *Dispatcher) waitWithNoCandidate(order *orders.Order, c gateCandidate, group *nodes.Node, cErr error,
	propose func(gateCandidate, *nodes.Node, QueueCause)) bool {
	d.dbg("lane gate: order %d has nowhere in group %s: %v", order.ID, group.Name, cErr)
	cause := causeForGateRebind(cErr)
	if v, vErr := d.gateEntryVerdict(c.lane, order, c.node, false); vErr != nil {
		log.Printf("lane gate: classifier error for order %d on lane %s: %v", order.ID, c.lane.Name, vErr)
		d.setQueueReason(order, protocol.QueueWaitingForSlot, CauseAdmissionError, QueueParams{Lane: c.lane.Name})
		return false // no heal on no evidence
	} else if !v.Admitted() {
		d.recordLaneWait(order, c.lane, v)
		cause = v.Cause()
	} else {
		d.setQueueReason(order, protocol.QueueWaitingForSlot, cause, QueueParams{Destination: group.Name})
	}
	if propose != nil {
		propose(c, c.lane, cause)
	}
	return false
}

// dropNeedFor is what a gate-staged drop asks the chooser with: the drop's own
// payload (none for a carried empty — C2b Q4, the leak §0.5(a) found at
// release), the carried bin's type, the asker, the plan's later pickups, and
// that it is a committed robot. A drop the gate does not choose across the group
// is asked of its own lane only.
func (d *Dispatcher) dropNeedFor(order *orders.Order, steps []resolvedStep, c gateCandidate) plan.DropNeed {
	i := c.entryIndex
	need := plan.DropNeed{
		Payload: plan.DropPayload(i, steps[i].PayloadCode, order.PayloadCode,
			func(j int) bool { return steps[j].Action == protocol.ActionPickup },
			func(j int) bool { return steps[j].Action == protocol.ActionPickup && steps[j].Empty }),
		Stated:       d.carriedType(order, steps, i),
		Asker:        digAskerFor(order),
		LaterPickups: plan.LaterPickups(steps, i),
		Refused:      map[int64]bool{},
		Committed:    true,
	}
	// D13 Q-NAMED: "every release" is read as group-stamped drops. A drop with no
	// stamp — a complex step that named its slot, and every PLAIN store, whose
	// plan is built from the order's endpoints and carries none — is asked of its
	// own lane only, as the in-lane re-bind asked it.
	if !gatedGroupDrop(order, steps, i, c.node) {
		need.Lane = c.lane
	}
	return need
}

// carriedType is the type of the bin the robot carries into the drop at step i:
// the bin of the nearest pickup ahead of it (binForStep, the junction first). At
// release the pickup's node no longer holds it, so the plan-time reading
// (BinTypeBeforeStep, the bin standing at the pickup) would read the wrong node.
// Unknown leaves the resolver to settle it from the order, as it always has.
func (d *Dispatcher) carriedType(order *orders.Order, steps []resolvedStep, i int) binresolver.BinTypeStatement {
	for j := i - 1; j >= 0; j-- {
		if steps[j].Action != protocol.ActionPickup {
			continue
		}
		b, err := d.binForStep(order, steps, j)
		if err == nil && b.known && b.binID != 0 {
			if bin, gErr := d.db.GetBin(b.binID); gErr == nil && bin != nil {
				return binresolver.KnownBinType(bin.BinTypeID)
			}
		}
		break
	}
	return binresolver.UnknownBinType("lane-gate release: the carried bin is not known")
}

// releaseChosenDrop is the release hand and the tail (C2c ruling 3): the chosen
// slot taken as armor in one transaction (DB.ConfirmReleaseDrop: the lane's
// lock, a stranger's paper taken when the chooser offered it, the order's paper
// moved and confirmed, the old slot let go), the plan patched to the choice,
// then the tail appended.
//
// A drop that is already armor on the chosen slot (a person-named slot the hand
// confirmed at dispatch, still the right one) writes nothing before the append.
func (d *Dispatcher) releaseChosenDrop(order *orders.Order, c gateCandidate, steps []resolvedStep, lane *nodes.Node,
	choice plan.DropChoice, reads *store.GroupPassReads, what string) (in bool, next int64, failed bool) {
	current := c.node
	moved := current == nil || current.ID != choice.Slot.ID
	// A FLAT CHILD has no lane (lane is nil): no lane lock, no corridor, no
	// occupancy. While it waits the row names the lane its wait names, as it did.
	var laneID int64
	into, waitsAt := "flat child "+choice.Slot.Name, c.lane
	if lane != nil {
		laneID, into, waitsAt = lane.ID, "lane "+lane.Name, lane
	}
	armored := !moved && current.ClaimedBy != nil && *current.ClaimedBy == order.ID
	// took says this release made the slot armor, which is what a release whose
	// tail never reached the fleet gives back (giveBackRelease).
	took := false
	if !armored || choice.Take != nil {
		var take *store.SlotTake
		if choice.Take != nil {
			named := order.ID
			if order.ParentOrderID != nil {
				named = *order.ParentOrderID
			}
			mode := store.SlotTakeByRank
			if choice.Take.ByCommitment {
				mode = store.SlotTakeByCommitment
			}
			take = &store.SlotTake{Mode: mode, Named: named, Note: store.HolderNote{
				Reason: FormatQueueSentence(protocol.QueueWaitingForSlot, QueueParams{TakenByOrderID: named}),
				Code:   protocol.QueueWaitingForSlot,
				Cause:  string(CauseSlotTaken),
				Actor:  "dispatcher",
			}}
		}
		var leave int64
		if moved && current != nil {
			leave = current.ID
		}
		holder, err := d.db.ConfirmReleaseDrop(laneID, choice.Slot.ID, order.ID, leave, take)
		if err != nil {
			// THE CAUSE IS DETERMINED AT THE REFUSAL, beside the error (the shape
			// TestRefusalClassifiersAreNotReimplementedScan keeps). Nothing was
			// taken: the hand rolled back, and the order keeps the paper it had.
			log.Printf("lane gate: order %d chose %s in %s but its release hand was refused: %v",
				order.ID, choice.Slot.Name, into, err)
			d.setQueueReason(order, protocol.QueueWaitingForSlot, CauseGateReleaseFailed, QueueParams{Lane: waitsAt.Name})
			return false, 0, true
		}
		if holder != 0 {
			how := "outranks its plan"
			if choice.Take.ByCommitment {
				how = "is a committed robot and its plan was never sent"
			}
			log.Printf("lane gate: order %d took %s from order %d's paper — it %s; order %d is told and "+
				"re-plans on its next pass", order.ID, choice.Slot.Name, holder, how, holder)
			if h, gErr := d.db.GetOrder(holder); gErr == nil && h != nil {
				d.NotifyWaitChanged(h)
			}
		}
		took = true
		reads.NoteReleased(order.ID, choice.Slot.ID, leave, entryIsFinalIn(steps, c.entryIndex))
		d.stampBound(order, choice.Slot)
	}
	if moved {
		if err := d.patchChosenDrop(order, c, steps, lane, choice.Slot); err != nil {
			log.Printf("lane gate: order %d took %s but its plan could not follow: %v", order.ID, choice.Slot.Name, err)
			d.giveBackRelease(order, choice.Slot, took, reads)
			d.setQueueReason(order, protocol.QueueWaitingForSlot, CauseGateReleaseFailed, QueueParams{Lane: waitsAt.Name})
			return false, 0, true
		}
		log.Printf("lane gate: order %d bound at release %s → %s (%s)",
			order.ID, nodeName(current), choice.Slot.Name, into)
	}
	if err := d.appendGateTail(order, what); err != nil {
		if errors.Is(err, ErrAppendNotOwed) {
			return false, 0, false
		}
		// The release-failed cause first, then the debounce: past its threshold
		// the wait becomes operator-visible as a fleet wait (noteGateAppendFailure),
		// and that sentence must be the one left on the row.
		log.Printf("lane gate: release order %d into %s: %v", order.ID, into, err)
		d.setQueueReason(order, protocol.QueueWaitingForSlot, CauseGateReleaseFailed, QueueParams{Lane: waitsAt.Name})
		if !IsAppendLanded(err) {
			// THE ROBOT IS STILL AT THE SPOT: the occupancy was refused, or the
			// fleet never took the segment. The slot this release took goes back
			// to paper, so the order waits holding what it held before.
			d.giveBackRelease(order, choice.Slot, took, reads)
		}
		d.noteGateAppendFailure(order, waitsAt)
		return false, 0, true
	}
	d.clearGateAppendFailures(order.ID)
	// CLEARED ON ENTRY: the cause described a wait that is over.
	d.setQueueReason(order, "", "", QueueParams{})
	d.dbg("lane gate: order %d released into %s at %s", order.ID, into, choice.Slot.Name)
	return true, laneWaitAfterAppend(order), false
}

// giveBackRelease is the occupancy pin's answer (RULINGS-C2c-run-through §3.2):
// lane occupancy is taken at the tail append, after the release transaction
// made the chosen slot armor, so an append refused there — the occupancy row
// refused, or the fleet never taking the segment — leaves a robot at the
// group's spot holding armor on a slot nothing is driving to. The order does
// not strand either way (it is still gate-staged, under gate-release-failed,
// and the next pass or the floor releases it), but it must wait holding what it
// held before the release: the slot goes back to paper (DB.GiveBackReleaseDrop).
// took is whether this release made the armor; a drop armored before it (a
// person-named slot the hand confirmed at dispatch) keeps its armor.
// Pinned: TestC2cPin_ARefusedOccupancyAtTheAppendGivesTheSlotBack.
func (d *Dispatcher) giveBackRelease(order *orders.Order, slot *nodes.Node, took bool, reads *store.GroupPassReads) {
	if !took {
		return
	}
	if err := d.db.GiveBackReleaseDrop(slot.ID, order.ID); err != nil {
		log.Printf("lane gate: order %d's release took %s and its tail never left, but the slot could not be "+
			"given back: %v", order.ID, slot.Name, err)
		return
	}
	reads.NoteGivenBack(order.ID, slot.ID)
	d.dbg("lane gate: order %d gives %s back as paper — its tail never reached the fleet", order.ID, slot.Name)
}

// patchChosenDrop moves the plan to the chosen slot. Three copies of "where is
// this going", all or none (they were the widen's):
//
//	the entry step (+ delivery_node    what the append emits, and what the row
//	  when it is the plan's last drop)  says; an interior drop moves in the plan
//	                                    alone (C2a A.5: delivery_node is the trip's end)
//	the WAIT step's WaitLane           whose occupancy the append takes — a stale
//	                                    one takes the wrong lane's (F-12's shape)
//	order_bins.dest_node                what the whole-order settle places against
//
// A mid-trip drop that changed lane leaves the steps after it entering a lane the
// robot is not in, so each such entry gets its own wait (gateLaterLaneEntries,
// ruling 6), and the waits are checked again (assertEachWaitGatesItsEntry): a
// guard that runs only where a value is born stops guarding once it can change.
func (d *Dispatcher) patchChosenDrop(order *orders.Order, c gateCandidate, steps []resolvedStep, lane, slot *nodes.Node) error {
	final := entryIsFinalIn(steps, c.entryIndex)
	if final {
		if err := applyDeliveryNodeAtStep(d.db, order, slot.Name, c.entryIndex); err != nil {
			return err
		}
	} else {
		applyPlanNode(d.db, order, slot.Name, c.entryIndex, protocol.ActionDropoff)
	}
	// A FLAT CHILD (lane nil) is in no lane: the wait it is released from names
	// no lane after it (WaitLane 0) and keeps its group, so the tail takes no
	// occupancy and the plan's check reads the child as its group's.
	var laneID int64
	if lane != nil {
		laneID = lane.ID
	}
	if laneID != c.lane.ID {
		if err := d.rebindWaitLane(order, laneID, groupIDOf(c.lane)); err != nil {
			return err
		}
		if !final {
			if err := d.gateLaterLaneEntries(order, c.entryIndex, laneID); err != nil {
				return err
			}
		}
		var patched []resolvedStep
		if err := json.Unmarshal([]byte(order.StepsJSON), &patched); err != nil {
			return fmt.Errorf("re-read the patched plan: %w", err)
		}
		if err := d.assertEachWaitGatesItsEntry(patched); err != nil {
			return fmt.Errorf("the plan no longer holds after moving to %s: %w", slot.Name, err)
		}
	}
	// After the step patch, not before: the derivation reads the plan.
	d.refreshOrderBinDestinations(order)
	return nil
}

// entryIsFinalIn reports whether the step at i is the plan's last actionable one.
func entryIsFinalIn(steps []resolvedStep, i int) bool {
	if i < 0 || i >= len(steps) || steps[i].Action != protocol.ActionDropoff {
		return false
	}
	for j := i + 1; j < len(steps); j++ {
		if steps[j].Action == protocol.ActionDropoff || steps[j].Action == protocol.ActionPickup {
			return false
		}
	}
	return true
}
