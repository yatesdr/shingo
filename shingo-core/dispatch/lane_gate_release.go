package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// The lane-gate RELEASE EVALUATOR — Core as traffic cop.
//
// Increment 3 gave every lane-bound order on a gated group the
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
//   OCCUPANCY  the A′ predicate (stillComingToLane): a store blocks until it
//              PLACES, DERIVED from three facts something else already
//              maintains — not dispatched, standing at this lane's mark, or
//              inside the corridor. Nothing new is written.
//   APPEND     appendSegmentAndAdvance — the one fleet-append path, shared with
//              operator release, vehicle pinned.
//
// So the evaluator is a loop, an ordering, and two guards.
//
// WHY THE `others` SET IS STABLE ACROSS A PASS — AND IT NO LONGER DEPENDS ON A ROW.
//
// This used to say: a gate-staged PLAIN candidate holds its inbound mouth row
// from dispatch and keeps it until it places, so releasing one does not remove it
// from the blocker set. True of the plain arm, and FALSE of the complex arm —
// resolvePlanLaneHolds skipped the hold for a gated lane on purpose, so a
// gate-staged COMPLEX order held no mouth row while it dwelled, and the old
// predicate read "dispatched and no longer holding the mouth" as "it has placed".
// A dwelling complex candidate satisfied both halves and was filtered OUT of the
// blocker set while it was still very much coming. That was a live latent defect,
// invisible only because nothing dwelled.
//
// THE PARAGRAPH ALSO CARRIED THE INSTRUCTION FOR FIXING IT, AND THIS IS THAT FIX
// LANDING: "the sequencing token has to come from somewhere else first:
// IsGateStaged + WaitLane is already how this file derives its candidate set
// (gateStagedForLane), and is the obvious source. Do not defer the plain arm
// without moving that signal."
//
// Both halves landed together, in one commit, because either alone is broken.
// resolveOrderLaneHolds stopped taking the plain arm's DESTINATION hold on a
// gated lane — the row reserved a mouth for a slot the dweller had not chosen yet —
// and stillComingToLane no longer asks about rows at all. It keeps a candidate
// that is not dispatched, one standing at THIS lane's mark (IsGateStaged +
// WaitLane), or one holding lane occupancy. The mouth row was doing two jobs,
// corridor mutex and sequencing token; the mutex was always the OCCUPANCY row,
// and the token is now derived. Both arms read the same way, and the complex
// defect closes as a consequence rather than as a separate fix.
//
// The SOURCE hold is untouched and still held from dispatch: §R.101's lock is
// about a bin that has already been chosen in a lane, which no gate changes.
//
// What DOES change mid-pass is a release, which takes a slot and a lane's
// occupancy. That is why each candidate is evaluated against a freshly read
// view rather than one snapshot taken up front.
//
// ── C2c: THE GATE IS KEYED BY GROUP, AND THE DROP IS CHOSEN THERE ─────────
//
// The owner's rule (2026-10-08 §2; BRIEF-unification-C2c Lane C): the plan
// targets the group; the oracle chooses the drop across the whole node group at
// every release, by the group's store algorithm, hard rules first; the gate is
// keyed by group; room is a group count; the stop is the group's waiting spot.
// So a pass is a GROUP's (EvaluateGroupReleases, ruling 4), its waiters taken in
// DemandRank.Precedes order, and a store's drop is asked of the one chooser
// (plan.Planner.ChooseDrop, ruling 1) with every lane whose admission refused it
// set aside, until a lane admits it or the group has none (ruling 3). The
// widen arm and the in-lane re-bind it backed up are deleted: one release path
// (DESIGN-C2c §7.3).

// laneGateRetryQueueThreshold is how many consecutive append failures on one
// order are tolerated before the wait becomes operator-visible. Below it a
// failure is a transient fleet blip and the next firing retries silently; at it,
// the order carries a structured queue code so the floor can see a robot is
// holding at a gate for a fleet reason rather than a traffic one.
const laneGateRetryQueueThreshold = 3

// laneGateSerializer serializes evaluator passes per key: since C2c the GROUP
// (ruling 4; the valve and the pass take the same group key, D13 H1).
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
// reload-and-recheck inside the critical section (see releaseStoreCandidate), which is
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
	d.redriveHeldCompoundLegs(laneID)
}

// redriveHeldCompoundLegs is RedriveHeldCompoundLegs reporting the compounds
// THIS CALL advanced, for the floor (C2b F6): a parent whose advance handed a
// leg to the fleet. Not every parent it tried: a lost dispatch CAS is another
// caller's dispatch, not a floor release (order 263; DECISIONS D27 F2,
// TestF2Pin_TheFloorRecordsACompoundItAdvancedNotOneItTried).
func (d *Dispatcher) redriveHeldCompoundLegs(laneID int64) []int64 {
	parents, err := d.db.ListHeldLegParentsInLane(laneID)
	if err != nil {
		log.Printf("lanegate: list held legs in lane %d: %v", laneID, err)
		return nil
	}
	var advanced []int64
	for _, parentID := range parents {
		dispatched := false
		if err := d.advanceCompound(parentID, &dispatched); err != nil {
			log.Printf("lanegate: re-drive compound %d after lane %d cleared: %v", parentID, laneID, err)
		}
		if dispatched {
			advanced = append(advanced, parentID)
		}
	}
	return advanced
}

// EvaluateLaneReleases runs one release pass over the GROUP the lane stands in
// (C2c ruling 4: a waiter is keyed by its group). It is kept under its old name
// because a lane is what most triggers have in hand — a slot's lane, a wait's
// lane, a dig's lane — and every one of them now means "this group's room may
// have changed". EvaluateGroupReleases is the same pass for a caller that has
// the group.
//
// Idempotent and level-triggered — it derives everything from live state and
// nothing from the event that woke it. A firing that finds nothing to do is a
// no-op, a dropped event costs only latency (the next firing recovers), and a
// duplicate event is harmless. That is what lets the trigger set be generous.
//
// ── IT DOES NOT CONSULT THE LANE'S MARK, AND THAT IS THE ROLLBACK RULE ────
//
// There was an early return here for a lane with no mark, sold as "an
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
// candidate derivation below already asks (gate-staged, wait kind lane, its
// wait in this group). Deriving the set from durable order state and then
// gating the whole pass on live config was the one place those two disagreed.
//
// WHAT IT COSTS on an ungated plant: one ActiveGateCandidates query per
// lane-touching event, which returns nothing when no order carries a lane wait.
// Its sibling on the same events — RedriveHeldCompoundLegs — already runs a
// per-lane query with no mode gate at all, and for the same reason: the state
// it recovers is mode-independent.
func (d *Dispatcher) EvaluateLaneReleases(laneID int64) {
	d.evaluateLaneReleases(laneID)
}

// evaluateLaneReleases is EvaluateLaneReleases reporting the orders THIS CALL
// released. The floor reads it to record only what its own call changed (C2b
// F6).
func (d *Dispatcher) evaluateLaneReleases(laneID int64) []int64 {
	lane, err := d.db.GetNode(laneID)
	if err != nil || lane == nil || lane.ParentID == nil {
		return nil
	}
	return d.evaluateGroupReleases(*lane.ParentID)
}

// EvaluateGroupReleases runs one release pass over a node group's waiters: every
// gate-staged order waiting at one of the group's lanes (C2c ruling 4).
func (d *Dispatcher) EvaluateGroupReleases(groupID int64) {
	d.evaluateGroupReleases(groupID)
}

// evaluateGroupReleases is EvaluateGroupReleases reporting the orders THIS CALL
// released, the groups its appends left orders waiting at included (F1).
func (d *Dispatcher) evaluateGroupReleases(groupID int64) []int64 {
	if groupID == 0 {
		return nil
	}
	// THE PASS RUNS UNDER THE GROUP'S MUTEX; THE HEAL RUNS AFTER IT. Firing a dig
	// creates a compound, which dispatches its first leg synchronously, which emits
	// events, which the bus delivers ON THIS GOROUTINE to subscribers that call
	// back into this evaluator for this same group. The mutex is not reentrant, so
	// doing it inside the pass is a self-deadlock — the same shape the dissolve
	// site documents for scanMu (compound.go). Split rather than made reentrant: a
	// reentrant lock here would let a nested pass append a tail against state the
	// outer pass is halfway through changing, which is what the mutex is for.
	//
	// Outside it, the nested pass is harmless and bounded: it finds the dig lock
	// held, refuses everyone with lane-dig-active, and returns.
	res := d.evaluateGroupReleasesPass(groupID)
	// AND THE SAME SPLIT COVERS THE DWELL'S EXIT. A released dweller drives out of
	// its lane and drops its occupancy row inside the pass, where the fact becomes
	// true — but the re-drive that fact enables cannot run there: it dispatches,
	// which emits, which re-enters this function on the same goroutine for the same
	// group. So the wake happens here, outside the mutex, exactly like the heal.
	//
	// RedriveHeldCompoundLegs and not another evaluator pass: the population that
	// queues behind a dwelling dig leg is its own siblings, which are `pending` legs
	// the evaluator cannot see. Gate-staged waiters in this group were re-asked by
	// the pass itself — it re-reads per candidate, so the ones after the release
	// already saw the freed corridor.
	for _, laneID := range res.freed {
		// FLIP 2 RIDES THE SAME DEFERRAL. A released dweller driving out of the dug
		// lane can be the last thing that dig had in it, and the claim should go with
		// it — but releasing a lane wakes it, and waking it from inside the pass is
		// the self-deadlock above. Same split, same reason.
		d.maybeReleaseDigOnLastBlockerOut(laneID)
		d.RedriveHeldCompoundLegs(laneID)
	}
	if res.digWanted {
		d.summonOwnDigs(res.digLane, res.acceptance)
	}
	// AND THE LANES AN APPEND LEFT AN ORDER WAITING AT (F1), by the same split: a
	// robot sent on to its next wait is a waiter of that wait's group now, and the
	// event that would have woken it already happened. Evaluated once per group.
	done := map[int64]bool{}
	released := res.released
	for _, laneID := range res.next {
		g := d.groupOfLane(laneID)
		if g == 0 || done[g] {
			continue
		}
		done[g] = true
		released = append(released, d.evaluateGroupReleases(g)...)
	}
	return released
}

// groupOfLane is the group a lane stands in, or 0.
func (d *Dispatcher) groupOfLane(laneID int64) int64 {
	lane, err := d.db.GetNode(laneID)
	if err != nil || lane == nil || lane.ParentID == nil {
		return 0
	}
	return *lane.ParentID
}

// groupPass is what one group pass decided that has to happen after its mutex:
// the one heal (and the lane it digs), the lanes a dweller drove out of, the
// lanes an append left an order waiting at, and the orders it released.
type groupPass struct {
	acceptance acceptanceRequest
	digLane    *nodes.Node
	digWanted  bool
	freed      []int64
	next       []int64
	released   []int64
}

// evaluateGroupReleasesPass is the pass proper — everything that happens under
// the group's mutex.
//
// ONE HEAL PER PASS, not one per candidate, and the reason is that a dig is a
// fact about the LANE rather than about the order that noticed. The wall in
// front of a depth-2 store is the same wall in front of the depth-4 store behind
// it, so one excavation frees them all; a second request would find the lane
// locked by the first and do nothing. Returning one keeps that obvious instead of
// relying on the lock to absorb the duplicates.
//
// ── THE MUTEX IS THE GROUP'S (D13 H1) ─────────────────────────────────────
//
// One group's waiters are decided together, because they compete for one
// group's room: two passes for two lanes of a group could each choose the same
// sibling slot for a different waiter. The cost is that the RDS append runs under
// it, so the lanes of a large group serialize behind one append; D13 H1 accepts
// it and measures release latency in the pair.
func (d *Dispatcher) evaluateGroupReleasesPass(groupID int64) groupPass {
	unlock := d.laneGates.lock(groupID)
	defer unlock()

	var out groupPass
	candidates, lanes, err := d.gateStagedForGroup(groupID)
	if err != nil {
		log.Printf("lane gate: list staged orders for group %d: %v", groupID, err)
		return out
	}
	if len(candidates) == 0 {
		return out
	}

	// THE ONE COMPARATOR (C2c ruling 4): priority, then oldest, then id —
	// DemandRank.Precedes, the scan's own order. This used to be depth-first,
	// which bought "the deepest admissible order goes out on THIS pass rather than
	// the next firing" and nothing about safety: that is Tier 2's (a candidate
	// with a deeper un-placed cross-origin store in its lane is held whenever it
	// is evaluated; reversing the old sort left every gate scenario and the
	// 200-seed soak green, whereas disabling Tier-2 walls 160/200 seeds). Tier 1
	// and Tier 2 stay lane facts, asked after the choice, as refusals that send a
	// store to the group's next lane (DESIGN-C2c §7.4).
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].rank.Precedes(candidates[j].rank)
	})

	var group *nodes.Node // read once, on the first store candidate that needs it
	groupNode := func() *nodes.Node {
		if group == nil {
			if g, gErr := d.db.GetNode(groupID); gErr == nil {
				group = g
			}
		}
		return group
	}
	// THE PASS ASKS THE CHOOSER THROUGH ONE READ OF THE GROUP (store.GroupPassReads):
	// the group's slot state is read once per pass, and every waiter after the
	// first is answered from it at 0 reads (RULINGS-C2c-run-through §3.3).
	var reads *store.GroupPassReads

	// THE HEAL'S PROMPT. A refusal is the prompt and the physics is the question
	// (acceptanceDigNeeded reads it): a dig is summoned only for bins physically in
	// the corridor that nobody live is coming for. A store asks it only when its
	// group offered it no candidate at all (D13 F3, DESIGN-C2c §7.3.4): a wall in
	// front of the pencilled slot is not a reason to dig while a sibling lane has
	// room. A retrieve asks it on any refusal, as before — its lane is where its
	// bin is, which no choice changes.
	//
	// AND NOT OF A CANDIDATE CORE COULD NOT JUDGE. The classifier-error arm does
	// not propose: a heal is the largest action this evaluator can take, and
	// taking it because Core could not read the lane is deciding to dig on no
	// evidence. Fail-closed on an undetermined answer means doing LESS.
	propose := func(c gateCandidate, lane *nodes.Node, cause QueueCause) {
		if out.digWanted || cause == "" {
			return // one dig per pass; and an admitted candidate never reaches here
		}
		// ONE SUMMON AT A TIME PER DWELLER. The summon runs after this pass
		// releases the gate lock, so until it has written its chapter the
		// open-chapter skip below finds nothing, and a second pass decided the
		// same summon (order 95, before-edge2 2026-09-24: two legs for one wall).
		// The claim is taken here, under the lock, and summonOwnDigs gives it
		// back once the chapter is written or refused. In-process, no statement.
		if d.summonInFlight(c.order.ID) {
			d.dbg("lane gate: order %d already has a dig being summoned for it — not deciding it twice",
				c.order.ID)
			return
		}
		if req, ok := d.acceptanceDigNeeded(lane, c); ok && d.beginSummon(req.order.ID) {
			out.acceptance, out.digLane, out.digWanted = req, lane, true
		}
	}
	released := 0
	for _, c := range candidates {
		// ── NOTHING IS APPENDED MID-EXCAVATION (§R.104a's ordering) ───────
		//
		// A candidate with an open dig chapter is one of these robots standing at
		// its mark while its OWN children work the lane. The pass must not release
		// it: the law is lock → dig chapter → append, and appending here would send
		// the robot in behind its own diggers. Its releaser is the chapter closing,
		// which appends the tail on the completion path.
		//
		// It also keeps such a candidate out of every downstream proposal on this
		// pass, which is the audit's item 10 — a digging dweller must never read as
		// a candidate for another excavation of the lane it is already excavating.
		if d.hasOpenDigChapter(c.order.ID) {
			d.dbg("lane gate: order %d is digging %s open with its own children — not released, and "+
				"not a candidate for anything else, until its chapter closes", c.order.ID, c.lane.Name)
			continue
		}
		if c.dwell {
			freedD, next := d.releaseDweller(c, c.lane)
			if freedD {
				released++
				out.freed = append(out.freed, c.lane.ID)
				out.released = append(out.released, c.order.ID)
			}
			// The dweller's tail may enter a marked lane: its wait there is that
			// group's event (F1), evaluated after this group's mutex.
			if next != 0 {
				out.next = append(out.next, next)
			}
			continue
		}
		var (
			in, failed bool
			next       int64
		)
		if c.retrieve {
			in, next, failed = d.releaseRetrieveCandidate(c, propose)
		} else {
			g := groupNode()
			if g == nil {
				continue
			}
			if reads == nil {
				reads = d.db.GroupPassReads(groupID)
			}
			in, next, failed = d.releaseStoreCandidate(c, g, lanes, propose, 0, reads, "lane gate release")
		}
		// AN APPEND THAT LEFT THE ORDER AT ANOTHER WAIT is that wait's group's
		// event (F1), evaluated after the unlock (P0 ruling Q5).
		if next != 0 {
			out.next = append(out.next, next)
		}
		if failed || !in {
			continue
		}
		released++
		out.released = append(out.released, c.order.ID)
	}

	if released > 0 {
		log.Printf("lane gate: released %d order(s) into group %d", released, groupID)
	}
	return out
}

// releaseRetrieveCandidate is a gate-staged RETRIEVE's turn in its group's pass:
// its lane is the lane its bin stands in, which no choice changes, so it asks
// its own lane's admission and nothing else — the per-lane evaluator's arm,
// unchanged by C2c (DESIGN-C2c §7.3.3).
func (d *Dispatcher) releaseRetrieveCandidate(c gateCandidate,
	propose func(gateCandidate, *nodes.Node, QueueCause)) (in bool, next int64, failed bool) {
	v, cErr := d.gateEntryVerdict(c.lane, c.order, c.node, true)
	if cErr != nil {
		// AND THE CAUSE GOES ON THE ROW, exactly as the refusal arm below does it:
		// a robot dwelling because Core could not READ the lane must not read like
		// one nobody has evaluated. CauseAdmissionError, not a lane cause: an
		// undetermined answer is Core declining, not a busy lane (compound.go's
		// admission-error arm, same constant). It stays a candidate — the set is
		// derived from durable order state, never from a verdict.
		log.Printf("lane gate: classifier error for order %d on lane %s: %v", c.order.ID, c.lane.Name, cErr)
		d.setQueueReason(c.order, protocol.QueueWaitingForSlot, CauseAdmissionError,
			QueueParams{Lane: c.lane.Name})
		return false, 0, false
	}
	if !v.Admitted() {
		// STAYS A CANDIDATE, AND IT SAYS WHY ON THE ROW: the same code and cause
		// vocabulary as the pre-dispatch park, so a lane-occupied wait reads the
		// same whether the robot is parked before dispatch or at a mark.
		d.recordLaneWait(c.order, c.lane, v)
		d.dbg("lane gate: order %d still held at %s (%s)", c.order.ID, c.lane.Name, v.Cause())
		propose(c, c.lane, v.Cause())
		return false, 0, false
	}
	in, next, failed = d.releaseAdmittedCandidate(c, c.lane)
	if failed {
		// A FAILED RELEASE IS NOT A WALL — the classifier admitted it — but the
		// physics is still the question, so it is still a prompt.
		propose(c, c.lane, CauseGateReleaseFailed)
	}
	return in, next, failed
}

// wakeLanesOfSlots evaluates the group of each slot an order just stopped
// holding (C2c ruling 4: a hold dropped is an event that changes the group's
// room — DESIGN-C2c §7.2's wakeGroupsOfSlots, kept under its old name for its
// two callers).
//
// ── A SLOT ROW GIVEN UP IS THE OTHER END OF lane-deeper-pending ───────────
//
// A never-dispatched deeper store walls a lane only through the slot row it
// holds there (stillComingToLane: a parked order that only names a slot is a
// plan, not a presence). Placing its bin is one way the wall ends; giving up the
// row is the other: a re-made choice that moved (the allocator's reconcile) or a
// park that releases its holdings. Nothing else wakes the group, and its dweller
// waited for the floor: packed 477, behind order 481 whose drop was re-chosen
// into another lane. Never under a gate mutex: both callers run in the scanner's
// acquisition, before the valve or the evaluator takes one. One pass per group.
func (d *Dispatcher) wakeLanesOfSlots(nodeIDs ...int64) {
	woken := map[int64]bool{}
	for _, id := range nodeIDs {
		g := d.groupOfNode(id)
		if g == 0 || woken[g] {
			continue
		}
		woken[g] = true
		d.EvaluateGroupReleases(g)
	}
}

// errOwnDigNotACandidate: the order whose own dig just closed is not a gate
// candidate at the lane it waits for, so there is nothing to release it as.
var errOwnDigNotACandidate = errors.New("not a gate candidate at its lane")

// releaseAfterOwnDig is the own-dig resume's release: the robot that summoned the
// dig stands at its mark, and its tail goes out through the gate's own release —
// a store's drop chosen by the one chooser (releaseStoreCandidate, C2c ruling 1:
// "every release"), a retrieve's pickup re-bound (releaseGatedRetrieve) — then
// appended. One spelling of "a tail is re-asked before it is sent". The dig
// having opened its lane for this order is that lane's admission; another lane
// of the group the chooser answers is asked as at any release.
//
// nil when it went in. ErrAppendNotOwed when another pass already appended it.
// errOwnDigNotACandidate when the evaluator cannot see it as a candidate. Any
// other error is a release that failed, whose cause is already on the row.
func (d *Dispatcher) releaseAfterOwnDig(order *orders.Order) error {
	fresh, err := d.db.GetOrder(order.ID)
	if err != nil || fresh == nil {
		return fmt.Errorf("own-dig release: reload order %d: %v", order.ID, err)
	}
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(fresh.StepsJSON), &steps); err != nil {
		return fmt.Errorf("own-dig release: parse steps for order %d: %w", order.ID, err)
	}
	w, ok := waitAt(steps, fresh.WaitIndex)
	if !ok || w.WaitKind != WaitKindLane {
		return fmt.Errorf("%w: order %d waits at no lane", errOwnDigNotACandidate, order.ID)
	}
	groupID := w.WaitGroup
	if groupID == 0 {
		groupID = d.groupOfLane(w.WaitLane)
	}
	cands, lanes, err := d.gateStagedForGroup(groupID)
	if err != nil {
		return fmt.Errorf("own-dig release: candidates at group %d: %w", groupID, err)
	}
	for _, c := range cands {
		if c.order.ID != order.ID {
			continue
		}
		var in, failed bool
		if c.retrieve {
			in, _, failed = d.releaseAdmittedCandidate(c, c.lane)
		} else {
			group, gErr := d.db.GetNode(groupID)
			if gErr != nil || group == nil {
				return fmt.Errorf("own-dig release: group %d for order %d: %v", groupID, order.ID, gErr)
			}
			in, _, failed = d.releaseStoreCandidate(c, group, lanes, nil, c.lane.ID, nil, "lane gate release")
		}
		switch {
		case in:
			return nil
		case failed:
			return fmt.Errorf("own-dig release: order %d was not released from lane %s", order.ID, c.lane.Name)
		}
		return ErrAppendNotOwed
	}
	return fmt.Errorf("%w: order %d at lane %d", errOwnDigNotACandidate, order.ID, w.WaitLane)
}

// releaseAdmittedCandidate releases c, a RETRIEVE the classifier admitted, into
// lane: its pickup re-bound to where its bin sits now, then the tail. A store's
// release is releaseStoreCandidate's, which chooses the drop across the group
// (C2c ruling 1). It returns whether the order went in, the lane its append left
// it waiting at (0 for none), and whether the release failed — the pass proposes
// on a failure and skips quietly on an append that was not owed.
func (d *Dispatcher) releaseAdmittedCandidate(c gateCandidate, lane *nodes.Node) (in bool, next int64, failed bool) {
	next, rErr := d.releaseGatedRetrieve(c.order, lane, c.entryIndex)
	if errors.Is(rErr, ErrAppendNotOwed) {
		// The leg ended while it waited (or another pass appended it). It owes
		// the lane nothing: no wait is written on it, nothing is proposed, and
		// the next candidate behind it is still asked.
		return false, next, false
	}
	if rErr != nil {
		// AND THE CAUSE GOES ON THE ROW. This arm wrote nothing, and it is the
		// worst of the three places to write nothing: it runs AFTER the verdict
		// admitted, so the order has just had any previous cause cleared on the
		// success path's assumption — it loses the old sentence and gains none.
		// A dweller whose release keeps failing then reads exactly like one
		// nobody has evaluated yet. Measured on the lane-stress rig 2026-08-10:
		// orders 6 and 40 dwelling 13-16 minutes under "no empty slot in lane"
		// with a blank row, surfaced by the floor as a blank-cause release.
		//
		// It stays a candidate — the set is durable order state, never a verdict
		// — so the next pass retries, which is what the cause's releaser says.
		log.Printf("lane gate: release order %d into lane %s: %v", c.order.ID, lane.Name, rErr)
		d.setQueueReason(c.order, protocol.QueueWaitingForSlot, CauseGateReleaseFailed,
			QueueParams{Lane: lane.Name})
		return false, next, true
	}
	// CLEARED ON ENTRY. The cause described a wait that is over; leaving it
	// would put a stale "waiting for a slot" on a robot that is now driving,
	// which is the same lie as the blank was, told the other way round.
	d.setQueueReason(c.order, "", "", QueueParams{})
	return true, next, false
}

// gateCandidate is one gate-staged order with what the ordering and the
// classifier need.
type gateCandidate struct {
	order *orders.Order
	// node is the LANE-RELEVANT node: the slot the order's next step works —
	// its dropoff target for a store, its pickup slot for a retrieve. It was
	// called `dest` when two queries produced it and the retrieve query had to
	// explain that it carried the source there instead. For a gated group drop it
	// is the pencil: the slot its paper sits on, which the release re-chooses.
	node *nodes.Node
	// lane is the lane the wait names (WaitLane): the retrieve's lane, the
	// dweller's, the pencilled lane of a store.
	lane *nodes.Node
	// rank is the demand the order presents: the group pass's one comparator
	// (DemandRank.Precedes, C2c ruling 4).
	rank orders.DemandRank
	// retrieve is the direction, read off the plan (a pickup after the wait)
	// rather than inferred from which query found the order.
	retrieve bool
	// dwell marks the OUTBOUND candidate: a dig leg standing INSIDE this lane
	// holding a blocker, waiting for Core to choose where it goes. It gates an
	// APPEND rather than an entry, so it takes neither of the two release paths
	// below and asks none of the entry questions — the robot is already in.
	//
	// Mutually exclusive with `retrieve` by construction: the direction walk only
	// runs when there IS a step after the wait, and a dweller is defined by there
	// not being one (waitGatesAnAppend).
	dwell bool
	// entryIndex is the steps_json index of THAT lane-entry step — the same walk
	// that produced `node` and `retrieve` already knew it. The release needs it to
	// patch the step it is actually speaking for: a swap has two dropoffs and a
	// spliced plan two pickups, so "the last dropoff" and "the first pickup" name
	// the wrong leg (see applyPlanNode).
	//
	// -1 for a dweller, and nothing reads it: there is no entry step to patch,
	// which is the whole of what makes it a dweller. The release-time resolver
	// APPENDS its step rather than re-binding one.
	entryIndex int
}

// waitingAtLane is the one answer to "is this order waiting at a lane wait
// now", read by everything that releases or counts lane waiters: the evaluator
// (gateStagedForLane), the periodic lane check (laneWaiters) and the group
// fan-out (DwellerLanesSharingGroupWith). It returns the order's plan and the
// wait it is parked at.
//
// Gate-staged and not faulted, as before, and one thing more: a dig leg is not
// waiting until it has lifted its blocker (digLegNotYetLifted). Two readers
// with two answers found two different sets, and the event path released a dig
// leg before its lift that the periodic check had learned to leave alone.
func waitingAtLane(c *orders.GateCandidate) ([]resolvedStep, resolvedStep, bool) {
	o := c.Order
	if !IsGateStaged(o) || o.Status == StatusFaulted {
		return nil, resolvedStep{}, false
	}
	var steps []resolvedStep
	if json.Unmarshal([]byte(o.StepsJSON), &steps) != nil {
		// IsGateStaged already parsed this and logged; it cannot be true here
		// on an unparseable plan. Defensive only.
		return nil, resolvedStep{}, false
	}
	w, ok := waitAt(steps, o.WaitIndex)
	if !ok || w.WaitKind != WaitKindLane || w.WaitLane == 0 {
		return nil, resolvedStep{}, false
	}
	if digLegNotYetLifted(c, steps) {
		return nil, resolvedStep{}, false
	}
	return steps, w, true
}

// digLegNotYetLifted reports whether c is a dig leg whose blocker is still in
// the slot it is to be lifted from.
//
// A dig leg's plan is parked at its lane wait from dispatch (IsGateStaged reads
// the plan, not the robot), but until the lift its robot is driving to the
// blocker, and the lift is that leg's releaser: the bin goes to the transit
// node, the lane is woken, and Core chooses where the blocker goes and appends
// the tail. Released before the lift, the leg had its destination bound and its
// tail appended while the robot did not yet have the bin.
//
// The lift's own durable fact answers it, no new column: a lifted blocker has
// left its slot (handlePickupBlockCompleted). The bin's node rides on the
// candidate row, so this reads nothing. A leg with no bin, or a bin on no node,
// answers false and is kept, as it always has been.
func digLegNotYetLifted(c *orders.GateCandidate, steps []resolvedStep) bool {
	o := c.Order
	if o.ParentOrderID == nil || o.BinID == nil || o.SourceNode == "" {
		return false
	}
	if !waitGatesAnAppend(steps, o.WaitIndex) {
		return false
	}
	return c.BinNode == o.SourceNode
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
// stagedAtMarkOnLane is the set of orders parked at laneID's MARK — the rows a
// dig may ignore, because their robots are standing outside the corridor.
//
// ── IT IS NOT gateStagedForLane, AND THE DIFFERENCE IS THE POINT ──────────
//
// The two ask different questions off the same rows. gateStagedForLane asks who
// is RELEASABLE, so it goes on to resolve each candidate's entry step and drops
// anyone whose tail cannot be built (a mis-spliced plan, an unresolvable wait
// node). This asks who is STANDING OUTSIDE, and an order whose tail cannot be
// built is standing outside exactly like one whose tail can. Forcing the two
// sets to be equal would mean a robot at a mark started refusing excavations the
// moment its own plan went unreadable, which is the wrong direction on both
// counts. They share the primitives — IsGateStaged, waitAt, WaitLane — so the
// definition of "at this lane's mark" has one spelling; only the follow-on
// differs.
//
// FAULTED IS EXCLUDED, matching gateStagedForLane. A faulted leg is mid-recovery
// and Core does not know where its robot will be when the recovery resolves, so
// its row keeps refusing — over-restrictive, never under, which is the direction
// TakeLaneOccupancy already argues for by name.
//
// ON A READ ERROR IT RETURNS NOTHING, which exempts nobody and leaves the
// refusal exactly as it is today. A dig that cannot be admitted retries on the
// next lane event; a dig admitted past a robot that turns out to be in the
// corridor does not have a next anything.
//
// COST: one ActiveGateCandidates scan per dig-lock decision. Digs are rare
// (35 hold events in the whole gated sim run of 2026-08-30) and this is not on
// the per-order dispatch path. It is deliberately NOT called from
// shuffleSlotsFrom's candidate loop, which would make it per-candidate-lane.
func stagedAtMarkOnLane(db *store.DB, laneID int64) reservations.StagedOutside {
	return stagedAtMarkByLane(db, laneID).On(laneID)
}

// stagedAtMarkByLane answers, for each named lane, which orders are standing at
// its GROUP's wait points. stagedAtMarkOnLane is the single-lane spelling of it.
//
// ── THE MEMBERSHIP IS GROUP-SCOPED, AND THAT IS THE WHOLE POINT ───────────
//
// A wait point is staging for the node group, not a doorway onto the lane whose
// name it happens to carry (reservations.StagedOutside says why). A robot at
// "Lane_01-WAIT" has not entered a corridor and has not been committed to one —
// the oracle can still send it to lane 10 — so it obstructs an excavation in
// NONE of that group's lanes, and it is exempt on all of them.
//
// Matching on w.WaitLane == laneID instead would be reading the paint on the
// floor rather than the geometry: it would go on refusing a dig on lane 10 for
// a robot that is standing in the same staging area, doing nothing, several
// lanes away.
//
// ONE scan, bucketed. The alternative is a scan per lane, and a multi-lane
// acquire would pay for each. Only the named lanes get a bucket, and each is
// resolved against ITS OWN group — which is what keeps a plan spanning two
// groups from exempting an order across the boundary.
func stagedAtMarkByLane(db *store.DB, laneIDs ...int64) reservations.StagedOutsideByLane {
	if len(laneIDs) == 0 {
		return nil
	}
	// groupOfLane, and the reverse index the match actually uses: for each lane
	// asked about, every lane sharing its group. A dweller's wait names SOME lane
	// in the group; the question is whether that lane and the one being dug are
	// siblings.
	siblings := make(map[int64]map[int64]bool, len(laneIDs)) // laneID -> set of lanes in its group
	for _, id := range laneIDs {
		if _, done := siblings[id]; done {
			continue
		}
		lane, err := db.GetNode(id)
		if err != nil || lane == nil || lane.ParentID == nil {
			// Unreadable, or a lane with no group. Exempt nobody for it: a doubt
			// here must refuse the dig, never wave it through.
			siblings[id] = nil
			continue
		}
		kin, kErr := db.ListChildNodes(*lane.ParentID)
		if kErr != nil {
			log.Printf("lane gate: could not list the group of lane %d (%v) — exempting nobody there, "+
				"so a dig-mode acquire is refused on any ordinary row exactly as before", id, kErr)
			siblings[id] = nil
			continue
		}
		set := make(map[int64]bool, len(kin))
		for _, k := range kin {
			set[k.ID] = true
		}
		siblings[id] = set
	}

	active, err := db.ActiveGateCandidates()
	if err != nil {
		log.Printf("lane gate: could not read who is standing at the marks for %v (%v) — exempting "+
			"nobody, so a dig-mode acquire is refused on any ordinary row exactly as before",
			laneIDs, err)
		return nil
	}
	out := reservations.StagedOutsideByLane{}
	for _, c := range active {
		// ONE SPELLING OF "STANDING OUTSIDE" (C2b A.4): waitingAtLane's, which
		// leaves out a dig leg that has not lifted its blocker — its robot is
		// driving INSIDE the lane to it, whatever its plan's wait index says.
		_, w, ok := waitingAtLane(c)
		if !ok {
			continue
		}
		o := c.Order
		for laneID, kin := range siblings {
			if kin == nil || !kin[w.WaitLane] {
				continue // staged in a different group, or this lane is unreadable
			}
			set := out[laneID]
			if set == nil {
				set = reservations.StagedOutside{}
				out[laneID] = set
			}
			set[o.ID] = true
		}
	}
	return out
}

func (d *Dispatcher) gateStagedForLane(lane *nodes.Node) ([]gateCandidate, error) {
	active, err := d.db.ActiveGateCandidates()
	if err != nil {
		return nil, err
	}
	var out []gateCandidate
	for _, c := range active {
		steps, w, ok := waitingAtLane(c)
		if !ok || w.WaitLane != lane.ID {
			continue // not waiting at a lane yet, or at somebody else's
		}
		cand, ok, cErr := d.gateCandidateAt(c, steps, w, lane)
		if cErr != nil {
			return nil, cErr
		}
		if ok {
			out = append(out, cand)
		}
	}
	return out, nil
}

// gateCandidateAt builds one waiter's candidate against the lane its wait names:
// the per-lane derivation's body, shared with the group's (gateStagedForGroup)
// so the two cannot disagree about who is releasable. ok=false drops a waiter
// whose tail cannot be built, loudly.
func (d *Dispatcher) gateCandidateAt(c *orders.GateCandidate, steps []resolvedStep, w resolvedStep,
	lane *nodes.Node) (gateCandidate, bool, error) {
	o := c.Order
	entry, entryIdx, isRetrieve, ok := laneEntryAfterWait(steps, o.WaitIndex)
	if !ok {
		// THE OUTBOUND ARM. A lane wait with nothing actionable after it is not
		// a broken plan — it is a dig leg dwelling in the lane it is digging,
		// whose next step is exactly what Core has not chosen yet. Its tail
		// cannot be BUILT from the plan; it is APPENDED to it by the release-time
		// resolver, which is what this candidate is for.
		//
		// The old sentence stays for the case it was written about, and it is a
		// different one: a plan whose wait is neither a dweller's nor followed by
		// its entry is a mis-splice, and it is still loud rather than skipped
		// quietly, because the order would otherwise dwell with no diagnosis.
		if !waitGatesAnAppend(steps, o.WaitIndex) {
			log.Printf("lane gate: order %d is parked at a wait for lane %s with no actionable step "+
				"after it and no destination to choose — its tail cannot be built", o.ID, lane.Name)
			return gateCandidate{}, false, nil
		}
		// The node the dweller STANDS IN, which is the lane-relevant one for it:
		// the wait's own node. Resolving it is also the assertion that §3's
		// identity holds on the live row — a dweller must be inside the lane it
		// names, or the wait is not what it says it is.
		at, aErr := d.db.GetNodeByDotName(w.Node)
		if aErr != nil || at == nil {
			log.Printf("lane gate: dwelling leg %d waits at %q, which does not resolve: %v",
				o.ID, w.Node, aErr)
			return gateCandidate{}, false, nil
		}
		if at.ParentID == nil || *at.ParentID != lane.ID {
			log.Printf("lane gate: dwelling leg %d names lane %s but stands at %q (parent %q) — "+
				"mis-spliced plan, refusing to release it here", o.ID, lane.Name, w.Node, at.ParentName)
			return gateCandidate{}, false, nil
		}
		return gateCandidate{order: o, node: at, lane: lane, rank: c.Rank, dwell: true, entryIndex: -1}, true, nil
	}
	node, nErr := d.db.GetNodeByDotName(entry.Node)
	if nErr != nil || node == nil {
		log.Printf("lane gate: order %d's lane entry %q does not resolve: %v", o.ID, entry.Node, nErr)
		return gateCandidate{}, false, nil
	}
	// The wait says which lane owns it; this checks the PLAN agrees. A
	// disagreement is a mis-spliced plan, and it is loud rather than skipped
	// quietly, because the order would otherwise dwell with no diagnosis. A lane
	// slot's lane is its parent, read off the row already in hand.
	if node.ParentID == nil || *node.ParentID != lane.ID {
		log.Printf("lane gate: order %d waits for lane %s but its next step %q is in %q — "+
			"mis-spliced plan, refusing to release it here", o.ID, lane.Name, entry.Node, node.ParentName)
		return gateCandidate{}, false, nil
	}
	return gateCandidate{order: o, node: node, lane: lane, rank: c.Rank, retrieve: isRetrieve, entryIndex: entryIdx},
		true, nil
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
// It also returns the entry step's INDEX into the full step list. That index is
// the key to order_bins.step_index, which is how the gate finds out WHICH BIN the
// entry is for on an order that has several — the walk already computed it and
// used to throw it away, and a second walk to recover it would be two spellings
// of one traversal.
func laneEntryAfterWait(steps []resolvedStep, waitIndex int) (resolvedStep, int, bool, bool) {
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
		return resolvedStep{}, -1, false, false
	}
	for off, s := range steps[start:] {
		switch s.Action {
		case protocol.ActionPickup:
			return s, start + off, true, true
		case protocol.ActionDropoff:
			return s, start + off, false, true
		}
	}
	return resolvedStep{}, -1, false, false
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

	// WHICH BIN, ASKED PER STEP. It used to read order.BinID directly, which is
	// one column with two meanings — the source bin for a plain retrieve, and
	// "the bin claimed at the process node" for a complex order. A swap parked at
	// a lane's mark to fetch a FRESH bin was therefore checked against the bin at
	// the MACHINE, found elsewhere (correctly — that is where it belongs), and
	// refused entry forever. See binForStep.
	// AN UNREADABLE JUNCTION IS NOT AN ANSWER, so it leaves here as a plain
	// error rather than as ErrPickupNotInLane. The distinction is the whole
	// point of the sentinel: "the bin is somewhere else" is a fact about the
	// plant that the gate refuses on, with a cause and a releaser; "I could not
	// tell" is Core declining, and admitLane's error arm turns it into
	// CauseAdmissionError — a candidate held as undetermined, re-asked next pass.
	// Collapsing the two would report a database outage as a bin that moved.
	want, err := d.wantedBin(order)
	if err != nil {
		return nil, false, err
	}
	switch {
	case !want.known:
		if named == nil {
			return nil, false, fmt.Errorf("pickup slot: source node %q not found", order.SourceNode)
		}
		return named, false, nil
	case want.atNode != "":
		// A RELAY — this order dropped the bin at that node itself, so the node is
		// the answer and there is nothing to locate. It still has to be IN this
		// lane for the gate to bind it.
		at, rErr := d.db.GetNodeByDotName(want.atNode)
		if rErr != nil {
			return nil, false, rErr
		}
		if at == nil || at.ParentID == nil || *at.ParentID != lane.ID {
			return nil, false, fmt.Errorf("%w: relay pickup for order %d is at %s, outside lane %s",
				ErrPickupNotInLane, order.ID, want.atNode, lane.Name)
		}
		return at, at.ID != namedID(named), nil
	}

	bin, err := d.db.GetBin(want.binID)
	if err != nil {
		return nil, false, err
	}
	if bin == nil || bin.NodeID == nil {
		return nil, false, fmt.Errorf("pickup slot: bin %d for order %d is gone or has no node",
			want.binID, order.ID)
	}
	if named != nil && *bin.NodeID == named.ID {
		return named, false, nil // the common case: nothing moved, nothing to write
	}
	at, err := d.db.GetNode(*bin.NodeID)
	if err != nil {
		return nil, false, err
	}
	if at == nil || at.ParentID == nil || *at.ParentID != lane.ID {
		// A DEFINITE ANSWER, NOT AN UNREADABLE ONE — hence the sentinel. The bin
		// is somewhere and it is not here; that is a fact the caller can refuse
		// on, with a cause and a releaser. Returning it as a bare error made it
		// indistinguishable from a failed read, so the gate treated a knowable
		// "no" as "I could not tell" and wedged instead of waiting.
		return nil, false, fmt.Errorf("%w: bin %d for order %d is at %s, outside lane %s",
			ErrPickupNotInLane, want.binID, order.ID, nodeName(at), lane.Name)
	}
	return at, true, nil
}

// namedID is nil-safe access to the order's remembered source node id, for the
// `moved` flag. A nil named node means the order never had one to move from.
func namedID(named *nodes.Node) int64 {
	if named == nil {
		return 0
	}
	return named.ID
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

// releaseGatedRetrieve is the retrieve's release (a store's is releaseStoreCandidate): re-bind the
// pickup against where the bin actually sits NOW, append the [pickup, dropoff]
// tail, and seal. Same double-append guard (re-read + IsGateStaged re-check under
// the per-lane mutex) — wait_index is the witness either way.
//
// The pickup re-bind is the retrieve's reason to bind at release: a dig may have
// moved the wanted bin while this order dwelled at the gate, so the slot the order
// was born wanting is not necessarily the slot the bin occupies when the lane opens.
func (d *Dispatcher) releaseGatedRetrieve(order *orders.Order, lane *nodes.Node, entryIndex int) (int64, error) {
	fresh, err := d.db.GetOrder(order.ID)
	if err != nil || fresh == nil {
		return 0, err
	}
	if !IsGateStaged(fresh) {
		d.dbg("lane gate: retrieve %d no longer gate-staged (wait_index=%d status=%s) — another pass released it",
			fresh.ID, fresh.WaitIndex, fresh.Status)
		return 0, ErrAppendNotOwed
	}

	src, cause, err := d.rebindGatedPickup(fresh, lane, entryIndex)
	if err != nil {
		// The bin moved (a dig relocated it) and its current location is not the lane
		// slot the order holds, or the slot is no longer reachable. Refusing is safe:
		// appending a pickup against a slot the bin left would pick the wrong bin (or
		// none). The order stays staged; the dig/reshuffle machinery updates the bin's
		// location and the next firing re-tries once the lane is consistent.
		d.setQueueReason(fresh, protocol.QueueWaitingForSlot, cause,
			QueueParams{Destination: lane.Name})
		return 0, err
	}

	if err := d.appendGateTail(fresh, "lane gate release (retrieve)"); err != nil {
		if !errors.Is(err, ErrAppendNotOwed) {
			d.noteGateAppendFailure(fresh, lane)
		}
		return 0, err
	}
	d.clearGateAppendFailures(fresh.ID)
	d.setQueueReason(fresh, "", "", QueueParams{})
	d.dbg("lane gate: retrieve %d released into lane %s from %s", fresh.ID, lane.Name, src.Name)
	return laneWaitAfterAppend(fresh), nil
}

// rebindGatedPickup re-resolves the order's pickup against where its bin ACTUALLY
// SITS at the moment of append. Returns the node the tail will pick from.
//
// The pickup's re-ask at release (a drop's is the group chooser), with three outcomes:
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
func (d *Dispatcher) rebindGatedPickup(order *orders.Order, lane *nodes.Node, entryIndex int) (*nodes.Node, QueueCause, error) {
	at, moved, err := d.pickupSlotNow(order, lane)
	if err != nil {
		return nil, causeForGateRebind(err), err
	}
	if !moved {
		// Unchanged. The emptiness check survives for the bin_id-less fallback, where
		// pickupSlotNow can only report the slot the order names and cannot tell
		// whether anything is still in it — refusing beats picking nothing.
		bins, bErr := d.db.ListBinsByNode(at.ID)
		if bErr != nil {
			return nil, causeForGateRebind(bErr), bErr
		}
		if len(bins) == 0 {
			return nil, CauseGateRebindUnavailable,
				fmt.Errorf("rebind pickup: source slot %s is empty — the bin moved; refusing to release", at.Name)
		}
		return at, "", nil
	}

	// applySourceNodeAtStep, not a raw source_node write: it patches the deferred
	// pickup step in steps_json in the same breath, and that tail is exactly what the
	// append is about to emit. A raw write would send the robot to the slot the bin
	// left. The index is THIS order's lane entry, carried from the candidate walk —
	// a spliced plan has an earlier pickup that belongs to another leg.
	was := order.SourceNode
	if err := applySourceNodeAtStep(d.db, order, at.Name, entryIndex); err != nil {
		return nil, causeForGateRebind(err), err
	}
	log.Printf("lane gate: retrieve %d re-bound at release %s → %s (lane %s, bin %d)",
		order.ID, was, at.Name, lane.Name, *order.BinID)
	return at, "", nil
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
	// delivery_node is the order's FINAL destination: since C2a's A.5 the gate's
	// re-bind no longer writes a swap's interior lane slot there. An interior lane
	// is found through its wait step below, which is where it always was.
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

// NodeIDsForOrder returns the nodes an order was working — its source and its
// destination — for the triggers that care about a NODE freeing rather than a
// lane clearing.
//
// The distinction is the shuffle pool. A lane trigger asks "may somebody enter
// this corridor now"; a node trigger asks "is there somewhere to put a bin", and
// the answer to the second changes when an order that was inbound to a node dies
// without ever getting there — no bin moves, no lane clears, and a slot the
// capacity gate was counting as spoken for becomes free.
//
// ── AND THE LANES ITS PLAN WAS WAITING ON ────────────────────────────────
//
// Endpoints alone are not everything an order was working. A gate-staged order's
// own terminal event is exactly the moment its lane becomes interesting to
// everybody else standing outside it — and its WaitLane is not necessarily
// either endpoint: a spliced plan can wait on a lane it picks from on the way to
// a destination somewhere else entirely. So a dweller that failed or was
// cancelled woke the lanes at the ends of its plan and NOT the lane its robot
// was parked outside, which is the one whose queue just got shorter.
//
// The wait steps carry the lane id directly (WaitLane), so this is a walk over
// the plan rather than a second resolution: no names, no lookups, nothing that
// can fail. Same class of gap as the LaneIDsForOrder fix already in the tree.
//
// Names, not ids, are what the row carries for the endpoints, so those are
// resolved; an unresolvable name contributes nothing rather than a zero.
func (d *Dispatcher) NodeIDsForOrder(orderID int64) []int64 {
	order, err := d.db.GetOrder(orderID)
	if err != nil || order == nil {
		return nil
	}
	seen := map[int64]bool{}
	var out []int64
	add := func(id int64) {
		if id == 0 || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	// As in LaneIDsForOrder: the endpoints are the order's first pickup and FINAL
	// drop (an interior lane drop has not been in delivery_node since C2a's A.5);
	// the interior lane arrives through its wait step's WaitLane below.
	for _, name := range []string{order.DeliveryNode, order.SourceNode} {
		if name == "" {
			continue
		}
		node, nErr := d.db.GetNodeByDotName(name)
		if nErr != nil || node == nil {
			continue
		}
		add(node.ID)
	}
	var steps []resolvedStep
	if json.Unmarshal([]byte(order.StepsJSON), &steps) == nil {
		for _, s := range steps {
			if s.Action == protocol.ActionWait && s.WaitKind == WaitKindLane {
				add(s.WaitLane)
			}
		}
	}
	return out
}

// EvaluateWaitLaneForStagedOrder re-asks the gate question for the lane an order
// has just PARKED at — the arrival trigger.
//
// ── WHY ARRIVAL IS AN EVENT AND NOT A NON-EVENT ───────────────────────────
//
// Every other trigger in the set is something changing in a LANE. This one is
// the robot itself becoming ready, and it is the trigger the outbound dwell
// needs: the whole value of the dwell is that the destination is chosen when the
// robot is standing ready to drive rather than when its leg was planned, so the
// moment it reports staged is the moment there is a question to answer. Without
// it a dweller waits for unrelated traffic or for the 60-second floor, and the
// open-destination case — which should be a pause the robot barely notices —
// costs an interval.
//
// It is not dwell-only, and deliberately so: an INBOUND order that arrives at a
// mark while its lane is contended is re-asked here too. That firing usually
// answers "still no", which is what a level-triggered idempotent evaluator is
// for, and it costs one query on a path that has just done a fleet round trip.
func (d *Dispatcher) EvaluateWaitLaneForStagedOrder(orderID int64) {
	order, err := d.db.GetOrder(orderID)
	if err != nil || order == nil {
		return
	}
	if !IsGateStaged(order) {
		return // a station wait, or an order already past its lane wait
	}
	if lane := laneOfGateWait(order); lane != 0 {
		d.EvaluateLaneReleases(lane)
	}
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

// GroupGateStagedCount is GateStagedCount for a whole node group: everybody
// dwelling anywhere in it.
//
// ── THE CONFIRMATION HAD TO WIDEN WITH THE PROPERTY ───────────────────────
//
// The waiting spots belong to the GROUP now, so clearing them stands down every
// lane in the block at once. A confirmation that counted one lane's dwellers
// would report a number far smaller than the interruption it is about to cause —
// which is worse than no confirmation, because the person reads it and proceeds.
//
// Same derivation as the per-lane count, summed over the group's lanes, for the
// same reason: one answer, so the number shown and the number acted on cannot
// disagree.
func (d *Dispatcher) GroupGateStagedCount(groupID int64) (int, error) {
	children, err := d.db.ListChildNodes(groupID)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, c := range children {
		if c == nil || c.NodeTypeCode != protocol.NodeClassLANE {
			continue
		}
		staged, err := d.gateStagedForLane(c)
		if err != nil {
			return 0, err
		}
		total += len(staged)
	}
	return total, nil
}

// releaseDweller resolves where a dwelling dig leg's blocker goes and releases
// it, or records why it is still holding. Reports whether it drove out.
//
// A dweller asks a different question from an entrant -- it is already inside
// the lane -- so it takes neither release path and proposes no heal. That rule
// is why this is a function and not an arm of the entry classifier, and why it
// touches neither the pass's acceptance nor its dig proposal.
func (d *Dispatcher) releaseDweller(c gateCandidate, lane *nodes.Node) (freed bool, next int64) {
	// A DWELLER ASKS A DIFFERENT QUESTION, SO IT DOES NOT GO THROUGH THE
	// ENTRY CLASSIFIER.
	//
	// gateEntryVerdict answers "may this robot come INTO this lane". The
	// dweller is already in it — Core sent it there and it is standing in a
	// slot the dig itself emptied — so every arm of that verdict is either
	// vacuously true or actively wrong for it (its own parent's dig holds the
	// lane; its own occupancy row is the one inside it). The question it is
	// actually waiting on is "where does this blocker go", and the resolver
	// both answers it and acts on the answer.
	//
	// AND IT PROPOSES NO HEAL. acceptanceDigNeeded proposes an excavation of THIS
	// lane's mouth, which is the wall a refused ENTRANT is sitting behind. A
	// dweller is behind no wall here: its lane is already being dug, by the
	// very parent this leg belongs to. What it lacks is somewhere in the
	// GROUP to put a bin, and no dig on this lane produces one.
	v, next, rErr := d.releaseDwellingDigLeg(c.order, lane)
	switch {
	case errors.Is(rErr, ErrAppendNotOwed):
		// The leg ended while it dwelled; nothing is written on it.
	case rErr != nil:
		log.Printf("lane gate: release dwelling leg %d in lane %s: %v", c.order.ID, lane.Name, rErr)
		d.setQueueReason(c.order, protocol.QueueWaitingForSlot, CauseGateReleaseFailed,
			QueueParams{Lane: lane.Name})
	case !v.Admitted():
		// THE LANE THAT HAS TO FREE, NOT THE ONE THE ROBOT IS IN. This passed
		// QueueParams{Lane: lane.Name}, which is wrong twice: QueueWaitingForSlot
		// does not render Lane at all (QueueParams' own header calls that the F1
		// defect — populated and ignored), and the dug lane is not the answer
		// anyway. A dweller is refused ABOUT SOMEWHERE ELSE: the slot it wanted
		// is in a lane another dig holds, or one a robot is standing in. The
		// verdict carries that lane, so the operator gets "Waiting for a slot at
		// <the lane that must free>" instead of a bare "Waiting for a slot".
		where := v.Lane()
		if where == "" {
			where = lane.Name
		}
		d.setQueueReason(c.order, protocol.QueueWaitingForSlot, v.Cause(),
			QueueParams{Destination: where})
		d.dbg("lane gate: dwelling leg %d still holding its blocker in %s (%s at %s)",
			c.order.ID, lane.Name, v.Cause(), where)
	default:
		d.setQueueReason(c.order, "", "", QueueParams{})
		freed = true
	}
	return freed, next
}

// gateLaterLaneEntries puts a lane wait in front of each step after entryIndex
// that enters a lane the robot is not in, starting from inLane (where the chosen
// drop now puts it). It is the splice's rule 2 (a wait per lane change) applied to
// the rest of a plan whose entry lane moved at release: the plan was spliced for
// the lane it pencilled, and a later step in that lane was "still in the lane"
// then, so it carries no wait of its own. An existing lane wait is kept and moves
// the robot into its lane. The junction (order_bins) shifts with the plan, as the
// splice's does (reindexOrderBinsForSplice).
func (d *Dispatcher) gateLaterLaneEntries(order *orders.Order, entryIndex int, inLane int64) error {
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(order.StepsJSON), &steps); err != nil {
		return fmt.Errorf("gate later entries: parse steps for order %d: %w", order.ID, err)
	}
	out := append(make([]resolvedStep, 0, len(steps)+1), steps[:entryIndex+1]...)
	moved := map[int]int{}
	inside := inLane
	for i := entryIndex + 1; i < len(steps); i++ {
		s := steps[i]
		if s.Action == protocol.ActionWait {
			if s.WaitKind == WaitKindLane {
				inside = s.WaitLane
			}
		} else if (s.Action == protocol.ActionPickup || s.Action == protocol.ActionDropoff) && s.Node != "" {
			lane, err := d.laneOfStep(s.Node)
			if err != nil {
				return fmt.Errorf("gate later entries: order %d step %d: %w", order.ID, i, err)
			}
			switch {
			case lane == nil:
				inside = 0
			case lane.ID != inside:
				t, gated, err := d.gateTargetForLane(lane)
				if err != nil {
					return err
				}
				if !gated {
					return fmt.Errorf("gate later entries: %w: %s", nodes.ErrLaneUnmarked, lane.Name)
				}
				out = append(out, resolvedStep{Action: protocol.ActionWait, Node: t.pointFor(order.ID),
					WaitKind: WaitKindLane, WaitLane: lane.ID, WaitGroup: groupIDOf(lane)})
				inside = lane.ID
			}
		}
		if len(out) != i {
			moved[i] = len(out)
		}
		out = append(out, s)
	}
	if len(out) == len(steps) {
		return nil
	}
	patched, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("gate later entries: marshal steps for order %d: %w", order.ID, err)
	}
	if err := d.db.UpdateOrderStepsJSON(order.ID, string(patched)); err != nil {
		return fmt.Errorf("gate later entries: persist steps for order %d: %w", order.ID, err)
	}
	order.StepsJSON = string(patched)
	rows, err := d.db.ListOrderBins(order.ID)
	if err != nil {
		return fmt.Errorf("gate later entries: read order_bins for order %d: %w", order.ID, err)
	}
	shift := map[int]int{}
	for _, r := range rows {
		if to, ok := moved[r.StepIndex]; ok {
			shift[r.StepIndex] = to
		}
	}
	if len(shift) > 0 {
		if err := d.db.ShiftOrderBinSteps(order.ID, shift); err != nil {
			return fmt.Errorf("gate later entries: shift order_bins for order %d: %w", order.ID, err)
		}
	}
	return nil
}

// laneOfStep is the lane a step's node stands in, or nil when it is not a lane
// slot (or not a Core node, as a wait's map point is not).
func (d *Dispatcher) laneOfStep(name string) (*nodes.Node, error) {
	node, err := d.db.GetNodeByDotName(name)
	if err != nil || node == nil {
		return nil, nil
	}
	return d.db.LaneForNode(node.ID)
}

// rebindWaitLane re-points the lane wait the order is parked AT, so the wait and
// the entry it gates name the same corridor once the release chose a slot in
// another lane of the group (C2c: the drop is chosen across the group).
//
// It patches the wait at the order's CURRENT WaitIndex — the one the evaluator is
// releasing — and not "the first lane wait", which is a different step on any
// plan that enters two lanes.
//
// groupID, when not 0, is written as the wait's group (WaitGroup) with it: the
// release onto a group's flat child names no lane (laneID 0), and the plan's
// check (assertEachWaitGatesItsEntry) reads the child against that group.
func (d *Dispatcher) rebindWaitLane(order *orders.Order, laneID, groupID int64) error {
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(order.StepsJSON), &steps); err != nil {
		return fmt.Errorf("rebind wait lane: parse steps for order %d: %w", order.ID, err)
	}
	seen := 0
	for i := range steps {
		if steps[i].Action != protocol.ActionWait {
			continue
		}
		if seen == order.WaitIndex {
			if steps[i].WaitKind != WaitKindLane {
				return fmt.Errorf("rebind wait lane: order %d is parked at a %q wait, not a lane one",
					order.ID, steps[i].WaitKind)
			}
			steps[i].WaitLane = laneID
			if groupID != 0 {
				steps[i].WaitGroup = groupID
			}
			patched, mErr := json.Marshal(steps)
			if mErr != nil {
				return fmt.Errorf("rebind wait lane: marshal steps for order %d: %w", order.ID, mErr)
			}
			if uErr := d.db.UpdateOrderStepsJSON(order.ID, string(patched)); uErr != nil {
				return fmt.Errorf("rebind wait lane: persist steps for order %d: %w", order.ID, uErr)
			}
			order.StepsJSON = string(patched)
			return nil
		}
		seen++
	}
	return fmt.Errorf("rebind wait lane: order %d has no wait at index %d", order.ID, order.WaitIndex)
}

// recordLaneWait is the one write of a lane refusal on a gate-staged order's
// row: the cause, the lane, and the order it waits behind (F5). The evaluator
// writes it on every refused pass, and the valve writes it when it refuses a
// create at its lane's wait (F2): before C2b the valve logged the refusal and
// wrote nothing, so the row carried no cause and nothing named the order it was
// waiting behind until the next evaluation or the floor.
func (d *Dispatcher) recordLaneWait(order *orders.Order, lane *nodes.Node, v GateVerdict) {
	d.setQueueReason(order, protocol.QueueWaitingForSlot, v.Cause(),
		QueueParams{Lane: lane.Name, BehindOrderID: v.Blocker()})
}

// laneWaitAfterAppend is the lane an append left order waiting at, or 0 (C2b
// F1). An append that advances an order onto a lane wait is that wait's own
// event: the lane may be free now, and nothing else will ask it until an
// unrelated event on that lane, the robot reaching the mark, or the periodic
// floor (STUDY-floor-and-gate §1(b): 141-166 of every packed run's 131-170
// floor releases were this). It reads the order as the append left it
// (appendSegmentAndAdvance advances wait_index in memory), and nothing else.
func laneWaitAfterAppend(order *orders.Order) int64 {
	if order == nil || !IsGateStaged(order) {
		return 0
	}
	var steps []resolvedStep
	if json.Unmarshal([]byte(order.StepsJSON), &steps) != nil {
		return 0
	}
	w, ok := waitAt(steps, order.WaitIndex)
	if !ok || w.WaitKind != WaitKindLane {
		return 0
	}
	return w.WaitLane
}

// evaluateLaneWaitAfterAppend evaluates the lane an append left order waiting
// at, now. For a caller holding no lane's mutex: a caller inside one (the lane
// release, the dwell) defers it through the pass's widened list, and the valve
// after its unlock (P0 ruling Q5).
func (d *Dispatcher) evaluateLaneWaitAfterAppend(order *orders.Order) {
	if lane := laneWaitAfterAppend(order); lane != 0 {
		d.EvaluateLaneReleases(lane)
	}
}

// stampBound records the moment the gate bound order's drop to slot, for the
// burial shadow's question "when did the selector last look" (C2b F4). The
// re-bind left no stamp, so a placement the selector re-bound was dated by a
// later, unrelated in_transit row (order 233: read as a GUARD BYPASS). A failed
// write costs the instrument a date, never the release.
func (d *Dispatcher) stampBound(order *orders.Order, slot *nodes.Node) {
	if err := d.db.StampDestinationBound(order.ID, clock.Now()); err != nil {
		log.Printf("lane gate: order %d bound to %s but the bind moment was not recorded: %v",
			order.ID, slot.Name, err)
	}
}
