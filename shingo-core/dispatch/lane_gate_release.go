package dispatch

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"

	"shingo/protocol"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// The lane-gate RELEASE EVALUATOR — Core as traffic cop.
//
// Increment 3 gave every lane-bound order on a gate_choreography group the
// uniform unsealed shape, and appended the tail immediately when the classifier
// already admitted. A CONTENDED order was created unsealed and then dwelled with
// nothing to release it. This is that release.
//
// The shape is deliberately boring, because all three moving parts already exist:
//
//   POLICY     laneEntryCause — the landed tiered classifier, verbatim, the same
//              function the dispatch-time valve calls. Tier-1 same-origin
//              co-release, Tier-2 cross-origin deepest-first, Tier-3 group wait
//              are not re-expressed here; they ARE the classifier.
//   OCCUPANCY  the A′ predicate (stillWorkingLaneMouth): a store blocks until it
//              PLACES, which is when ReleaseInboundLaneForOrder deletes its
//              inbound mouth row. Nothing new is read.
//   APPEND     appendSegmentAndAdvance — the one fleet-append path, shared with
//              operator release, vehicle pinned.
//
// So the evaluator is a loop, an ordering, and two guards.
//
// WHY THE `others` SET IS STABLE ACROSS A PASS. A gate-staged candidate holds its
// inbound mouth row from dispatch (admitLanes runs before the fleet commit) and
// keeps it until it places — which is long after release. So releasing an order
// does NOT remove it from the blocker set: a shallower cross-origin candidate
// still sees it and still parks, exactly as it should. What DOES change mid-pass
// is a re-bind (§ rebindGatedDropoff), which moves a candidate's depth. That is
// why each candidate is evaluated against a freshly read view rather than one
// snapshot taken up front.

// laneGateRetryQueueThreshold is how many consecutive append failures on one
// order are tolerated before the wait becomes operator-visible. Below it a
// failure is a transient fleet blip and the next firing retries silently; at it,
// the order carries a structured queue code so the floor can see a robot is
// holding at a gate for a fleet reason rather than a traffic one.
const laneGateRetryQueueThreshold = 3

// laneGateSerializer serializes evaluator passes per lane.
//
// The evaluator fires from event subscribers, and the event bus dispatches on the
// EMITTING goroutine — so two different emitters (the RDS poller and the
// fulfillment scanner, say) can run a pass for the same lane at the same time.
// Two concurrent passes over one staged set would each read wait_index 0 and each
// append the same tail, and a duplicate blockId is the one thing SEER rejects
// outright.
//
// An in-process keyed mutex rather than a Postgres advisory lock, deliberately:
// Core is a single process, so the only thing a DB lock would add is cross-process
// exclusion that nothing needs — and it would have to be held across the RDS
// append, which means a database transaction open across an external HTTP call.
// That trades a race we can close in-process for connection-pool exposure and a
// lock that outlives a hung vendor call. The DURABLE half of the guard is the
// reload-and-recheck inside the critical section (see releaseGatedOrder), which is
// what actually makes a double-append impossible rather than merely unlikely.
type laneGateSerializer struct {
	mu    sync.Mutex
	locks map[int64]*sync.Mutex
}

func newLaneGateSerializer() *laneGateSerializer {
	return &laneGateSerializer{locks: make(map[int64]*sync.Mutex)}
}

// lock takes the per-lane mutex and returns its release func.
func (s *laneGateSerializer) lock(laneID int64) func() {
	s.mu.Lock()
	m, ok := s.locks[laneID]
	if !ok {
		m = &sync.Mutex{}
		s.locks[laneID] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// RedriveHeldCompoundLegs re-drives every compound parent holding a PENDING leg
// in this lane. Called on the same lane-clearing events as EvaluateLaneReleases.
//
// WHY THIS IS SEPARATE FROM THE EVALUATOR, and it is not a preference. It is NOT
// because the evaluator is mode-gated — it no longer is. That gate was deleted
// (F-05: it stranded every robot already dwelling when a mark was cleared, which
// is the one thing the enablement rollback rule promises it will not do), and the
// reasoning that used to sit here rested on it.
//
// The real reason is the POPULATION. The evaluator's candidates are gate-staged
// orders: a vendor order exists, a robot is parked at a wait point, and releasing
// one means appending a tail to a waybill the fleet already holds. A held
// compound leg is none of those things — it has no vendor order, it is `pending`,
// and "releasing" it means dispatching it for the first time. Two different sets,
// two different actions, one shared question: has this lane changed. Folding them
// would mean one loop branching on which kind of thing it was looking at, which
// is how the two answers drift.
//
// LEVEL-TRIGGERED, like the evaluator: it derives everything from live state, so
// a duplicate firing is a no-op (AdvanceCompoundOrder re-reads and re-admits), a
// dropped one costs only latency, and no subscriber has to agree about ordering.
// A timer would be a backstop for this; the lane-clearing event is the mechanism.
//
// No per-lane mutex. AdvanceCompoundOrder does its own arbitration — the status
// CAS is the atomic claim on a child, and the occupancy row is the lane's — so
// serializing here would add a second, weaker answer to a question already
// settled underneath.
func (d *Dispatcher) RedriveHeldCompoundLegs(laneID int64) {
	parents, err := d.db.ListHeldLegParentsInLane(laneID)
	if err != nil {
		log.Printf("lanegate: list held legs in lane %d: %v", laneID, err)
		return
	}
	for _, parentID := range parents {
		if err := d.AdvanceCompoundOrder(parentID); err != nil {
			log.Printf("lanegate: re-drive compound %d after lane %d cleared: %v", parentID, laneID, err)
		}
	}
}

// EvaluateLaneReleases runs one release pass over a lane: every gate-staged order
// the classifier now admits gets its tail appended and is released into the lane.
//
// Idempotent and level-triggered — it derives everything from live state and
// nothing from the event that woke it. A firing that finds nothing to do is a
// no-op, a dropped event costs only latency (the next firing recovers), and a
// duplicate event is harmless. That is what lets the trigger set be generous.
//
// ── IT DOES NOT CONSULT THE LANE'S MARK, AND THAT IS THE ROLLBACK RULE ────
//
// There was a `if !d.laneIsGated(lane.ID) { return }` here, sold as "an
// unconfigured plant does one lane lookup and returns". It also stranded every
// robot already dwelling when a mark was cleared, which is the one thing the
// enablement ruling promises it will not do: "rollback is clearing it (robots
// already dwelling complete under the old rules)" (lane_gate.go). Clearing a
// mark is an admin click with a confirmation that COUNTS the dwellers first
// (GateStagedCount → www apiLaneWaiting), so the stranded population was
// displayed to the person about to strand it.
//
// A dwelling robot is not a fact about configuration. It is unsealed at the
// fleet, parked on a Wait block, and only Core can append its tail — so whether
// it may be released is a question about the ORDER, which is exactly what the
// candidate derivation below already asks (gate-staged, wait kind lane,
// wait_lane = this lane). Deriving the set from durable order state and then
// gating the whole pass on live config was the one place those two disagreed.
//
// WHAT IT COSTS on an ungated plant: one ActiveGateCandidates query per
// lane-touching event, which returns nothing when no order carries a lane wait.
// Its sibling on the same events — RedriveHeldCompoundLegs — already runs a
// per-lane query with no mode gate at all, and for the same reason: the state
// it recovers is mode-independent.
func (d *Dispatcher) EvaluateLaneReleases(laneID int64) {
	lane, err := d.db.GetNode(laneID)
	if err != nil || lane == nil || lane.ParentID == nil {
		return
	}

	// THE PASS RUNS UNDER THE LANE'S MUTEX; THE HEAL RUNS AFTER IT. Firing a dig
	// creates a compound, which dispatches its first leg synchronously, which emits
	// events, which the bus delivers ON THIS GOROUTINE to subscribers that call
	// EvaluateLaneReleases for this same lane. The per-lane mutex is not reentrant,
	// so doing it inside the pass is a self-deadlock — the same shape the dissolve
	// site documents for scanMu (compound.go). Split rather than made reentrant: a
	// reentrant lock here would let a nested pass append a tail against state the
	// outer pass is halfway through changing, which is what the mutex is for.
	//
	// Outside it, the nested pass is harmless and bounded: it finds the dig lock
	// held, refuses everyone with lane-dig-active, and returns.
	heal, wanted := d.evaluateLaneReleasesPass(lane)
	if wanted {
		d.healLaneMouth(lane, heal)
	}
}

// evaluateLaneReleasesPass is the pass proper — everything that happens under the
// lane's mutex. It returns the one heal the caller should fire afterwards, if any.
//
// ONE PER PASS, not one per candidate, and the reason is that a dig is a fact
// about the LANE rather than about the order that noticed. The wall in front of a
// depth-2 store is the same wall in front of the depth-4 store behind it, so one
// excavation frees them all; a second request would find the lane locked by the
// first and do nothing. Returning one keeps that obvious instead of relying on the
// lock to absorb the duplicates.
func (d *Dispatcher) evaluateLaneReleasesPass(lane *nodes.Node) (healRequest, bool) {
	laneID := lane.ID
	unlock := d.laneGates.lock(laneID)
	defer unlock()

	var (
		heal       healRequest
		healWanted bool
	)

	candidates, err := d.gateStagedForLane(lane)
	if err != nil {
		log.Printf("lane gate: list staged orders for lane %s: %v", lane.Name, err)
		return heal, false
	}

	// Deepest first — but be clear about what this sort does and does not buy,
	// because it is easy to mistake it for the safety mechanism.
	//
	// It is NOT what keeps a shallower order from walling a deeper one. That is
	// the classifier's Tier-2 rule: a candidate with a deeper un-placed
	// cross-origin store in the lane is held no matter when it is evaluated, so
	// iterating shallow-first produces the same releases in the same pass.
	// (Verified: reversing this comparison leaves every gate scenario and the
	// 200-seed soak green, whereas disabling Tier-2 walls 160/200 seeds.)
	//
	// What it buys is that the deepest admissible order goes out on THIS pass
	// rather than the next firing.
	//
	// It used to buy more: the appends left in the same sequence as the
	// depth-derived RDS priorities they carried, so Core's append order and the
	// fleet's own dispatch order could not disagree. That boost is deleted (see
	// lane_gate.go), so the fleet now sequences these however it sequences equal
	// priorities, and this sort no longer has a counterpart on the RDS side. The
	// safety property is unaffected — that is Tier 2's, stated below, not this
	// sort's.
	//
	// Tier 1 rides on the same loop: a SAME-ORIGIN partner is skipped by the
	// classifier rather than depth-gated, so a press pair is released together in
	// one pass instead of one partner waiting for the other to place.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].depth != candidates[j].depth {
			return candidates[i].depth > candidates[j].depth
		}
		return candidates[i].order.ID < candidates[j].order.ID // deterministic
	})

	// ONE LOOP. There used to be two, because there were two candidate queries —
	// one matching delivery_node for stores, one matching source_node for
	// retrieves. Direction is now read off the PLAN (the first actionable step
	// after the wait: a pickup means the robot is going in to take something out),
	// so which query found the order stops being how Core knows what it is.
	//
	// What stays per-direction is the release itself, and legitimately so: a store
	// re-binds its DROPOFF against the lane as it stands, a retrieve re-binds its
	// PICKUP against where its bin actually sits. Those are different facts about
	// different ends of the order.
	released := 0
	// WINDOW 3's QUESTION IS ASKED OF EVERY CANDIDATE THAT DID NOT GET IN, whatever
	// stopped it. A Tier-2 park, a lane-occupied refusal and a failed re-bind can
	// all be sitting on top of the same unclaimed bin, so keying the heal on a
	// particular refusal cause would find it from one arm and miss it from the other
	// two. The refusal is the PROMPT; mouthHealNeeded reads the physics and answers.
	propose := func(c gateCandidate) {
		if healWanted {
			return // one dig per pass — see the function doc
		}
		if req, ok := d.mouthHealNeeded(lane, c); ok {
			heal, healWanted = req, true
		}
	}
	for _, c := range candidates {
		// Re-read per candidate rather than reusing one snapshot: a re-bind by an
		// earlier candidate in this pass moves its depth and its bound node, and
		// the classifier's view has to see that.
		v, cErr := d.gateEntryVerdict(lane, c.order, c.node, c.retrieve)
		if cErr != nil {
			// AND THE CAUSE GOES ON THE ROW, exactly as the refusal arm below does
			// it. This arm logged and continued, so a robot dwelling at a mark
			// because Core could not READ the lane was indistinguishable, on the row,
			// from one nobody had evaluated yet — and the two are investigated
			// differently. `dcb2c014` gave the refusal arm its cause and left this one
			// blank, which is the same gap it closed, one branch over.
			//
			// CauseAdmissionError, not a lane cause: an undetermined answer is Core
			// declining, not a busy lane. Same distinction, same constant, as the
			// compound leg's admission-error arm (compound.go).
			//
			// The order stays a candidate — the set is derived from durable order
			// state, never from a verdict — so the next firing re-asks, and the cause
			// is cleared on entry like every other.
			log.Printf("lane gate: classifier error for order %d on lane %s: %v", c.order.ID, lane.Name, cErr)
			d.setQueueReason(c.order, protocol.QueueWaitingForSlot, CauseAdmissionError,
				QueueParams{Lane: lane.Name})
			continue
		}
		if !v.Admitted() {
			// STAYS A CANDIDATE. The candidate set is derived from the order's own
			// durable state — gate-staged, wait_kind lane, wait_lane this lane — and
			// never from a verdict, so a refusal here removes nothing. The next
			// firing re-derives the same set and re-asks. That matters more since
			// the unification: a gate-staged retrieve is now refused while a plain
			// store holds occupancy, which is a condition that clears on an event
			// this evaluator is already subscribed to.
			// AND IT SAYS WHY, ON THE ROW. A dwell used to be the one wait state
			// with nothing an operator could read: the order sits `staged` with a
			// robot parked at the mark and a blank queue_cause, so the board shows a
			// robot doing nothing and no sentence explaining it. Three of them ran
			// 77 minutes that way on the lane-stress rig before anyone could say
			// what they were waiting for, and the answer was in this verdict the
			// whole time.
			//
			// Same code and cause vocabulary as the pre-dispatch park, deliberately:
			// a lane-occupied wait reads identically whether the order is parked
			// before dispatch or dwelling at a mark, because to the operator it is
			// the same fact about the same lane. Only where the robot is standing
			// differs, and that is not the operator's problem to reconcile.
			d.setQueueReason(c.order, protocol.QueueWaitingForSlot, v.Cause(),
				QueueParams{Lane: lane.Name})
			d.dbg("lane gate: order %d still held at %s (%s)", c.order.ID, lane.Name, v.Cause())
			propose(c)
			continue
		}
		var rErr error
		if c.retrieve {
			rErr = d.releaseGatedRetrieve(c.order, lane)
		} else {
			rErr = d.releaseGatedOrder(c.order, lane)
		}
		if rErr != nil {
			log.Printf("lane gate: release order %d into lane %s: %v", c.order.ID, lane.Name, rErr)
			propose(c)
			continue
		}
		// CLEARED ON ENTRY. The cause described a wait that is over; leaving it
		// would put a stale "waiting for a slot" on a robot that is now driving,
		// which is the same lie as the blank was, told the other way round.
		d.setQueueReason(c.order, "", "", QueueParams{})
		released++
	}

	if released > 0 {
		log.Printf("lane gate: released %d order(s) into lane %s", released, lane.Name)
	}
	return heal, healWanted
}

// gateCandidate is one gate-staged order with the destination and depth the
// ordering and the classifier need.
type gateCandidate struct {
	order *orders.Order
	// node is the LANE-RELEVANT node: the slot the order's next step works —
	// its dropoff target for a store, its pickup slot for a retrieve. It was
	// called `dest` when two queries produced it and the retrieve query had to
	// explain that it carried the source there instead.
	node  *nodes.Node
	depth int
	// retrieve is the direction, read off the plan (a pickup after the wait)
	// rather than inferred from which query found the order.
	retrieve bool
}

// gateStagedForLane returns every order dwelling at THIS lane's gate.
//
// ── IT KEYS ON THE WAIT STEP, NOT ON AN ENDPOINT COLUMN ───────────────────
//
// Two queries used to do this: one matching delivery_node against the lane's
// slot names (stores), one matching source_node (retrieves). Both asked "is one
// END of this order in my lane", and that is not the question. The question is
// "is this order parked at a wait that belongs to me", and the answer is written
// on the wait itself (WaitKind/WaitLane, stamped when the wait is minted).
//
// The endpoint form had a structural blind spot rather than a bug: an order
// whose lane entry is INTERIOR to its plan — an evac that picks at a line, drops
// into a lane, and delivers to a line — has neither endpoint in the lane, so
// NEITHER query could see it and no evaluator would ever release it. It dwelt
// until the abandon sweep. Nothing in the plain valve produced that shape, which
// is why it never bit; the splice produces it routinely.
//
// DIRECTION COMES FROM THE PLAN. The first actionable step after the wait says
// what the robot is going in to do: a pickup is a retrieve (it takes something
// out), a dropoff is a store. That node is also the lane-relevant one — what the
// classifier reads and what the depth sort orders on — so one walk answers all
// three.
//
// Faulted orders are excluded. A faulted leg is mid-recovery, not dead, and
// appending to it would race the recovery; the operator-release path refuses a
// faulted order for the same reason (complex_release.go). It re-enters the set
// on its own once it recovers, because nothing about its row changed.
func (d *Dispatcher) gateStagedForLane(lane *nodes.Node) ([]gateCandidate, error) {
	active, err := d.db.ActiveGateCandidates()
	if err != nil {
		return nil, err
	}
	var out []gateCandidate
	for _, o := range active {
		if !IsGateStaged(o) || o.Status == StatusFaulted {
			continue
		}
		var steps []resolvedStep
		if uErr := json.Unmarshal([]byte(o.StepsJSON), &steps); uErr != nil {
			// IsGateStaged already parsed this and logged; it cannot be true here
			// on an unparseable plan. Defensive only.
			continue
		}
		w, ok := waitAt(steps, o.WaitIndex)
		if !ok || w.WaitLane != lane.ID {
			continue // parked at somebody else's wait, or at none
		}
		entry, isRetrieve, ok := laneEntryAfterWait(steps, o.WaitIndex)
		if !ok {
			log.Printf("lane gate: order %d is parked at a wait for lane %s with no actionable step "+
				"after it — its tail cannot be built", o.ID, lane.Name)
			continue
		}
		node, nErr := d.db.GetNodeByDotName(entry.Node)
		if nErr != nil || node == nil {
			log.Printf("lane gate: order %d's lane entry %q does not resolve: %v", o.ID, entry.Node, nErr)
			continue
		}
		// The wait says which lane owns it; this checks the PLAN agrees. A
		// disagreement is a mis-spliced plan, and it is loud rather than skipped
		// quietly, because the order would otherwise dwell with no diagnosis.
		entryLane, lErr := d.db.LaneForNode(node.ID)
		if lErr != nil {
			return nil, lErr
		}
		if entryLane == nil || entryLane.ID != lane.ID {
			log.Printf("lane gate: order %d waits for lane %s but its next step %q is in %s — "+
				"mis-spliced plan, refusing to release it here",
				o.ID, lane.Name, entry.Node, nodeName(entryLane))
			continue
		}
		depth, dErr := d.db.GetSlotDepth(node.ID)
		if dErr != nil {
			return nil, dErr
		}
		out = append(out, gateCandidate{order: o, node: node, depth: depth, retrieve: isRetrieve})
	}
	return out, nil
}

// gateEntryVerdict is the ONE question asked before a gate-staged order is let
// into its lane, at both moments it is asked: the valve's immediate check at
// create time, and every evaluator pass afterwards.
//
// A RETRIEVE asks the physical questions and nothing else. Ordering does not
// apply to it — retrieves do not wall each other, they free each other, and the
// mouth gate already serialises same-mode sharers.
//
// A STORE asks the physical questions AND the ordering tiers, in that order.
// Physical first because a refusal there is absolute: a dig excludes everything,
// and there is no point asking whose turn it is on a lane nobody may enter. The
// tiers then answer the question the physical checks do not — a lane that IS open
// can still be one this order should not enter yet, because a deeper cross-origin
// store has not placed.
//
// The store's skip set is documented at skipsForGatedStoreEntry — it now skips
// NOTHING, so a partner released earlier in this same pass is visible to the next
// candidate through its occupancy row.
func (d *Dispatcher) gateEntryVerdict(lane *nodes.Node, order *orders.Order, entry *nodes.Node, isRetrieve bool) (GateVerdict, error) {
	if isRetrieve {
		return d.laneGateRetrieveCause(entry, order)
	}
	v, err := d.admit(admissionSituation{
		order:    order,
		destNode: entry,
		skip:     skipsForGatedStoreEntry,
	})
	if err != nil || !v.Admitted() {
		return v, err
	}
	return d.laneEntryCause(lane, order, entry)
}

// laneEntryAfterWait returns the first actionable step after the wait at
// waitIndex, and whether it is a PICKUP (the retrieve direction).
//
// It is the same enumeration waitAt and splitSegment use — every ActionWait
// counts, bare or not — walked one step further to the work the wait is gating.
func laneEntryAfterWait(steps []resolvedStep, waitIndex int) (resolvedStep, bool, bool) {
	seen, start := 0, -1
	for i, s := range steps {
		if s.Action != protocol.ActionWait {
			continue
		}
		if seen == waitIndex {
			start = i + 1
			break
		}
		seen++
	}
	if start < 0 {
		return resolvedStep{}, false, false
	}
	for _, s := range steps[start:] {
		switch s.Action {
		case protocol.ActionPickup:
			return s, true, true
		case protocol.ActionDropoff:
			return s, false, true
		}
	}
	return resolvedStep{}, false, false
}

// outbound mirror of laneEntryCause. A retrieve dwelling at the gate is parked
// (held) when EITHER:
//
//   - a dig belonging to SOMEONE ELSE holds the lane (ModeDig is always-
//     exclusive: nothing enters while a dig works, and "everything respects the
//     dig" is the whole point of gating). Its own legs are exempt — see below.
//   - the bin the retrieve wants is BURIED — a shallower slot in the lane still
//     holds a bin, so the target is physically unreachable until that bin leaves.
//
// Otherwise the lane is safe and the retrieve is released: the tail's pickup+dropoff
// is appended and the robot enters to pull the bin.
//
// This is deliberately NOT a depth-ordered entry classifier like laneEntryCause
// (the store path's Tier 1/2/3). Those tiers exist to stop stores from WALLING each
// other — two stores into one lane must enter deepest-first or the shallow one
// blocks the deep. Retrieves do the opposite: a retrieve REMOVES a bin, so two
// retrieves into one lane free each other rather than wall, and the only thing that
// can actually stop a retrieve is a dig or a real burial. The single-file mouth gate
// (reservations/mouth.go admitMouth) already serializes same-mode sharers, so
// retrieve-vs-retrieve ordering is handled there, not here. (See buried_retrieve_test
// .go's claim: the gain is travel overlap ONLY, never entry at exposure.)
//
// The burial bound is the slot the bin sits in NOW (pickupSlotNow), not the one
// the order was born wanting. A dig can move the bin while this order dwells, and
// asking about the abandoned slot is wrong in both directions: a bin moved
// SHALLOWER makes the old slot read buried — by the very bin the order wants —
// and a bin moved DEEPER behind another makes the old slot read clear, which
// would append a pickup into a walled slot.
// ── IT IS ADMISSION, ASKED AT THE SOURCE END, AND IT SKIPS NOTHING ────────
//
// The body was a third copy of the physical questions and is now one call. It
// briefly carried a skip set (dig and burial, not occupancy); the unification
// retired it — see the note where skipsForGateStagedRetrieve used to be. A
// gate-staged retrieve now asks every physical question, which is the same set
// a compound leg asks, which is the point of there being one function.
//
// Both of admission's arms behave as the hand-written version did: an unreadable
// dig row PROPAGATES rather than resolving to an answer (DigOwner rather than
// IsLocked, so the failure cannot hide as a routine wait), and the caller's
// disposition for an error and for a refusal is the same — log it and leave the
// order parked at the gate, which is exactly what a gate wait is for.
//
// IT CANNOT BLOCK ITSELF on the occupancy it now asks about: a gate-staged
// retrieve takes its own occupancy row when its TAIL is appended
// (appendGateTail), which is strictly after this verdict admits.
//
// sourceNode rather than the lane, because admission resolves lanes itself and
// a caller handing it a pre-resolved one would be a second answer to
// "which lane is this". Both callers have the node: the valve from its own
// dispatch arguments, the evaluator from its candidate row.
func (d *Dispatcher) laneGateRetrieveCause(sourceNode *nodes.Node, order *orders.Order) (GateVerdict, error) {
	return d.admit(admissionSituation{
		order:      order,
		sourceNode: sourceNode,
	})
}

// pickupSlotNow answers ONE question — which slot does this retrieve's wanted bin
// sit in right now — for the two readers that used to answer it separately from
// the order's remembered source_node: the classifier's burial bound, and the
// release-time rebind. moved reports whether that is a different slot from the one
// the order names.
//
// It reads, it does not write. rebindGatedPickup is the only writer of the answer,
// so the classifier asking the same question a moment earlier is two reads of one
// definition and not a second writer for one fact.
//
// Resolution is by BIN, not by slot, because the bin is what the order is owed.
// bin_id is stamped by the scanner before dispatch (fulfillment/scanner.go), so a
// gate-staged plain retrieve carries one; the nil case is the older/empty-intent
// rows that never got a claim, and those fall back to the order's own source node
// — exactly what every retrieve did before this existed.
//
// A bin that has left the LANE is an error, not a rebind: there is no slot here to
// bind to. The order stays parked and the stuck-order sweep bounds it (it is in a
// sweep-eligible state by construction — see the commit for why a wedged gate
// order can never be in_transit). A second give-up path keyed on the same
// condition would be a second writer for one fact.
func (d *Dispatcher) pickupSlotNow(order *orders.Order, lane *nodes.Node) (slot *nodes.Node, moved bool, err error) {
	named, err := d.db.GetNodeByDotName(order.SourceNode)
	if err != nil {
		return nil, false, err
	}
	if order.BinID == nil {
		if named == nil {
			return nil, false, fmt.Errorf("pickup slot: source node %q not found", order.SourceNode)
		}
		return named, false, nil
	}
	bin, err := d.db.GetBin(*order.BinID)
	if err != nil {
		return nil, false, err
	}
	if bin == nil || bin.NodeID == nil {
		return nil, false, fmt.Errorf("pickup slot: bin %d for order %d is gone or has no node",
			*order.BinID, order.ID)
	}
	if named != nil && *bin.NodeID == named.ID {
		return named, false, nil // the common case: nothing moved, nothing to write
	}
	at, err := d.db.GetNode(*bin.NodeID)
	if err != nil {
		return nil, false, err
	}
	if at == nil || at.ParentID == nil || *at.ParentID != lane.ID {
		return nil, false, fmt.Errorf("pickup slot: bin %d left lane %s (now at %s) — no slot in this lane to bind",
			*order.BinID, lane.Name, nodeName(at))
	}
	return at, true, nil
}

// isOwnDigLeg WAS HERE AND IS NOW ownsDig (lane_gate.go).
//
// It reported whether `order` is a LEG of the dig holding the lane, and it was
// the same predicate as ownsDig minus the owner arm. Two answers to one
// question, on a question the convergence reduced to one function — so one had
// to go, and this was the one whose missing arm was reachable (a resumed dig
// owner re-entering its own lane; a wedge, pinned by
// TestAcquireLanesForOrder_OwnDigAdmitsTheDigOwner).
//
// BOTH of its load-bearing notes moved with it rather than being dropped: the
// parent-identity argument, and the warning about what its narrowing to
// CHILDREN protected — a gate-staged digger being released into its own dig.
// See ownsDig for the current statement of both, including the line to revisit
// if a digger is ever given a pre-position.

// releaseGatedOrder binds the dropoff against the lane as it stands, appends the
// tail, and seals the order.
//
// ── The double-append guard ───────────────────────────────────────────────
// Called only under the per-lane mutex, and the FIRST thing it does is re-read
// the order and re-check IsGateStaged. wait_index is the durable witness: the
// shared append helper advances it to 1 only after ReleaseOrder returns nil, so a
// second pass that raced this one reloads, sees wait_index 1, and returns without
// touching the fleet. Reloading is what makes the guard durable rather than a
// property of one in-memory struct — two passes hold two different *orders.Order
// values, and the stale one would otherwise pass every check.
//
// lifecycle.Release inside the helper is the backstop: it validates
// staged→in_transit against the live status and refuses an already-released order
// (lifecycle.go transition → protocol.IsValidTransition).
func (d *Dispatcher) releaseGatedOrder(order *orders.Order, lane *nodes.Node) error {
	fresh, err := d.db.GetOrder(order.ID)
	if err != nil || fresh == nil {
		return err
	}
	if !IsGateStaged(fresh) {
		d.dbg("lane gate: order %d no longer gate-staged (wait_index=%d status=%s) — another pass released it",
			fresh.ID, fresh.WaitIndex, fresh.Status)
		return nil
	}

	dest, err := d.rebindGatedDropoff(fresh, lane)
	if err != nil {
		// The lane moved against this order and no reachable slot is available.
		// Refusing is the safe disposition: appending into an occupied or walled
		// slot is the exact failure the bind-at-release rule exists to prevent.
		// The order stays staged and the next firing re-tries.
		d.setQueueReason(fresh, protocol.QueueWaitingForSlot, CauseGateRebindUnavailable,
			QueueParams{Destination: lane.Name})
		return err
	}

	if err := d.appendGateTail(fresh, "lane gate release"); err != nil {
		d.noteGateAppendFailure(fresh, lane)
		return err
	}
	d.clearGateAppendFailures(fresh.ID)
	d.setQueueReason(fresh, "", "", QueueParams{})
	d.dbg("lane gate: order %d released into lane %s at %s", fresh.ID, lane.Name, dest.Name)
	return nil
}

// releaseGatedRetrieve is the retrieve mirror of releaseGatedOrder: re-bind the
// pickup against where the bin actually sits NOW, append the [pickup, dropoff]
// tail, and seal. Same double-append guard (re-read + IsGateStaged re-check under
// the per-lane mutex) — wait_index is the witness either way.
//
// The pickup re-bind is the retrieve's reason to bind at release: a dig may have
// moved the wanted bin while this order dwelled at the gate, so the slot the order
// was born wanting is not necessarily the slot the bin occupies when the lane opens.
func (d *Dispatcher) releaseGatedRetrieve(order *orders.Order, lane *nodes.Node) error {
	fresh, err := d.db.GetOrder(order.ID)
	if err != nil || fresh == nil {
		return err
	}
	if !IsGateStaged(fresh) {
		d.dbg("lane gate: retrieve %d no longer gate-staged (wait_index=%d status=%s) — another pass released it",
			fresh.ID, fresh.WaitIndex, fresh.Status)
		return nil
	}

	src, err := d.rebindGatedPickup(fresh, lane)
	if err != nil {
		// The bin moved (a dig relocated it) and its current location is not the lane
		// slot the order holds, or the slot is no longer reachable. Refusing is safe:
		// appending a pickup against a slot the bin left would pick the wrong bin (or
		// none). The order stays staged; the dig/reshuffle machinery updates the bin's
		// location and the next firing re-tries once the lane is consistent.
		d.setQueueReason(fresh, protocol.QueueWaitingForSlot, CauseGateRebindUnavailable,
			QueueParams{Destination: lane.Name})
		return err
	}

	if err := d.appendGateTail(fresh, "lane gate release (retrieve)"); err != nil {
		d.noteGateAppendFailure(fresh, lane)
		return err
	}
	d.clearGateAppendFailures(fresh.ID)
	d.setQueueReason(fresh, "", "", QueueParams{})
	d.dbg("lane gate: retrieve %d released into lane %s from %s", fresh.ID, lane.Name, src.Name)
	return nil
}

// rebindGatedPickup re-resolves the order's pickup against where its bin ACTUALLY
// SITS at the moment of append. Returns the node the tail will pick from.
//
// The pickup mirror of rebindGatedDropoff, and now with the same three outcomes:
//
//   - the bin is still at the order's own slot → keep it. No writes at all.
//   - the bin moved to a different slot in this lane → re-point the order at it.
//   - the bin is gone, or left the lane → error; the caller refuses to release.
//
// It used to have only the first and third, and the third swallowed the second:
// an emptied source slot returned an error whatever the reason, so a bin a dig had
// merely SHUFFLED — still in the lane, still reachable, still owed to this order —
// read as unavailable. The caller logs that and continues, which leaves the order
// staged for an identical refusal on every later pass. Nothing moves the bin back.
//
// The resolution itself is pickupSlotNow, shared with the classifier, because the
// classifier's burial test had the same stale-slot bug and fixing one without the
// other leaves the order parked before it ever reaches this function.
func (d *Dispatcher) rebindGatedPickup(order *orders.Order, lane *nodes.Node) (*nodes.Node, error) {
	at, moved, err := d.pickupSlotNow(order, lane)
	if err != nil {
		return nil, err
	}
	if !moved {
		// Unchanged. The emptiness check survives for the bin_id-less fallback, where
		// pickupSlotNow can only report the slot the order names and cannot tell
		// whether anything is still in it — refusing beats picking nothing.
		bins, bErr := d.db.ListBinsByNode(at.ID)
		if bErr != nil {
			return nil, bErr
		}
		if len(bins) == 0 {
			return nil, fmt.Errorf("rebind pickup: source slot %s is empty — the bin moved; refusing to release", at.Name)
		}
		return at, nil
	}

	// applySourceNode, not a raw source_node write: it patches the deferred pickup
	// step in steps_json in the same breath, and that tail is exactly what the append
	// is about to emit. A raw write would send the robot to the slot the bin left.
	was := order.SourceNode
	if err := applySourceNode(d.db, order, at.Name); err != nil {
		return nil, err
	}
	log.Printf("lane gate: retrieve %d re-bound at release %s → %s (lane %s, bin %d)",
		order.ID, was, at.Name, lane.Name, *order.BinID)
	return at, nil
}

// rebindGatedDropoff re-resolves the order's dropoff against the lane AS IT
// STANDS, at the moment of append. Returns the node the tail will target.
//
// This is the bind-at-release property, and it is the reason a gated create
// carries no dropoff block at all: there is no committed binding to go stale, so
// there is nothing to re-confirm — only something to resolve, once, late.
//
// Three outcomes, and the common one writes nothing:
//
//   - the order's own slot is still the deepest reachable pick → keep it. The
//     owner-aware resolver is what makes this the answer instead of a spurious
//     move to a shallower slot (see nodes.FindStoreSlotInLaneExcluding).
//   - a different slot wins — its own was walled by a bin that landed shallower
//     while it dwelled, or a DEEPER slot freed → move to it.
//   - nothing is available → error; the caller refuses to release.
//
// The move takes the new slot BEFORE releasing the old one, so a failure part-way
// leaves the order holding exactly what it held before. Holding two slot rows for
// the instant between is legal: the uniqueness index is per NODE, not per order.
func (d *Dispatcher) rebindGatedDropoff(order *orders.Order, lane *nodes.Node) (*nodes.Node, error) {
	current, err := d.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		return nil, err
	}
	best, err := d.db.FindStoreSlotInLaneExcluding(lane.ID, order.ID)
	if err != nil {
		return nil, err // no reachable empty slot right now — refuse to release
	}
	if current != nil && best.ID == current.ID {
		return current, nil // still the right slot; no writes at all
	}

	if err := claimStoreSlot(d.db, order, best); err != nil {
		return nil, err
	}
	if err := d.confirmDropoffSlot(order, best); err != nil {
		return nil, err
	}
	if current != nil {
		if rErr := d.db.ReleaseSlotClaim(current.ID, order.ID); rErr != nil {
			// Non-fatal: the new slot is secured and the append is correct. A
			// lingering old hold is reclaimed by the owner-liveness reaper when the
			// order goes terminal. Log it — a leaked hold narrows the lane.
			log.Printf("lane gate: order %d re-bound %s → %s but releasing the old slot failed: %v",
				order.ID, current.Name, best.Name, rErr)
		}
	}
	// applyDeliveryNode, not a raw delivery_node write: it patches the deferred
	// tail in steps_json in the same breath, and that tail is exactly what the
	// append is about to emit. A raw write would append a block for the slot the
	// order no longer owns.
	if err := applyDeliveryNode(d.db, order, best.Name); err != nil {
		return nil, err
	}
	log.Printf("lane gate: order %d re-bound at release %s → %s (lane %s)",
		order.ID, nodeName(current), best.Name, lane.Name)
	return best, nil
}

func nodeName(n *nodes.Node) string {
	if n == nil {
		return "(unbound)"
	}
	return n.Name
}

// noteGateAppendFailure counts consecutive append failures for an order and makes
// the wait operator-visible once they stop looking transient. Below the threshold
// the next firing simply retries: one failed append against a busy fleet is not
// worth a floor-facing message.
//
// The counter is in-process and crash-volatile ON PURPOSE — it is a debounce, not
// a fact. Losing it on restart re-arms the debounce, which is the safe direction:
// the order is still staged, still retried, and still visible once the failures
// repeat. Nothing about recovery depends on it.
func (d *Dispatcher) noteGateAppendFailure(order *orders.Order, lane *nodes.Node) {
	d.gateFailMu.Lock()
	d.gateAppendFails[order.ID]++
	n := d.gateAppendFails[order.ID]
	d.gateFailMu.Unlock()
	if n < laneGateRetryQueueThreshold {
		return
	}
	d.setQueueReason(order, protocol.QueueFleetUnavailable, CauseGateAppendFailed,
		QueueParams{Destination: lane.Name})
}

func (d *Dispatcher) clearGateAppendFailures(orderID int64) {
	d.gateFailMu.Lock()
	delete(d.gateAppendFails, orderID)
	d.gateFailMu.Unlock()
}

// LaneIDsForGateEvent maps a node the caller just saw change to the lane whose
// gate should be re-evaluated, or 0 when the node is not a lane slot. The engine
// subscribers use it to turn an occupancy event into a lane without knowing
// anything about lane geometry.
func (d *Dispatcher) LaneIDsForGateEvent(nodeID int64) int64 {
	if nodeID == 0 {
		return 0
	}
	lane, err := d.db.LaneForNode(nodeID)
	if err != nil || lane == nil {
		return 0
	}
	return lane.ID
}

// LaneIDForNodeName is LaneIDsForGateEvent for a node addressed by name (block
// locations arrive as vendor strings, not ids). Returns 0 when it is not a lane
// slot.
func (d *Dispatcher) LaneIDForNodeName(name string) int64 {
	if name == "" {
		return 0
	}
	node, err := d.db.GetNodeByDotName(name)
	if err != nil || node == nil {
		return 0
	}
	return d.LaneIDsForGateEvent(node.ID)
}

// LaneIDsForOrder returns the lanes an order touches (its source and its
// destination), for the terminal-event triggers: an order that dies holding a
// mouth row frees whichever lane it was working.
func (d *Dispatcher) LaneIDsForOrder(orderID int64) []int64 {
	order, err := d.db.GetOrder(orderID)
	if err != nil || order == nil {
		return nil
	}
	seen := map[int64]bool{}
	var out []int64
	add := func(id int64) {
		if id != 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, name := range []string{order.DeliveryNode, order.SourceNode} {
		add(d.LaneIDForNodeName(name))
	}
	// AND EVERY LANE THE PLAN NAMES A WAIT FOR, which the endpoints do not cover.
	//
	// Same blind spot the candidate query had, on the trigger side: an order whose
	// lane entry is interior to its plan touches a lane that neither endpoint
	// column names, so a terminal event for it would re-evaluate the wrong lanes
	// (or none) and leave whoever was waiting on that lane waiting for the sweep.
	//
	// The endpoints are KEPT rather than replaced: an order genuinely works its
	// source and destination lanes whether or not a wait was ever spliced for
	// them, and this function's job is "which lanes did this order touch", not
	// "which lane was it gated for".
	if order.StepsJSON != "" {
		var steps []resolvedStep
		if uErr := json.Unmarshal([]byte(order.StepsJSON), &steps); uErr == nil {
			for _, s := range steps {
				if s.Action == protocol.ActionWait && s.WaitKind == WaitKindLane {
					add(s.WaitLane)
				}
			}
		}
	}
	return out
}

// GateStagedCount reports how many orders are currently dwelling at a lane's wait
// point, for the admin surface's clear-the-mark confirmation.
//
// It answers from gateStagedForLane — the SAME derivation the evaluator releases
// from — rather than a count of its own. That matters more than the duplication
// it saves: a confirmation that says "3 robots are waiting" while the evaluator
// believes something else is worse than no confirmation, and the only way to be
// sure they agree is for there to be one answer. No new bookkeeping, by
// construction.
//
// A lane that is not gated has nobody dwelling at it, which is the honest zero
// rather than a special case.
func (d *Dispatcher) GateStagedCount(laneID int64) (int, error) {
	lane, err := d.db.GetNode(laneID)
	if err != nil {
		return 0, err
	}
	if lane == nil {
		return 0, nil
	}
	staged, err := d.gateStagedForLane(lane)
	if err != nil {
		return 0, err
	}
	return len(staged), nil
}
