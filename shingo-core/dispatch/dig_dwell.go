package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// THE OUTBOUND DWELL — a dig leg ships with no destination, waits inside the
// lane it is digging, and Core chooses where the blocker goes at release time.
//
// ── THE SHAPE ─────────────────────────────────────────────────────────────
//
// A robot is sent a dig mission at lane A, depth 2. Its waybill is
//
//	drive to A.N2 → jackload → drive to A.N1 → wait
//
// and it names NO destination. While the robot stands there, Core answers the
// question that used to be answered at plan time — where does this bin go — and
// APPENDS the tail. That is the same unsealed-create-plus-append the inbound
// gate has always used; what is new is which end of the order it serves.
//
// ── ALWAYS SHALLOWEST, NEVER OUTSIDE ──────────────────────────────────────
//
// Pick up, come to the SHALLOWEST slot in the lane, wait there. Already the
// shallowest — leg 1, always — and it does not move at all: it jackloads and
// waits in place.
//
// It costs no new motion. A robot that picked from depth 3 has to reverse out
// through depth 2 and depth 1 to leave the lane at all, so waiting at depth 1 is
// a PAUSE AT A POINT IT WAS ALREADY GOING TO DRIVE, not a manoeuvre added to the
// leg.
//
// The slot is empty by construction, and three independent facts in this tree
// say so rather than one assumption:
//
//  1. excavation is shallowest-first (planUnbury emits one unbury step per
//     blocker, front-to-back), so at the moment leg N lifts, depths 1…N−1 are
//     already worked;
//  2. blockers are never restocked — they lie where the unbury parked them, and
//     findShuffleSlots parks them in SIBLING lanes;
//  3. the dug lane's own slots are offered to nobody while the dig holds it
//     (findShuffleSlots skips c.ID == laneID, "the dig holds the lane
//     exclusively").
//
// So the waiting position consumes nothing the dig has not already consumed.
//
// ── WHAT IT BUYS, WHICH IS NOT THE STANDING SPOT ──────────────────────────
//
// Both wins are about WHEN THE DESTINATION IS CHOSEN.
//
// Lane B is no longer committed while the robot works in lane A. A sealed leg
// carries delivery_node = B.N4 from dispatch, and CheckDropoffCapacity counts
// in-flight orders BY delivery_node — so B.N4 read as spoken-for through the
// drive to A, the jackload and the drive out, while no robot was anywhere near
// it. Every other order that wanted B.N4 in that window was turned away for a
// bin that had not been picked up yet.
//
// And the chosen slot can no longer be buried before the robot arrives. The
// plan-time pick is held by NOTHING (shuffleSlotFree says the non-reservation is
// deliberate), and the burial test that would protect it is a plan-time snapshot
// by its own admission — "the set is computed once above". Two ordinary stores
// into shallower slots in B while the robot is still in A and the leg arrives at
// a slot it cannot reach, which lands in handleStaleDigLeg with two dispositions
// and no good one. Choosing at release does not mitigate that class; it removes
// the interval the class lives in, because there is no gap between choosing B.N4
// and driving there.
//
// ── WHY THIS IS NOT A SECOND WAIT MECHANISM ───────────────────────────────
//
// The wait is an ordinary WaitKindLane wait carrying an ordinary WaitLane — the
// DUG lane, which is a real lane with an evaluator, a 60-second floor, a cause
// vocabulary and a tripwire. That identity is the whole reason this shape works
// where the round-2 reading did not: a wait naming no lane is exempt from the
// abandon sweep while being invisible to every evaluator and every floor, which
// is unbounded, unreleasable and silent with a bin in the gripper. Nothing here
// adds a field, a status or a population; the dweller joins PopGateStaged, which
// already has both of law 8's paths.

// ErrNoDwellSlot is a lane a dig cannot stand in: no enabled, non-synthetic slot
// to hold the blocker at while Core decides where it goes.
//
// It is GEOMETRY, not congestion, and it is a sentinel for the same reason
// ErrSlotNotInLane is: every other way the dwell dispatch can fail is a database
// read, and the disposition for those is to WAIT (§R.45). A lane with no slots
// will not grow one by waiting, so the leg fails loudly and names the lane.
var ErrNoDwellSlot = errors.New("lane has no slot for a dig leg to dwell in")

// awaitsReleaseTimeDestination reports whether this order's destination is
// chosen at RELEASE rather than at planning: a compound child carrying no
// delivery node.
//
// ── ONE DECLARED DERIVATION, NOT AN INFERENCE FROM RESIDUE ────────────────
//
// It reads like residue and is not, because the population has exactly one
// writer. writeCompoundChildren is the only thing that creates a compound child,
// and it writes a delivery node for every step type EXCEPT an unbury — whose
// ToNode planUnbury now deliberately leaves nil, because the choice has moved.
// So "a child with no destination" is not a state to be inferred from; it is the
// state that writer creates on purpose, and the assertion dual is enforced where
// the leg is dispatched: a destination-deferred leg must resolve to a lane with a
// slot to stand in, or it fails as a config fault (AdvanceCompoundOrder).
//
// A leg with a destination is untouched by any of this, which is what keeps the
// retrieve tail and every non-dig order byte-identical.
func awaitsReleaseTimeDestination(o *orders.Order) bool {
	return o != nil && o.ParentOrderID != nil && o.DeliveryNode == ""
}

// waitGatesAnAppend reports whether the lane wait at waitIndex is an OUTBOUND
// one — a wait whose tail has not been chosen yet, so what it gates is the
// APPEND rather than an entry.
//
// ── THE DISCRIMINATOR IS THE PLAN'S SHAPE, AND THERE IS NO SECOND FIELD ───
//
// Owner ruling: the robot waits, Core decides — a dweller carries no extra
// stamp saying where it might go. It does not need one, because the two kinds
// of lane wait have structurally different plans and one writer each:
//
//	INBOUND  — spliceLaneWait inserts it IMMEDIATELY BEFORE the step that
//	           enters its lane, so an actionable step always follows it, and
//	           assertEachWaitGatesItsEntry refuses a plan where one does not.
//	OUTBOUND — the dwell plan ENDS at it. There is no next step because the
//	           step that would come next is exactly what Core has not chosen.
//
// So "a lane wait with nothing actionable after it" is the outbound arm, and it
// is answered off durable plan state rather than a second column. It stops being
// true the instant the tail is appended — which is correct, because at that
// moment the wait has been released and the order has indexed past it.
func waitGatesAnAppend(steps []resolvedStep, waitIndex int) bool {
	w, ok := waitAt(steps, waitIndex)
	if !ok || w.WaitKind != WaitKindLane {
		return false
	}
	_, _, _, hasEntry := laneEntryAfterWait(steps, waitIndex)
	return !hasEntry
}

// dwellSlotFor returns the slot a dig leg waits in: the SHALLOWEST enabled slot
// of the lane being dug.
//
// ListLaneSlots orders by depth ASC, so the first usable row is the answer.
// Synthetic children are skipped (they hold no bin and are no place to stand)
// and so are disabled ones — a disabled slot is a slot an engineer has taken out
// of service, and parking a loaded robot on it is not the exception to that.
//
// NO EMPTINESS CHECK, deliberately. The three facts in this file's header make
// the shallowest slot empty by construction at the moment the leg lifts, and a
// check here would be a guard compensating for a fact the system already owns
// (law 15) — it would also have to decide what to do when it failed, which is a
// question with no good answer while a robot holds a bin. If a bin is ever found
// standing in the dwell slot of a lane under an active dig, the defect is
// upstream of here: something placed into a lane the dig holds exclusively.
func (d *Dispatcher) dwellSlotFor(lane *nodes.Node) (*nodes.Node, error) {
	slots, err := d.db.ListLaneSlots(lane.ID)
	if err != nil {
		return nil, fmt.Errorf("dwell slot for lane %s: %w", lane.Name, err)
	}
	for _, s := range slots {
		if s.Enabled && !s.IsSynthetic {
			return s, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNoDwellSlot, lane.Name)
}

// digDwellPlan builds the unsealed waybill for a destination-deferred dig leg:
// pick the blocker up, come to the shallowest slot, wait for Core.
//
// It returns the lane target the valve ships against — the dug lane, with the
// dwell slot as its wait point. That target is what makes an UNMARKED lane work:
// the create must be unsealed whether or not the lane carries a gate mark,
// because the tail does not exist yet, and a sealed order with a trailing Wait
// block is a robot nothing can ever append to. The mark chooses where an INBOUND
// robot waits; it has nothing to say about where an outbound one does, which is
// why this shape has no deployability gate and works at both plants as they are
// configured today.
//
// ok=false for every order that is not a destination-deferred leg, so the plain
// path and the retrieve tail are untouched.
func (d *Dispatcher) digDwellPlan(order *orders.Order, sourceNode *nodes.Node) ([]resolvedStep, laneGateTarget, bool, error) {
	if !awaitsReleaseTimeDestination(order) || sourceNode == nil {
		return nil, laneGateTarget{}, false, nil
	}
	lane, err := d.db.LaneForNode(sourceNode.ID)
	if err != nil {
		return nil, laneGateTarget{}, false, fmt.Errorf("dwell plan: resolve lane for %s: %w", sourceNode.Name, err)
	}
	if lane == nil {
		// A leg with no destination whose pickup is not in a lane is not a dig leg
		// at all — there is no corridor to dig and no slot to stand in. Same
		// sentinel as an empty lane so the caller's one geometry arm covers both.
		return nil, laneGateTarget{}, false, fmt.Errorf("%w: %s is not a lane slot", ErrNoDwellSlot, sourceNode.Name)
	}
	dwellSlot, err := d.dwellSlotFor(lane)
	if err != nil {
		return nil, laneGateTarget{}, false, err
	}
	plan := []resolvedStep{
		{Action: protocol.ActionPickup, Node: sourceNode.Name},
		{
			Action:   protocol.ActionWait,
			Node:     dwellSlot.Name,
			WaitKind: WaitKindLane,
			WaitLane: lane.ID,
		},
	}
	return plan, laneGateTarget{lane: lane, gatePoint: dwellSlot.Name}, true, nil
}

// releaseDwellingDigLeg is the RELEASE-TIME RESOLVER: it chooses where a
// dwelling leg's blocker goes, binds it, and appends the tail.
//
// ── IT CALLS findShuffleSlots. IT IS NOT A SECOND SPELLING OF IT ──────────
//
// Law 3, and round 2 was unanimous about it: the question "where may a dig park
// a blocker" has one answer in this tree, and every exclusion it encodes — the
// dug lane itself, gated-pair lanes, slots in front of a hard-claimed bin,
// accessibility, occupancy and inbound traffic — is about what a dig may
// physically do, not about when the question is asked. Asking it later gets
// STRICTLY BETTER INFORMATION from the same function.
//
// ── THE INVARIANT AUDIT (§7.5) ────────────────────────────────────────────
//
// findShuffleSlots' invariants were written for a PLAN-TIME caller that asks once
// for N slots before any robot moves. This caller asks at RELEASE time, for one
// slot, repeatedly, with a robot already loaded. Every invariant was walked
// against that; the ones that needed something are named with what was built.
//
//	INVARIANT                     RELEASE-TIME CALLER            DISPOSITION
//	1 count contract              asks for 1; ErrNoShuffleSlot   HOLDS. The refusal
//	  (N slots or refuse)         is a wait, not a fault         is wait-not-fail
//	                                                             either way.
//	2 deepest-first (lane FIFO)   unchanged, and now reads       IMPROVED. The
//	                              live state                     answer is fresher.
//	3 dug-lane exclusion          the caller passes the          HOLDS. WaitLane IS
//	  (c.ID == laneID)            dweller's WaitLane             the dug lane, by
//	                                                             the assertion in
//	                                                             the splice.
//	4 gated-pair exclusion        still evaluated against the    HOLDS. (It is also
//	  (dugLaneGated)              dug lane                       the deletion the
//	                                                             ship order ends on;
//	                                                             not touched here.)
//	5 burial set computed ONCE    one read for one pick          IMPROVED. The
//	  ("the set is computed                                      snapshot no longer
//	  once above")                                               spans N picks.
//	6 shuffleSlotFree passes      would exclude the leg's OWN    BUILT: choose once.
//	  excludeOrderID = 0          destination on a retry         dwellDestination
//	                                                             returns early when
//	                                                             delivery_node is
//	                                                             set, so the choice
//	                                                             is never re-run.
//	7 it does not ask the DIG     the leg's dispatch used to     BUILT: the resolver
//	  question (dig_exclusion.go  ask it for the destination     runs admit() on the
//	  names this as one of the    lane; deferring the choice     chosen slot AND
//	  three disagreeing readers)  defers that question with it   WALKS ITS
//	                                                             CANDIDATES on a
//	                                                             refusal — see the
//	                                                             correction below.
//	8 idempotence                 the evaluator is level-        BUILT: choose-once,
//	                              triggered and re-asks          an owner-idempotent
//	                                                             claim, and
//	                                                             appendGateTail's
//	                                                             reload guard.
//	9 two choosers race between   plan-time callers raced too    BUILT: claim at the
//	  the read and the write      (2026-07-13); at release       moment of choice
//	                              they are under different       (§R.71 rider 1).
//	                              lane mutexes
//
// TWO THINGS THE AUDIT FOUND AND DID NOT FIX, both recorded rather than absorbed:
//
// A. THE PLAN-TIME COUNT CHECK SEES LESS THAN IT DID. It asks whether the group
//
//	has room NOW, and a dig that has planned but not yet released books nothing
//	— so on a tight pool two digs can both start where one used to be refused.
//	The wait moves from a `pending` row to a robot holding a bin, which is the
//	residual §7.8 names and accepts, bounded by the same congestion that would
//	have blocked the leg anyway. Reserving the count at plan time would be a
//	real span reservation, which is Track 3's open entry gate and explicitly not
//	this build's to pre-empt.
//
// B. A SLOT RESERVATION IS INVISIBLE TO shuffleSlotFree. The claim makes
//
//	double-booking impossible — the loser gets a conflict — but it does not make
//	the loser's NEXT read skip the slot; that happens one moment later, when the
//	winner's delivery_node makes it count as inbound traffic. Teaching
//	shuffleSlotFree to read reservations would close the gap and would also
//	narrow the pool for the plan-time caller, which is a behaviour change wider
//	than this build. Declined here, named here.
//
// ── AND ONE THING THE AUDIT GOT WRONG, CORRECTED BY MEASUREMENT ───────────
//
// The first version of row 7 stopped at "the resolver runs admit() on the chosen
// slot" and treated a refusal as an ordinary wait, reasoning that the candidate is
// deterministic so the dweller re-asks until the dig holding that lane finishes.
// THAT ASSUMED THE OTHER DIG FINISHES.
//
// Lane-stress rig, 17-minute window, 2026-08-13: seven digs each held a lane and
// each had a leg standing loaded under `lane-dig-active`, none able to finish
// because each was waiting on a lane one of the others held — while NINE slots
// stood free and legal elsewhere in the same two groups. 28 orders confirmed
// against a 113 baseline. Every dweller was handed the same refused candidate on
// every pass and had no way to ask for another.
//
// The fix is in the resolver's own contract rather than in the pool: it CHOOSES,
// which means walking its candidates until admission admits one. What it must not
// become is the parked right-of-way rule — that is a plan-time construction about
// which digs may START, and nothing here touches that.
//
// ── CHOOSE ONCE, RETRY THE APPEND ─────────────────────────────────────────
//
// The caller is idempotent by design (the evaluator re-derives its candidate set
// from durable state on every firing), so this can run several times for one
// leg. It must not re-choose on each pass: the first choice writes delivery_node
// and the tail step, and from that moment the leg is an ordinary in-flight order
// to everything that reads capacity. Re-running the choice would then exclude
// the leg's own destination (CheckDropoffCapacity counts it as inbound traffic)
// and walk the blocker to a different slot on every retry. So a leg that already
// has a destination skips straight to the append.
//
// ── THE VERDICT SHAPE IS THE CLASSIFIER'S, ON PURPOSE ─────────────────────
//
// A refused release is a WAIT with a cause, not an error: no free shuffle slot
// anywhere in the group is congestion (ErrNoShuffleSlot is explicit wait-not-fail
// policy) and a destination lane another dig owns is the same refusal a sealed
// leg would have taken at dispatch. Both leave the robot dwelling exactly where
// it is, which is the disposition the dwell exists to make cheap. An error is
// reserved for a read or a write that did not answer.
func (d *Dispatcher) releaseDwellingDigLeg(order *orders.Order, lane *nodes.Node) (GateVerdict, error) {
	fresh, err := d.db.GetOrder(order.ID)
	if err != nil {
		return GateVerdict{}, fmt.Errorf("reload dwelling leg %d: %w", order.ID, err)
	}
	// SAME DOUBLE-APPEND GUARD AS EVERY OTHER RELEASE. wait_index is the durable
	// witness: the shared append helper advances it only after the fleet accepted
	// the segment, so a second pass that raced this one reloads, sees the order is
	// no longer awaiting a tail, and returns without touching the fleet.
	if fresh == nil || !IsGateStaged(fresh) {
		d.dbg("dig dwell: leg %d is no longer awaiting a tail — another pass released it", order.ID)
		return Admitted(), nil
	}

	dest, v, err := d.dwellDestination(fresh, lane)
	if err != nil || !v.Admitted() {
		return v, err
	}

	// HOLD B FOR THE DESTINATION LANE, HERE AND NOT AT THE LEG'S DISPATCH.
	//
	// A sealed leg took occupancy on both of its lanes when it was dispatched,
	// because Core's decision to send is the entry moment and both endpoints were
	// known. A dwelling leg's second endpoint is not known until this line, so
	// this is where that moment is — the same reasoning appendGateTail states for
	// the inbound gate, applied to the other end. A destination that is not a lane
	// slot (a direct child of the group, findShuffleSlots pass 1) contributes no
	// row, exactly as it does everywhere else.
	if err := d.TakeLaneOccupancy(fresh.ID, dest); err != nil {
		return GateVerdict{}, err
	}
	if err := d.appendGateTail(fresh, "dig dwell release"); err != nil {
		// The robot never got the tail, so it is still standing in the dug lane and
		// has entered nothing. Drop the row this call took and leave everything the
		// dwell holds alone — releasing the order's whole presence here would free
		// the corridor the robot is physically standing in.
		d.releaseOccupancyForLaneOf(fresh.ID, dest)
		return GateVerdict{}, err
	}

	// AND NOW IT IS LEAVING. The tail is on the waybill, so the robot is driving
	// out of the dug lane — which is the exit the lift used to stand for and the
	// moment the corridor genuinely frees. Releasing here rather than at the lift
	// is the whole of the occupancy hold: the next leg still enters during the
	// drive-out, and only the dwell's own overlap is given up.
	//
	// ── THE ROW DROPS HERE; THE WAKE HAPPENS AFTER THE PASS ───────────────
	//
	// The ordinary exit path releases and immediately re-evaluates the lane, which
	// is what makes it a fix rather than bookkeeping. This one MUST NOT, and the
	// reason is the same one EvaluateLaneReleases states for the heal: this runs
	// INSIDE the pass, under that lane's mutex, and the mutex is not reentrant.
	// Waking the lane from here is a self-deadlock — a re-drive dispatches, which
	// emits, and the bus delivers on this goroutine to a subscriber that asks for
	// the lock this frame is holding.
	//
	// So the two halves are split exactly as the heal's are: the row goes now,
	// because the fact is true now, and the caller wakes the lane once it is out of
	// the critical section (evaluateLaneReleasesPass reports that it freed one).
	if err := reservations.ReleaseOccupancyForLane(d.db.DB, fresh.ID, lane.ID); err != nil {
		// The append has landed and the robot is driving out, so this is a stale row
		// rather than a failed release: loud, and left to the floor, because
		// returning an error here would report a successful release as a failure and
		// the next pass would try to append a tail the fleet already has.
		log.Printf("dig dwell: leg %d was released out of lane %d but its occupancy row could not be "+
			"dropped: %v — the lane will read busy until the row is reaped", fresh.ID, lane.ID, err)
	}
	d.dbg("dig dwell: leg %d released from %s to %s", fresh.ID, lane.Name, dest.Name)
	return Admitted(), nil
}

// dwellDestination answers where this leg's blocker goes, binding the answer as
// it makes it. Returns the destination node, or a refusal that leaves the robot
// dwelling.
func (d *Dispatcher) dwellDestination(leg *orders.Order, lane *nodes.Node) (*nodes.Node, GateVerdict, error) {
	if leg.DeliveryNode != "" {
		// Already chosen on an earlier pass whose append failed — see the doc above
		// on why this must not re-choose.
		dest, err := d.db.GetNodeByDotName(leg.DeliveryNode)
		if err != nil || dest == nil {
			return nil, GateVerdict{}, fmt.Errorf("dwelling leg %d: destination %q no longer resolves: %v",
				leg.ID, leg.DeliveryNode, err)
		}
		return dest, Admitted(), nil
	}
	if lane.ParentID == nil {
		// Shuffle slots are a GROUP-scoped resource; a lane with no group has no
		// pool to draw from. Geometry, and it fails the leg rather than parking it
		// under a cause nothing can clear.
		return nil, GateVerdict{}, fmt.Errorf("%w: lane %s has no group", ErrNoShuffleSlot, lane.Name)
	}

	// ── CHOOSING MEANS WALKING THE CANDIDATES, NOT TAKING THE FIRST ──────
	//
	// AND THE DESTINATION STILL FACES ADMISSION. A sealed leg asked the physical
	// questions about BOTH endpoints when it was dispatched (AdvanceCompoundOrder's
	// admit call). Deferring the destination defers that half of the question with
	// it, and the half that matters is the one findShuffleSlots has never asked: a
	// foreign dig CLAIMS a lane for a whole reshuffle without anyone being inside it
	// at that instant, so the candidate list can hand back a slot in a corridor
	// another excavation owns. Asking here keeps the leg's total set of admission
	// questions exactly what a sealed leg's was.
	//
	// TAKING ONE ANSWER AND GIVING UP ON A REFUSAL IS A LIVELOCK, AND THE RIG
	// MEASURED IT. findShuffleSlots is deterministic, so a caller that asks for one
	// candidate, is refused, and waits for the next event gets THE SAME CANDIDATE
	// and the same refusal on every pass — forever, if the dig holding that lane is
	// itself waiting. Lane-stress rig, 2026-08-13, 17-minute window: seven digs
	// standing loaded under `lane-dig-active` with NINE legal slots empty elsewhere
	// in their own groups, 28 orders confirmed against a 113 baseline. This was
	// named as a risk in the audit below and judged self-clearing on the grounds
	// that the other dig finishes. It does not, when the other dig is stuck the same
	// way.
	//
	// So the resolver's contract is what it always said it was — choose a slot
	// ADMISSION ADMITS — and it walks its candidates to keep it. Each refusal
	// excludes that slot from the next ask, so the loop is bounded by the pool.
	//
	// IT IS NOT THE RIGHT-OF-WAY RULE, and the distinction is exact. Right of way
	// (§R.61, ruled 2026-08-13) is a plan-time construction: a dig that cannot
	// assemble a dig-free plan never starts and holds nothing, which changes WHICH
	// DIGS RUN. The walk changes nothing about that; it is what a leg does with the
	// pool right of way left it.
	//
	// THEY COMPOSE AT THIS LINE, and the composition is what makes the walk short.
	// findShuffleSlots now takes the asker, so a dweller's candidate list never
	// contains a foreign dig's lane in the first place — the refusals this loop was
	// built to walk past are mostly gone at the source, and what remains is
	// admission's other arms (occupancy, reachability, a lane's own traffic). The
	// walk is kept anyway: right of way answers the DIG question and admission asks
	// several, and a resolver that gives up on the first refusal is a livelock
	// whatever produced the refusal.
	var (
		excluded = map[int64]bool{}
		refused  GateVerdict
	)
	for {
		slots, err := findShuffleSlots(d.db, lane.ID, *lane.ParentID, 1, digAskerFor(leg), excluded)
		if err != nil {
			// RIGHT OF WAY ANSWERS FIRST, because it names an order and the other two
			// name a shortage. Asked before ErrNoShuffleSlot deliberately: this error
			// does not wrap that one, and if it ever does, this arm still has to run
			// first or the specific cause is swallowed by the general one.
			var held *DigParkingHeldError
			if errors.As(err, &held) {
				return nil, RefusedAt(CauseDigHoldsParking, held.Lane), nil
			}
			if !errors.Is(err, ErrNoShuffleSlot) {
				return nil, GateVerdict{}, err
			}
			// Nothing left to offer. If admission turned candidates down along the
			// way, ITS cause is the true one and the more useful one: "every slot I
			// could reach is in a lane somebody else is digging" is a different
			// investigation from "the group is full", and they clear on different
			// events. Only an untouched pool reports no-shuffle-slot.
			if refused.Cause() != "" {
				return nil, refused, nil
			}
			return nil, RefusedAt(CauseNoShuffleSlot, lane.Name), nil
		}
		dest := slots[0]

		v, aErr := d.admit(admissionSituation{order: leg, destNode: dest})
		if aErr != nil {
			return nil, GateVerdict{}, aErr
		}
		if v.Admitted() {
			return d.bindChosenDestination(leg, lane, dest)
		}
		refused = v
		excluded[dest.ID] = true
		d.dbg("dig dwell: leg %d cannot place at %s (%s) — asking for another candidate",
			leg.ID, dest.Name, v.Cause())
	}
}

// bindChosenDestination is the second half of the choice: claim the slot, write it
// onto the leg, and hand it back. Split out so the candidate walk above reads as a
// walk rather than as a walk with a commit buried in it.
func (d *Dispatcher) bindChosenDestination(leg *orders.Order, lane, dest *nodes.Node) (*nodes.Node, GateVerdict, error) {

	// ── NO BIN EVER RETURNS TO A DUG LANE (§R.76, 2026-08-13) ─────────────
	//
	// THIS IS AN ASSERTION, NOT A FILTER. The filter already exists and is two
	// functions away: shuffleSlotsFrom's Pass 2 skips `c.ID == laneID`, so the
	// dug lane is not in the candidate pool and this can never fire in normal
	// operation. That skip is a `continue` inside a loop — one deleted line and
	// the pool silently widens to include the one lane it must never contain.
	// This is the line that makes the deletion loud instead.
	//
	// WHAT IT DEFENDS AGAINST IS A DESIGN, NOT A TYPO. The famine specimen makes
	// a symmetric fix look obvious: three digs are each other's only source of
	// space, so let one put its blocker back down at its own lane's mouth — the
	// slot its own dig just emptied — and dissolve. It is legal by construction,
	// it needs no new mode (the dwell's tail is unsealed and would take it), and
	// it was proposed. It is RULED OUT: a blocker is, by definition, the thing
	// standing between the lane's mouth and the bin somebody is coming for, so
	// putting one back re-buries the bin the excavation was raised to uncover.
	// No bin returns to a dug lane, in any scenario. A future builder reaching
	// for that fix arrives HERE, at a named refusal, rather than at a pool that
	// quietly hands them the slot.
	//
	// It is checked against the LANE BEING DUG rather than against dig locks in
	// general: a dig is exempt from its own lock everywhere else (that exemption
	// is what right of way is built on, reservations.DigAsker.ExcludedBy), so the
	// one hold that would otherwise stop this is precisely the one that lets it
	// through. Owner-blind here, deliberately, for the same reason the burial
	// exclusion above is: "not even your own" is the load-bearing half.
	//
	// A HARD ERROR rather than a refusal, against this file's own law-1 habit.
	// A refusal says "wait, this will clear" and it would not: the dug lane stays
	// dug for as long as the excavation runs, so a dweller parked on this cause
	// re-asks forever. Nothing legitimate produces this destination, so it is a
	// construction fault and it fails like one — loudly, naming the invariant, at
	// the writer that would have committed it.
	if dest.ID == lane.ID || (dest.ParentID != nil && *dest.ParentID == lane.ID) {
		return nil, GateVerdict{}, fmt.Errorf(
			"leg %d: refusing to place blocker at %s, which is inside %s — the lane this dig is "+
				"emptying. No bin ever returns to a dug lane (§R.76): a blocker is what stands between "+
				"the mouth and the target bin, so putting it back undoes the dig and re-buries the bin "+
				"this excavation was raised to uncover. Whatever chose this destination is the defect",
			leg.ID, dest.Name, lane.Name)
	}

	// CLAIM AT THE MOMENT OF CHOICE.
	//
	// Two dwellers released together must not pick one slot, and nothing else
	// stops them: they stand in different lanes, so they are released under
	// different mutexes, and between findShuffleSlots returning a slot and
	// bindDwellTail writing delivery_node there is a window in which that slot
	// still reads free to everybody. That window is the 2026-07-13 specimen with a
	// different clock on it — two digs picked SMN_008/SMN_009 three seconds apart,
	// a blocker landed on a blocker, EvictStaleGhostsTx threw a bin to _TRANSIT and
	// two bins were orphaned.
	//
	// claimStoreSlot is the sanctioned door and it is exclusive per node: the loser
	// of a concurrent choice gets ErrReservationConflict rather than a second
	// booking. It is reserve-ONLY, deliberately (a hard claim would make the node
	// look taken to everything, including the sibling stores that must instead
	// wait), and it is owner-idempotent, so a retry of this leg's own choice is not
	// a self-conflict.
	//
	// THE LOSER'S CAUSE IS CauseNoShuffleSlot, and it is not a new one on purpose.
	// "The slot I picked was taken from under me" and "there was no slot" are the
	// same wait to the robot standing in the lane, they clear on the same event —
	// any order anywhere in the group releasing a slot — and the loser re-asks on
	// the next firing, by which time the winner's delivery_node makes its slot
	// visibly spoken for and the loser picks another. A cause that lasts one pass
	// is vocabulary nobody groups by.
	if err := claimStoreSlot(d.db, leg, dest); err != nil {
		d.dbg("dig dwell: leg %d lost %s between choosing it and claiming it (%v) — re-asking", leg.ID, dest.Name, err)
		return nil, RefusedAt(CauseNoShuffleSlot, lane.Name), nil
	}
	if err := bindDwellTail(d.db, leg, dest); err != nil {
		return nil, GateVerdict{}, err
	}
	return dest, Admitted(), nil
}

// bindDwellTail writes the chosen destination onto the leg: the delivery_node
// column and the dropoff step the append is about to emit.
//
// BOTH, IN ONE PLACE, for the reason applyDeliveryNodeAtStep exists — the column
// and the plan are two copies of one fact and a writer that moves only one of
// them sends the robot somewhere the row does not name. This is the append
// flavour of that writer: there is no step to patch yet, so it APPENDS one,
// which is what makes the create carry no stale target to correct.
//
// The step is appended rather than inserted at an index, and the plan ends at
// the dwell wait, so the new step is the first actionable one after it — which
// is precisely what splitSegment will hand the fleet.
func bindDwellTail(db *store.DB, leg *orders.Order, dest *nodes.Node) error {
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(leg.StepsJSON), &steps); err != nil {
		return fmt.Errorf("dwelling leg %d: parse plan before binding its tail: %w", leg.ID, err)
	}
	steps = append(steps, resolvedStep{Action: protocol.ActionDropoff, Node: dest.Name})
	patched, err := json.Marshal(steps)
	if err != nil {
		return fmt.Errorf("dwelling leg %d: marshal plan with its tail: %w", leg.ID, err)
	}
	// THE PLAN FIRST, THEN THE COLUMN. appendGateTail rebuilds the segment from
	// steps_json, so a crash between the two leaves a leg whose plan names the
	// destination and whose column does not — recoverable, because the next pass
	// reads the plan. The reverse order leaves a column pointing at a slot no step
	// mentions, which is the shape that drove a robot to one node while steps_json
	// said another (§R.5).
	if err := db.UpdateOrderStepsJSON(leg.ID, string(patched)); err != nil {
		return fmt.Errorf("dwelling leg %d: persist plan with its tail: %w", leg.ID, err)
	}
	leg.StepsJSON = string(patched)
	if err := db.UpdateOrderDeliveryNode(leg.ID, dest.Name); err != nil {
		return fmt.Errorf("dwelling leg %d: record destination %s: %w", leg.ID, dest.Name, err)
	}
	leg.DeliveryNode = dest.Name
	log.Printf("dig dwell: leg %d bound at release to %s", leg.ID, dest.Name)
	return nil
}

// DwellerLanesSharingGroupWith returns the lanes holding an outbound dweller in
// the same GROUP as nodeID — the widened trigger set.
//
// ── WHY THE ORDINARY TRIGGERS ARE NOT ENOUGH FOR A DWELLER ────────────────
//
// Every other lane trigger resolves "which lane just changed" and re-evaluates
// THAT lane, because an entrant is waiting for the lane it wants to open. A
// dweller is waiting for something else: a slot to free anywhere in the group.
// And the one lane that will NOT clear is its own — it is standing in it. So the
// release evaluator, which picks up waiters by w.WaitLane == lane.ID, would only
// re-ask a dweller on events in the corridor it is itself blocking.
//
// The answer is to widen what re-evaluates a dwelling leg rather than to give it
// a second wait mechanism: a slot freeing in a sibling lane, or an order
// terminating and giving back its inbound claim, is exactly the event that
// changes a dweller's answer, and both already fire. This maps them onto the
// lanes that care.
//
// The 60-second floor is the backstop underneath, which makes an INCOMPLETE
// trigger set slow rather than wrong — the same property that lets the trigger
// set be generous everywhere else in this file's neighbours.
// EvaluateDwellersSharingGroupWith re-asks every outbound dweller whose shuffle
// pool contains nodeID's group.
//
// ── WHY THIS EXISTS AS A FUNCTION AND NOT AS A LOOP AT TWO CALL SITES ─────
//
// The engine had this loop inline over the bin and terminal events
// (wiring_lane_gate.go's evaluateGroup) and the DIG-LOCK RELEASE did not have it
// at all — it evaluated only the lane it freed. That is the one releaser the
// `dig-holds-parking` row names, so the cause whose whole definition is "another
// dig holds the parking" was the cause whose release did not reach the robots
// waiting on it.
//
// It was covered anyway, by accident, and the accident is the reason it survived:
// flip 2's release fires from a bin entering transit out of the dug lane, and that
// same event separately reaches the engine's fan-out. So the wake happened — for a
// different reason than the releaser table gives, on a path that stops working the
// moment a dig releases for any other reason. The teardown path
// (unlockLaneForCompound) is that other reason, and there the fan-out rides the
// order's TERMINAL event, which fires from the lifecycle write BEFORE the unlock —
// so the dwellers re-ask while the lock is still there, refuse, and nothing wakes
// them after it drops except the 60-second floor.
//
// One spelling, called from all three, so the releaser row's promise is true by
// construction rather than by coincidence.
func (d *Dispatcher) EvaluateDwellersSharingGroupWith(nodeID int64) {
	for _, laneID := range d.DwellerLanesSharingGroupWith(nodeID) {
		d.EvaluateLaneReleases(laneID)
	}
}

func (d *Dispatcher) DwellerLanesSharingGroupWith(nodeID int64) []int64 {
	groupID := d.groupOfNode(nodeID)
	if groupID == 0 {
		return nil
	}
	candidates, err := d.db.ActiveGateCandidates()
	if err != nil {
		log.Printf("dig dwell: list gate candidates while widening the trigger for node %d: %v", nodeID, err)
		return nil
	}
	seen := map[int64]bool{}
	var out []int64
	for _, o := range candidates {
		if !IsGateStaged(o) || o.Status == StatusFaulted {
			continue
		}
		var steps []resolvedStep
		if json.Unmarshal([]byte(o.StepsJSON), &steps) != nil {
			continue
		}
		if !waitGatesAnAppend(steps, o.WaitIndex) {
			continue
		}
		w, ok := waitAt(steps, o.WaitIndex)
		if !ok || w.WaitLane == 0 || seen[w.WaitLane] {
			continue
		}
		lane, lErr := d.db.GetNode(w.WaitLane)
		if lErr != nil || lane == nil || lane.ParentID == nil || *lane.ParentID != groupID {
			continue
		}
		seen[w.WaitLane] = true
		out = append(out, w.WaitLane)
	}
	return out
}

// groupOfNode resolves the group a node's shuffle pool is drawn from: the lane's
// parent for a lane slot, the node's own parent for a direct child of the group.
//
// It is the same walk findShuffleSlots' two passes make from the other side —
// pass 1 over the group's direct children, pass 2 over its lanes — so a node this
// returns a group for is a node whose freeing can change a dweller's answer.
func (d *Dispatcher) groupOfNode(nodeID int64) int64 {
	if nodeID == 0 {
		return 0
	}
	if lane, err := d.db.LaneForNode(nodeID); err == nil && lane != nil {
		if lane.ParentID == nil {
			return 0
		}
		return *lane.ParentID
	}
	node, err := d.db.GetNode(nodeID)
	if err != nil || node == nil || node.ParentID == nil {
		return 0
	}
	return *node.ParentID
}

// releaseOccupancyForLaneOf drops one order's occupancy on the lane containing
// node, without re-evaluating it.
//
// The re-evaluating form (releaseOccupancyOnExit) is for a robot that has LEFT a
// corridor, where waking whoever queued behind it is the point. This one is a
// rollback of a row taken moments ago for an entry that did not happen: nobody
// was ever refused because of it, so there is nobody to wake.
func (d *Dispatcher) releaseOccupancyForLaneOf(orderID int64, node *nodes.Node) {
	if node == nil {
		return
	}
	lane, err := d.db.LaneForNode(node.ID)
	if err != nil || lane == nil {
		return
	}
	if err := reservations.ReleaseOccupancyForLane(d.db.DB, orderID, lane.ID); err != nil {
		log.Printf("dig dwell: roll back occupancy for order %d on lane %d: %v", orderID, lane.ID, err)
	}
}
