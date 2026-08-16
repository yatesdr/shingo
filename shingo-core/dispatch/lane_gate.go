package dispatch

import (
	"errors"
	"fmt"
	"log"

	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// LaneEnforcementMode is the per-lane-group enforcement choice — the §15 methods
// menu, picked per plant topology and stored as a node property on the group
// (NGRP). Default is none: the Core mouth gate is off and behavior is identical
// to today.
type LaneEnforcementMode string

const (
	// LaneEnforceNone — no Core lane gate for this group (the default). Byte-
	// identical to pre-seam behavior.
	LaneEnforceNone LaneEnforcementMode = "none"
	// LaneEnforceMouth — the mode-aware mouth gate (§2): shared storage lanes that
	// need same-kind concurrency; a conflicting order waits, visibly, in sourcing.
	LaneEnforceMouth LaneEnforcementMode = "mouth"
	// `delegated` WAS A MODE HERE AND WAS DELETED. It meant "an RDS mutex zone owns
	// the physical mouth (B4), so Core takes no mouth hold" — and it had zero
	// production references beyond the switch arm that returned it.
	//
	// It is not being deleted for contradicting the authority ruling. The
	// contradiction dissolved underneath it: the physical questions (foreign dig,
	// occupancy, reachability) are now asked on BOTH entry paths regardless of
	// mode, and occupancy rows are written with no mode check at all — so "Core
	// still tracks" is unconditionally true and the re-scope it wanted had nothing
	// left to say. What remained was only "Core takes no mouth hold", which no
	// plant asked for and nothing could express.
	//
	// NOTHING TO MIGRATE. `lane_enforcement` is not set on any node at either
	// plant — verified live 2026-08-08, both cores: zero rows for this key, and
	// this branch has never been deployed to a plant anyway. So there is no
	// group, anywhere, carrying this value. The deletion is safe because the
	// configuration does not exist, not because a fallback rescues it.
	//
	// RE-INTRODUCE IT against a real RDS mutex zone AND a real config surface, not
	// before. It was argued about for five review rounds while being unreachable
	// by any deliberate means.
	// LaneEnforceGateChoreography — Core is the traffic cop: a lane-bound order
	// ships UNSEALED ending at the lane's wait point, and Core appends its tail
	// when the lane is safe. Same substrate as mouth (mouth rows, §4 release,
	// depth priority) and the SAME classifier; only the disposition of a park
	// verdict differs — mouth parks the order pre-dispatch, choreography stages
	// the robot at the gate. Introduced inert: until the staging path lands, a
	// group set to this value behaves exactly like `mouth`.
	LaneEnforceGateChoreography LaneEnforcementMode = "gate_choreography"
)

// PropLaneEnforcement is the node-property key (read on the lane's group) that
// selects the enforcement mode.
const PropLaneEnforcement = "lane_enforcement"

// laneGateReservedBy tags the mouth rows the gate writes, for forensics.
const laneGateReservedBy = "lanegate"

// THE HARDCODED LANE PRIORITY BOOST WAS DELETED HERE.
//
// It was `laneShareBasePriority = 30`, added to a lane-bound move's target slot
// depth and written into CreateOrderRequest.Priority, so co-admitted stores would
// enter a single-file lane deepest-first. RDS serves the LARGEST priority first.
//
// Why it went: it is left over from an earlier attempt at this, and priority is
// latent ability rather than something the system plans around. Lane entries are
// first-come-first-serve until they are not.
//
// Why deleting it is safe, stated without inventing a migration story: the value
// was only ever applied when the group's lane_enforcement selected it, and that
// property is set on no node at either plant (verified live 2026-08-08, zero rows
// both cores) — nor has this branch ever been deployed to one. The number never
// reached a fleet. The configuration that would have activated it does not exist.
//
// THE PRIORITY MECHANISM IS DELIBERATELY UNTOUCHED. CreateOrderRequest.Priority,
// the order's own priority column, and every path that carries a priority remain
// exactly as they were — that is dormant capability, not dead code. What was
// removed is Core INVENTING a number for lane-bound moves.
//
// Worth knowing when priority does start mattering — pre-clearing blockers ahead
// of a predicted changeover is the shape to expect: this boost wrote into
// the SAME field an operator boost uses, and under larger-wins a base of 30
// silently outranked the RDS team's conventional 10. That collision is why it was
// raised. It is now gone, and the field is clean for whoever needs it next.

// laneEnforcementMode reads the enforcement mode configured on a lane group
// (NGRP). Any unset or unrecognized value is none — off, byte-identical to today.
func (d *Dispatcher) laneEnforcementMode(groupID int64) LaneEnforcementMode {
	switch LaneEnforcementMode(d.db.GetNodeProperty(groupID, PropLaneEnforcement)) {
	case LaneEnforceMouth:
		return LaneEnforceMouth
	case LaneEnforceGateChoreography:
		return LaneEnforceGateChoreography
	default:
		// Unset, unrecognized, and the retired `delegated` all land here — the
		// same ordinary unrecognized-value handling this switch has always done
		// for any junk string. `delegated` simply joins that set; it is not a
		// migration arm, because no node anywhere has the property set.
		return LaneEnforceNone
	}
}

// ── TWO QUESTIONS, TWO PREDICATES ──────────────────────────────────────────
//
// These replace `laneGateActive`, which was ONE boolean answering THREE
// questions at three call sites. That was the right fix for the bug it was
// created for — the sites used to spell the test `!= LaneEnforceMouth`, so a new
// arm silently inherited `none` behaviour on a plant that had configured a gate,
// invisibly: nothing errors, orders just stop being sequenced. Routing them
// through one predicate stopped that.
//
// But it bought correctness by making the questions UNASKABLE separately, and
// they were not the same question:
//
//   - mouth holds       — a REFUSAL. Its effect is a wait, and waiting is how
//                         this system already declines work.
//   - entry sequencing  — an ORDERING among work that is already admitted.
//   - depth priority    — a WRITE TO THE FLEET. This one is now DELETED (see the
//                         boost note above), which is the cleanest possible
//                         resolution of it: the odd one out was the one that did
//                         not belong.
//
// The remaining two are both decisions INSIDE Core, and nothing here reaches the
// fleet. They are kept separate rather than re-fused because the geometry
// derivation may still want to answer them independently, and re-merging them
// would rebuild the exact trap the original predicate existed to close.
//
// Splitting changed NOTHING behaviourally — both return the same value for every
// mode — and that remains deliberate.
//
// What an ACTIVE mode still chooses for itself is the DISPOSITION of a classifier
// park verdict (mouth: park pre-dispatch; gate_choreography: stage at the wait
// point). That branch belongs in admitLaneEntry, never here.

// takesMouthHold reports whether Core takes mouth holds for this group — the
// inbound/outbound mode-sharing rows that make a conflicting order wait.
func takesMouthHold(m LaneEnforcementMode) bool {
	return m == LaneEnforceMouth || m == LaneEnforceGateChoreography
}

// sequencesLaneEntry reports whether the tiered lane-entry classifier applies to
// this group — ordering among already-admitted work, not admission itself.
func sequencesLaneEntry(m LaneEnforcementMode) bool {
	return m == LaneEnforceMouth || m == LaneEnforceGateChoreography
}

// setsDepthPriority and laneDispatchPriority were DELETED with the boost above.
// setsDepthPriority existed only to gate it, so the three-way split of
// laneGateActive is now a two-way one — see the block above takesMouthHold.

// laneHold is a (lane, mode) an order must hold to work that lane: outbound when
// it picks from the lane, inbound when it drops into it.
type laneHold struct {
	laneID int64
	mode   reservations.Mode
}

// resolveOrderLaneHolds computes the mouth holds a plain order needs from its
// source and destination nodes: the source lane is outbound (the order picks
// there), the destination lane is inbound (the order drops there). A node that is
// not a direct lane slot (LaneForNode == nil) contributes no hold, and only lanes
// whose group is configured for mouth enforcement are included — none/delegated
// groups contribute nothing, so an unconfigured plant yields zero holds (the gate
// is a no-op).
func (d *Dispatcher) resolveOrderLaneHolds(sourceNode, destNode *nodes.Node) ([]laneHold, error) {
	var holds []laneHold
	add := func(n *nodes.Node, mode reservations.Mode) error {
		if n == nil {
			return nil
		}
		lane, err := d.db.LaneForNode(n.ID)
		if err != nil {
			return err
		}
		if lane == nil || lane.ParentID == nil {
			return nil // not a lane slot, or a lane with no group — no hold
		}
		if !takesMouthHold(d.laneEnforcementMode(*lane.ParentID)) {
			return nil // Core does not own this group's mouth
		}
		holds = append(holds, laneHold{laneID: lane.ID, mode: mode})
		return nil
	}
	if err := add(sourceNode, reservations.ModeOutbound); err != nil {
		return nil, err
	}
	if err := add(destNode, reservations.ModeInbound); err != nil {
		return nil, err
	}
	return holds, nil
}

// acquireOrderLanes takes every hold the order needs, all-or-nothing across
// modes. It returns admitted=false on a mode conflict (the caller requeues the
// order in sourcing under WAITING_FOR_SLOT, per Rule 1); a non-nil error is a
// transient DB failure. With no holds (the common, unconfigured case) it admits
// immediately.
//
// AcquireLanes is per-mode all-or-nothing; an order picking from one lane and
// dropping into another needs one call per mode, so a conflict on the second
// mode rolls back the first via the order-scoped ReleaseLanesByOwner. All rows
// are owned by the order, so that release reclaims exactly this gate's takes.
func (d *Dispatcher) acquireOrderLanes(orderID int64, holds []laneHold) (admitted bool, err error) {
	if len(holds) == 0 {
		return true, nil
	}
	byMode := map[reservations.Mode][]int64{}
	for _, h := range holds {
		byMode[h.mode] = append(byMode[h.mode], h.laneID)
	}
	for mode, lanes := range byMode {
		if aErr := reservations.AcquireLanes(d.db.DB, orderID, mode, laneGateReservedBy, lanes...); aErr != nil {
			if errors.Is(aErr, reservations.ErrReservationConflict) {
				// Roll back any holds taken for an earlier mode so the acquire is
				// all-or-nothing across the order's lanes.
				_ = reservations.ReleaseLanesByOwner(d.db.DB, orderID)
				return false, nil
			}
			return false, aErr
		}
	}
	return true, nil
}

// releaseOrderLaneFor releases the order's mouth hold on the lane that contains
// node, if any — the early per-block handoff (§4): an outbound hold drops when
// the bin leaves the lane, an inbound hold drops when the bin lands. Owner-scoped
// and a no-op when no row exists, so it is byte-identical when the gate is off.
//
// A DIG CLAIM IS EXEMPT. This is a per-VISIT release and a dig's hold is
// per-RESHUFFLE: it has to outlive several legs, and the first leg's pickup is
// not the end of anything. Because laneOwnerFor routes a child's block progress
// to the compound parent, this used to arrive owned by the parent, match the
// parent's dig row, and delete it at leg one — leaving every later leg of the
// reshuffle with no durable claim on the lane at all.
//
// The exemption lives in ReleaseLaneHandoff's SQL rather than here, so there is
// no window between reading the mode and deleting the row. Everything else about
// the handoff is unchanged, which is the point: it is right for plain orders and
// was wrong only for digs.
func (d *Dispatcher) releaseOrderLaneFor(orderID int64, node *nodes.Node) error {
	if node == nil {
		return nil
	}
	lane, err := d.db.LaneForNode(node.ID)
	if err != nil {
		return err
	}
	if lane == nil {
		return nil
	}
	return reservations.ReleaseLaneHandoff(d.db.DB, orderID, lane.ID)
}

// TakeLaneOccupancy records that an order is INSIDE the lanes it was just
// dispatched into — Hold B, the second of the two lane holds.
//
// Called at dispatch rather than on any robot signal, and that is the design
// rather than a shortcut. Core cannot observe a robot entering a lane: every
// available signal is lagging, the earliest being a block completing AT a slot,
// which is already inside. It does not need to. Nothing enters a lane that Core
// did not send there, so Core's own dispatch decision IS the entry moment, and
// it is knowable exactly when it becomes true.
//
// Both endpoints are considered: a dig leg picks OUT of one lane and may place
// INTO another, and the robot is inside each while it is there. A node that is
// not a lane slot contributes nothing.
//
// NO LONGER ADVISORY. Step 6 removed the sibling-in-flight guard, and from that
// moment this row is the ONLY thing keeping two legs of one reshuffle out of one
// lane: admission reads it and holds the child. The doc that used to
// stand here — "it records; it does not arbitrate", true while the compound
// scheduler dispatched one child at a time — described a world that ended with
// that guard.
//
// So the error is returned rather than logged. The read side fails CLOSED (an
// unreadable lane is a busy lane, compound.go); a write side that logged and
// carried on meant a lane whose occupancy could not be RECORDED read as empty to
// the next leg — the same collision, arrived at from the other direction.
//
// Returns on the FIRST failure, deliberately leaving any rows already taken in
// place. A partial take is over-restrictive, never under: the extra row makes a
// lane look busier than it is, which costs a wait. Rolling it back would be the
// unsafe direction, and the caller holds the child rather than dispatching, so
// nothing is inside the lanes this reported.
func (d *Dispatcher) TakeLaneOccupancy(orderID int64, nodes ...*nodes.Node) error {
	for _, laneID := range d.lanesFor(nodes...) {
		if err := reservations.AcquireOccupancy(d.db.DB, orderID, laneID); err != nil {
			return fmt.Errorf("take occupancy for order %d on lane %d: %w", orderID, laneID, err)
		}
	}
	return nil
}

// takeLaneOccupancyByID is TakeLaneOccupancy for a caller that already knows the
// lane rather than a node in it — the gated append, whose lane comes off the
// wait step it is releasing (WaitLane) instead of off an endpoint column.
//
// Same semantics as the node-taking form: idempotent per (order, lane), and the
// error is RETURNED rather than logged, because a presence that could not be
// recorded reads as an empty lane to the next entrant.
func (d *Dispatcher) takeLaneOccupancyByID(orderID, laneID int64) error {
	if laneID == 0 {
		return nil // not a lane-owned wait — nothing to record
	}
	if err := reservations.AcquireOccupancy(d.db.DB, orderID, laneID); err != nil {
		return fmt.Errorf("take occupancy for order %d on lane %d: %w", orderID, laneID, err)
	}
	return nil
}

// ReleaseLaneOccupancy records that an order is out of every lane it occupied.
//
// Fired on DROPOFF completion, not on pickup. After a pickup the robot is
// holding the bin and still physically in the lane; it is out once it has placed
// at the destination. Releasing at pickup would declare the lane free with a
// robot still standing in it, which is the whole failure this hold exists to
// prevent.
//
// It releases ALL of the order's occupancy rather than the drop node's lane,
// because a dig leg's two endpoints are usually different lanes: it is the
// SOURCE lane the robot has finally left by placing elsewhere. "This leg has
// placed its bin and is out" is a statement about the leg, not about one node.
//
// Terminalization needs no separate arm: TerminalizeOrder's ReleaseByOrder is
// order-keyed and kind-agnostic, so a child that fails or is cancelled drops its
// occupancy in the same transaction that ends it.
func (d *Dispatcher) ReleaseLaneOccupancy(orderID int64) {
	if err := reservations.ReleaseAllOccupancy(d.db.DB, orderID); err != nil {
		log.Printf("lanegate: release occupancy for order %d: %v", orderID, err)
	}
}

// lanesFor maps nodes to the distinct lanes that contain them, skipping nodes
// that are not lane slots.
func (d *Dispatcher) lanesFor(ns ...*nodes.Node) []int64 {
	seen := make(map[int64]bool, len(ns))
	var out []int64
	for _, n := range ns {
		if n == nil {
			continue
		}
		lane, err := d.db.LaneForNode(n.ID)
		if err != nil {
			log.Printf("lanegate: resolve lane for node %d: %v", n.ID, err)
			continue
		}
		if lane == nil || seen[lane.ID] {
			continue
		}
		seen[lane.ID] = true
		out = append(out, lane.ID)
	}
	return out
}

// laneHoldRead is one lane's mouth rows as they came back, error included. The
// error is carried rather than handled so the classification below can see the
// difference between "read it, no dig" and "could not read it".
type laneHoldRead struct {
	rows []reservations.MouthHold
	err  error
}

// classifyLaneHoldCause turns those reads into the engineer-facing cause.
//
// Pure, and split out from the gathering for one reason: the interesting arm is
// the one where a read FAILED, and there is no way to make a SELECT fail for one
// lane in a shared test database without breaking every other test using the
// table. A pure classifier can be handed the failure directly. (The gathering
// half is a loop and a call; the decision is what had the bug.)
//
// Precedence is deliberate and is the fix:
//
//	a readable dig anywhere  -> lane-held-dig        (definite, and it wins)
//	otherwise any failed read -> lane-held-unreadable (we cannot rule a dig out)
//	otherwise                 -> lane-held-traffic    (definite)
//
// It used to `continue` past a failed read and fall through to traffic, so a
// dig-held lane whose row could not be read was filed as ordinary traffic
// contention. Nothing was unsafe — admission had already refused, and this only
// labels that refusal — but the label is what an engineer reads when a lane
// stalls, and a dig stall and a traffic stall are investigated differently. It
// is the §17.5/§17.8 family: not an alarm that fails to fire, an alarm that
// fires with the wrong name on it, which costs the next reader more than silence
// would.
//
// A definite dig still wins over an unreadable sibling lane, because a dig
// SEEN is a stronger fact than a lane not seen; reporting unreadable there
// would hide an answer we actually have.
func classifyLaneHoldCause(orderID int64, reads []laneHoldRead) QueueCause {
	unreadable := false
	for _, r := range reads {
		if r.err != nil {
			unreadable = true
			continue
		}
		for _, row := range r.rows {
			if row.OrderID != orderID && row.Mode == reservations.ModeDig {
				return CauseLaneHeldDig
			}
		}
	}
	if unreadable {
		return CauseLaneHeldUnreadable
	}
	return CauseLaneHeldTraffic
}

// causeForLaneHolds classifies a lane conflict for the engineer-facing queue
// cause (§6). The operator sees the same "Waiting for a slot at ‹lane›" in every
// case; this is the tag underneath it.
func (d *Dispatcher) causeForLaneHolds(orderID int64, holds []laneHold) QueueCause {
	reads := make([]laneHoldRead, 0, len(holds))
	for _, h := range holds {
		rows, err := reservations.ActiveMouthRows(d.db.DB, h.laneID)
		if err != nil {
			log.Printf("lanegate: mouth rows for lane %d unreadable while labelling order %d's wait: %v",
				h.laneID, orderID, err)
		}
		reads = append(reads, laneHoldRead{rows: rows, err: err})
	}
	return classifyLaneHoldCause(orderID, reads)
}

// ownsDig reports whether an order may pass a dig hold on a lane: it either IS
// the dig owner, or it is a leg of that dig.
//
// THE ONE DIG EXEMPTION. It absorbed isOwnDigLeg, which was the same predicate
// minus the owner arm and which admission and the retrieve classifier used
// while this one served the plain entry path. Two answers to one question is
// what the convergence ended; this is the answer that survived, because the
// arm it has and the other lacked is load-bearing and the arm it lacks is not
// reachable.
//
// ── THE OWNER ARM, WHICH isOwnDigLeg COULD NOT EXPRESS ────────────────────
//
// laneOwnerFor returns the order itself when it has no parent, and isOwnDigLeg
// required owner != order.ID, so a dig's own OWNER failed it. On the entry path
// that case is routine rather than exotic: in expose mode the lane lock is
// transferred to the complex parent (compound.go), and ResumeCompound puts that
// parent back through the scanner to re-resolve its own pickup — so the order
// asking is the dig owner, entering the lane its own dig holds, and that pickup
// is what releases the lock. Refusing it would not be a wait, it would be a
// wedge. Pinned by TestAcquireLanesForOrder_OwnDigAdmitsTheDigOwner.
//
// ── WHAT isOwnDigLeg's NARROWING PROTECTED, AND WHY LOSING IT IS SAFE ─────
//
// Carried forward verbatim from its doc, because it is a real future hazard
// rather than a historical note. The narrowing to CHILDREN existed so that a
// GATE-STAGED digger could not be released into the lane its own dig is still
// working: laneOwnerFor would return its own id, match, and open the gate.
//
// It cannot happen today, and not by luck — both planners divert a buried
// retrieve to Reshuffling before it ever reaches the fleet
// (complex_reshuffle.go planBuriedReshuffleAtIntake / handleComplexBuriedOnReplay,
// planning_service.go planBuriedReshuffle), so the digger carries no vendor
// order and is never gate-staged. GIVE THE DIGGER A PRE-POSITION AND THIS IS
// THE LINE TO REVISIT: the exemption would then need to be claim-scoped, or the
// release path would need the narrowing back on its own terms.
//
// The other line that makes this wrong if it moves is unchanged:
// reservations.AcquireLanes' dig rule. If it ever admits two claims on one
// lane, "the dig" stops being singular and parent identity stops identifying
// it. A lane carries exactly one dig claim because admitMouth refuses a second
// outright.
//
// Order-id only, deliberately: laneOwnerFor already answers the parent question,
// so nothing here needs the order struct and the common path costs no extra read.
func (d *Dispatcher) ownsDig(orderID, digOwner int64) bool {
	if digOwner == orderID {
		return true
	}
	return d.laneOwnerFor(orderID) == digOwner
}

// digRefusalFor WAS HERE AND IS NOW admission (admission.go).
//
// It answered the DIG question for an order about to work a set of nodes,
// REGARDLESS OF ENFORCEMENT MODE — the correction that closed the floor defect
// where a plain retrieve walked into a corridor another reshuffle owned. That
// correction is intact; only its implementation moved.
//
// It went because it was a second spelling of admission's first arm, and the
// two had drifted: this one exempted the dig's own OWNER (ownsDig) and
// admission's did not (isOwnDigLeg). One question with two answers is the thing
// the convergence exists to end. The surviving answer is ownsDig's — see the
// note on that function for which arm is load-bearing and why.
//
// AcquireLanesForOrder now asks the same question through the same function,
// declaring skipsForPlainEntry: the dig only, which is exactly what this asked.

// AcquireLanesForOrder takes the mouth holds a plain order needs before it
// dispatches — outbound on its source lane, inbound on its destination lane, for
// lanes whose group is configured for mouth enforcement. The scanner calls it
// just before the fleet commit; on a conflict it returns admitted=false plus the
// operator cause and the contended lane's name, and the scanner parks the order
// in sourcing under WAITING_FOR_SLOT holding its soft reservations (Rule 1).
//
// admitted=true with empty cause/lane means there was nothing to gate (no
// mouth-enforced lane on the order's path), so an unconfigured plant is a no-op
// and behavior is byte-identical. A non-nil error is a transient DB failure.
// DOES NOT DELEGATE TO admission, and the reason is structural rather than a
// preference: this is where admitMouth runs, which puts it on admission's
// INSIDE (see the boundary map in admission.go). A site cannot be both the thing admission
// calls and a caller of admission — that either closes a cycle or asks the
// physical questions twice for every lane-bound plain order.
//
// Putting admit in FRONT of the acquire is the same objection from the other
// side: it opens a read-to-write window the advisory lock exists not to have,
// and the acquire re-answers the part that matters anyway.
//
// It would also add no refusal that was missing — reachability for a plain
// order is covered by the finder and the gate classifier — only a second
// refusal point with a different cause, silently re-labelling what every
// lane-bound plain order parks under. That is the mislabel this branch fixed
// two commits ago, reintroduced at volume on the highest-traffic path.
//
// The audit of where a plain order DOES get each admission question answered is
// beside the boundary map in admission.go. It has one empty cell.
func (d *Dispatcher) AcquireLanesForOrder(order *orders.Order, sourceNode, destNode *nodes.Node, kind EntryKind) (admitted bool, cause QueueCause, laneName string, err error) {
	if order == nil {
		// A caller bug, not a refusal with a cause: there is no order to park and
		// no queue row to write one onto. Same disposition as admission's own
		// nil-order arm, for the same reason.
		return false, "", "", fmt.Errorf("lane gate: no order to acquire lanes for")
	}
	// THE PHYSICAL QUESTIONS FIRST, through admission — one function, one answer
	// per question. The skip set comes from the CALLER's kind (EntryFreshBin /
	// EntryHeldBin), because the one surviving skip — reachability — is justified
	// by the finder having answered it, and only the fresh caller went through the
	// finder. See skipsForEntry.
	//
	// Asked ahead of the mouth holds because a dig EXCLUDES everything: there is
	// no point taking holds on a lane that is not open to this order at all, and
	// the refusal carries its own cause and its own lane name.
	v, err := d.admit(admissionSituation{
		order:      order,
		sourceNode: sourceNode,
		destNode:   destNode,
		skip:       skipsForEntry(kind),
	})
	if err != nil {
		return false, "", "", err
	}
	if !v.Admitted() {
		return false, v.Cause(), v.Lane(), nil
	}

	holds, err := d.resolveOrderLaneHolds(sourceNode, destNode)
	if err != nil {
		return false, "", "", err
	}
	if len(holds) == 0 {
		return true, "", "", nil // nothing gated by the MOUTH — the dig is answered above
	}
	admitted, err = d.acquireOrderLanes(order.ID, holds)
	if err != nil {
		return false, "", "", err
	}
	if admitted {
		return true, "", "", nil
	}
	return false, d.causeForLaneHolds(order.ID, holds), d.laneDisplayName(holds), nil
}

// BuriedForHeldBin builds the BuriedError for an order whose HELD bin has become
// unreachable, so the caller can plan the dig that unburies it.
//
// It exists because a refusal is not an answer. Admission can now tell a held-bin
// order that its bin is walled, but a refusal with nothing to clear it is a
// permanent park — the wedge shape this stream keeps refusing to build. The fresh
// path never had that problem: the finder returns OutcomeReshuffle carrying a
// BuriedError, and the scanner turns it into PlanBuriedReshuffle. This is the same
// fact, reconstructed for the caller that did not go through the finder.
//
// The bin's CURRENT slot, not the order's remembered source_node: pickupSlotNow
// is the one definition of where a held bin actually sits, shared with the
// classifier and the gate rebind, and a dig planned against an abandoned slot
// would dig out the wrong thing.
func (d *Dispatcher) BuriedForHeldBin(order *orders.Order) (*BuriedError, error) {
	if order == nil || order.BinID == nil {
		return nil, fmt.Errorf("buried-for-held-bin: order holds no bin")
	}
	slot, err := d.db.GetNodeByDotName(order.SourceNode)
	if err != nil || slot == nil {
		return nil, fmt.Errorf("buried-for-held-bin: source node %q: %v", order.SourceNode, err)
	}
	lane, err := d.db.LaneForNode(slot.ID)
	if err != nil {
		return nil, err
	}
	if lane == nil {
		return nil, fmt.Errorf("buried-for-held-bin: %q is not a lane slot", slot.Name)
	}
	at, _, err := d.pickupSlotNow(order, lane)
	if err != nil {
		return nil, err
	}
	bin, err := d.db.GetBin(*order.BinID)
	if err != nil || bin == nil {
		return nil, fmt.Errorf("buried-for-held-bin: bin %d: %v", *order.BinID, err)
	}
	return &BuriedError{Bin: bin, Slot: at, LaneID: lane.ID}, nil
}

// laneDisplayName returns a human name for the first contended lane, for the
// "Waiting for a slot at ‹lane›" queue sentence.
func (d *Dispatcher) laneDisplayName(holds []laneHold) string {
	if len(holds) == 0 {
		return ""
	}
	if n, err := d.db.GetNode(holds[0].laneID); err == nil && n != nil {
		return n.Name
	}
	return ""
}

// ReleaseLanesForOrder drops all of an order's mouth holds. Used on a fleet-
// dispatch failure rollback: the robot never committed, so the hold taken by
// AcquireLanesForOrder must not linger and block the lane. Owner-scoped; a no-op
// when the order holds none.
func (d *Dispatcher) ReleaseLanesForOrder(orderID int64) error {
	return reservations.ReleaseLanesByOwner(d.db.DB, orderID)
}

// laneOwnerFor resolves the order that OWNS a lane mouth row for a block: the
// order itself for a plain order, or its complex parent for a compound child
// (children never own rows, §2). So a child's block progress releases the
// parent-owned hold.
func (d *Dispatcher) laneOwnerFor(orderID int64) int64 {
	o, err := d.db.GetOrder(orderID)
	if err != nil || o == nil || o.ParentOrderID == nil {
		return orderID
	}
	return *o.ParentOrderID
}

// HandleTransitForLaneGate releases the owner's mouth hold on the lane a picked
// bin just LEFT (§4 pickup / early handoff). Fired on EventBinEnteredTransit,
// routed to the compound parent for a child. A no-op when the from-node is not a
// lane or no mouth row is held — byte-identical when the gate is off.
func (d *Dispatcher) HandleTransitForLaneGate(orderID, fromNodeID int64) {
	if fromNodeID == 0 {
		return
	}
	owner := d.laneOwnerFor(orderID)
	node, err := d.db.GetNode(fromNodeID)
	if err != nil || node == nil {
		return
	}
	if err := d.releaseOrderLaneFor(owner, node); err != nil {
		log.Printf("lanegate: release hold for order %d (owner %d) on transit from node %d: %v",
			orderID, owner, fromNodeID, err)
	}
}

// ReleaseInboundLaneForOrder releases the owner's mouth hold on the lane an order
// just DROPPED into (§4 dropoff / early handoff). Fired from the store block-
// completion handler BEFORE the delivery-node early-return, routed to the compound
// parent for a child. A no-op when the drop node is not a lane or no mouth row is
// held.
func (d *Dispatcher) ReleaseInboundLaneForOrder(orderID int64, dropNodeName string) {
	if dropNodeName == "" {
		return
	}
	owner := d.laneOwnerFor(orderID)
	node, err := d.db.GetNodeByDotName(dropNodeName)
	if err != nil || node == nil {
		return
	}
	if err := d.releaseOrderLaneFor(owner, node); err != nil {
		log.Printf("lanegate: release hold for order %d (owner %d) on dropoff at %s: %v",
			orderID, owner, dropNodeName, err)
	}
}
