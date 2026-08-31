package dispatch

import (
	"errors"
	"fmt"
	"strings"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
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
//
// "RECOVERABLE" IS TRUE AGAIN, and for a while it was not. This justification was
// written when refusing meant the demand waited — but the complex burial sites
// mapped a bare error from here onto a terminal, so a momentary database stutter
// killed the operator's order and there was nothing to recover. The owner ruled
// on it (PLAN §R.45: "we dont want orders to fail because of a stutter. that
// demand isnt going away"), and the callers now park under CauseReadFailed and
// re-ask on the next sweep. The sentence and the behaviour agree again.
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
//
// ── COUNT AT PLAN TIME, CHOOSE AT RELEASE TIME ────────────────────────────
//
// The findShuffleSlots call below is kept for its COUNT and its refusal, and its
// answer is deliberately discarded: which slot each blocker goes to is chosen
// when the robot is standing ready to drive, by the release-time resolver
// (dig_dwell.go). An unbury step therefore carries NO ToNode, and the child
// written from it carries no delivery node.
//
// ALL-OR-NOTHING IS NOT GIVEN UP BY THAT, and it is worth saying why, because
// the obvious objection is that deferral trades an all-or-nothing acquisition
// for an incremental one that could strand a dig half-excavated. Today's
// guarantee was never an acquisition: the slots this call returns are RESERVED
// BY NOTHING (shuffleSlotFree says the non-reservation is deliberate — a real
// span reservation for the dig is Track 3's open entry gate), and
// writeCompoundChildren writes bin claims only. So a dig can already be stranded
// half-excavated by another order taking a planned slot; it just fails later and
// worse, by arriving at a buried slot and dissolving. What is kept here is
// exactly what was ever there — a plan-time COUNT snapshot, which still refuses
// to start a dig the group cannot hold — and what moves is the choice, which
// against an unreserved pool is strictly better information late than early.
//
// Owner ruling: if the count check cannot be satisfied, that is an engineering
// and configuration capacity problem, not a dispatch one.
//
// ── AND THE COUNT IS NOW A DIG-FREE COUNT (§R.61's construction) ──────────
//
// asker is the order that will own this dig. It is what makes the count check
// the right-of-way rule: findShuffleSlots reads the group through
// ListChildNodesUnlocked, so a lane another dig holds is not in the pool, and a
// dig that cannot count enough dig-free parking DOES NOT START. It has taken no
// lane, dispatched no leg and claimed no bin at this point — the plan precedes
// the lock — so the refusal costs a wait and nothing else.
func planUnbury(db *store.DB, target *bins.Bin, targetSlot, lane *nodes.Node, groupID int64, asker reservations.DigAsker) (*ReshufflePlan, int, error) {
	if targetSlot.ParentID == nil {
		return nil, 0, fmt.Errorf("%w: %s", ErrSlotNotInLane, targetSlot.Name)
	}

	blockers, err := findBuriedBlockers(db, targetSlot.ID)
	if err != nil {
		return nil, 0, err
	}

	// ── CAN THIS GROUP CLEAR THESE BLOCKERS? THAT IS THE WHOLE QUESTION ───
	//
	// A CLAIMS LEDGER STOOD HERE AND IS DELETED (§R.79 supersedes §R.75/§R.76
	// arm 1). It counted the room already owed to running digs and required this
	// dig to fit on top of it. Two things were wrong with it and the closing run
	// showed both.
	//
	// It counted the wrong UNIT: `outstanding` was a count of DIGS, added to
	// len(blockers), a count of SLOTS. And correcting the unit would not have
	// helped, because a dig's instantaneous claim genuinely IS one slot — a
	// compound dispatches one child at a time — while its cost to the group is
	// every blocker it will park, permanently, since blockers are never
	// restocked. The ledger was an estimate of a future it could not see.
	//
	// What actually keeps two digs from eating each other is OWNERSHIP, not
	// arithmetic: one robot per lane, enforced where the candidates are built
	// (shuffleSlotsFrom now drops occupied lanes), so a dig fills a lane it owns
	// deepest-first and cannot strand slots behind itself. A dig that can find
	// somewhere to put its blockers runs; one that cannot, waits.
	//
	// So the question is the plain one again, and the owner's ruling on it stands
	// unchanged: if the count cannot be satisfied, that is an engineering and
	// configuration capacity problem, not a dispatch one. The seed has to carry
	// enough reachable room to clear a lane, which is what the census at birth
	// asserts before a single order runs.
	if _, err := findShuffleSlots(db, lane.ID, groupID, len(blockers), asker, nil); err != nil {
		return nil, 0, fmt.Errorf("find shuffle slots: %w", err)
	}

	plan := &ReshufflePlan{
		TargetBin:  target,
		TargetSlot: targetSlot,
	}
	seq := 1
	// Front-to-back order = shallowest first, which is the order a robot can
	// physically take them out in.
	for _, b := range blockers {
		plan.Steps = append(plan.Steps, ReshuffleStep{
			Sequence: seq,
			StepType: protocol.StepUnbury,
			BinID:    b.bin.ID,
			FromNode: b.slot,
			// ToNode stays nil: the destination is chosen at release time.
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
// destination. A COMPLEX order never comes through here: its burial is served by
// a separate lane-clear dig (PlanLaneMouthClear), which owns no retrieve at all.
// There used to be a third planner for that — PlanReshuffleUnburyOnly, "expose
// mode" — which exposed the bin and handed back to the complex parent to fetch
// it. The hand-back is what the A batch deleted; the excavation it did is what
// PlanLaneMouthClear already does, keyed on the SLOT rather than the bin.
func PlanReshuffle(db *store.DB, target *bins.Bin, targetSlot *nodes.Node, lane *nodes.Node, groupID int64, asker reservations.DigAsker) (*ReshufflePlan, error) {
	plan, seq, err := planUnbury(db, target, targetSlot, lane, groupID, asker)
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

// PlanLaneMouthClear plans the excavation that makes targetSlot reachable and
// STOPS THERE: this dig exists to open a path, not to fetch anything.
//
// The other planner is built around a target BIN somebody wants: PlanReshuffle
// retrieves it. This dig has no such bin. What is wanted is the SLOT, and the order that
// wants it is already standing at the lane's mark with a robot under it. So the
// plan is planUnbury and nothing after it, and TargetBin stays nil.
//
// AN EMPTY EXCAVATION IS AN ERROR, NOT A NO-OP PLAN. A plan with no steps would
// still create a parent, take the lane's dig lock, complete on the next tick and
// release it — a lot of machinery for a lane nothing was blocking. The caller has
// already established that the lane IS blocked, so no blockers here means the
// lane moved underneath us between the two reads, and the right answer is to do
// nothing and re-ask on the next pass.
func PlanLaneMouthClear(db *store.DB, targetSlot, lane *nodes.Node, groupID int64, asker reservations.DigAsker) (*ReshufflePlan, error) {
	plan, _, err := planUnbury(db, nil, targetSlot, lane, groupID, asker)
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
// EXCLUDE IS THE RELEASE-TIME CALLER'S, AND IT IS NOT A POLICY.
//
// A caller that has to ASK ABOUT a candidate before it can use one — the dwell's
// release-time resolver puts every choice through admission — needs to be able to
// say "not that one, what else". Without it the function answers the same first
// candidate on every pass, and a caller that must refuse that candidate refuses
// forever: measured on the lane-stress rig 2026-08-13, seven digs stood loaded for
// a whole 17-minute window under lane-dig-active with NINE legal slots empty
// elsewhere in their groups.
//
// It changes no rule about WHICH slots a dig may use. Every exclusion in this
// function is unchanged and still applies; exclude only lets one caller walk past
// an answer it has already been refused, in its own loop, on its own pass. Plan
// time passes nil and is byte-identical.
//
// ── RIGHT OF WAY: A DIG MUST NOT PLAN INTO A LANE ANOTHER DIG HOLDS ───────
//
// asker is that rule, and it is the whole of it (§R.61, ruled 2026-08-13). The
// candidate read is ListChildNodesUnlocked — THE ONE EXISTING SPELLING of the
// dig-lock question (reservations.DigExclusionSQL), the same read sourcing has
// used since dig_exclusion.go, whose own header names this function as the third
// reader that "did not ask at all". It asks now.
//
// IT ADDS NO HOLD. This constrains the PLANNER, not the lock: a dig still takes
// exactly one mouth row, its own lane, and the parking lane is never claimed.
// The rejected C3 (all-or-nothing acquisition of every lane a dig will park in)
// is what would have closed corridors with nothing in them, and it stays in the
// drawer as the named escalation. Reservations do not ride along — law 13.
//
// WHY THIS IS A CONSTRUCTION AND NOT INCIDENCE REDUCTION: the plan precedes the
// lock (lane_clear_dig.go's PlanLaneMouthClear → TryLock; planning_service.go's
// PlanReshuffle → TryLock). A dig that cannot count a dig-free pool never starts,
// so it holds nothing, so it cannot be half of a hold-and-wait. That is where the
// dig→dig wait-for graph loses its edges rather than merely its cycles.
//
// THE DWELL WEAKENS "NO EDGES" TO "NO CYCLES", AND SAYING SO IS THE POINT.
// R.61 was written when the destination was chosen at plan time; the outbound
// dwell moved the choice to release time, so a dweller re-asks this function
// against a pool that has moved, and its pool excludes lanes dug by digs that
// started AFTER it planned. So an edge can exist. It cannot close into a cycle:
// a dig planning at t2 excludes every lane held at t2, so it never waits on a dig
// older than itself, and every edge therefore points from an older dig to a
// younger one. Edges ordered by start time are acyclic.
//
// THE RESIDUAL, NAMED, NOT PAPERED OVER: two digs that both planned against a
// pool that was wide enough, and then had it eaten out from under them by
// ordinary traffic until each one's only remaining candidates lie in the other's
// lane, are still a cycle — younger-waits-on-older is what the argument above
// forbids, and this is older-waits-on-younger in both directions, which it does
// not. It needs the pool to shrink to nothing-but-each-other AFTER both plans;
// the plan-time class (the 2026-08-13 rig specimen, where the second dig started
// into a group that had no dig-free parking at all) is gone. If wedge C recurs on
// a rig carrying this rule, that is the residual, and C3 is what answers it.
//
// ── THE COUNTER-ARGUMENT ON FILE, ANSWERED IN WRITING (law 14) ────────────
//
// The block below ("WHY THIS IS PER-SLOT AND NOT SKIP DIG-LOCKED LANES") records
// a previous rejection of exactly this exclusion: *a lane whose dig is still
// RUNNING is a legitimate place to park — its target is buried anyway, the mouth
// slot is genuinely free, and refusing it starves a dig that had somewhere to
// go.* Every clause of that is true and it is still refused, on three grounds.
//
// 1. IT WAS ARGUED ABOUT A DIFFERENT FACT. That paragraph is inside the BURIAL
// exclusion, which reads hard claims and must be owner-BLIND. The dig-lock
// question exempts its owner. Folding them was already refused there, in the
// other direction, for the same reason: two facts, two answers, two readers.
//
// 2. THE STARVATION IS BOUNDED AND THE ALTERNATIVE IS NOT. "Starves a dig that
// had somewhere to go" costs that dig a WAIT, taken before it holds anything,
// released by an event that is already wired (the holder's own lane release).
// Permitting the park costs a cycle between two loaded robots, which is not
// latency. Measured on the lane-stress rig 2026-08-13, 2h15m: two digs, two
// robots standing in lanes holding blockers, `lane-dig-active` on both, 28 orders
// confirmed against a 113 baseline, and neither ever moved again.
//
//	── GROUND 2 IS FALSIFIED ON A TIGHT GROUP (2026-08-13, §R.75/§R.76) ──
//
//	"Bounded" holds only while the starving dig can get space from something
//	OTHER than another dig. Under famine it cannot, and then the bound is
//	circular: the digs are each other's only source of space, so each one's
//	release event is behind the wait it is supposed to release.
//
//	Measured the same day, on the tree that carries this rule: three digs
//	(LS_C1/LS_C2/LS_C11), three loaded dwellers, frozen byte-identical across
//	two reads 5m39s apart, throughput decaying 10-8-7-2-6-3-1-2-1 per minute.
//	The only usable free space in the whole group was the six mouth slots the
//	three digs had themselves emptied; every other empty was buried behind a
//	bin or already claimed with a robot inbound. This rule then made those six
//	mutually invisible.
//
//	The claim that survives is the COMPARISON, not the bound: permitting the
//	park still costs a cycle, and grounds 1 and 3 are untouched. What the
//	famine killed is the idea that refusing is free. It is free only when the
//	group can afford the digs it is running — which is why the affordability
//	is now ASKED, at dig admission, instead of assumed here (§R.75 arm 1).
//	Right of way keeps digs out of each other's lanes; the capacity claim is
//	what keeps a group from starting digs it cannot feed.
//
//	AND THE SYMMETRIC FIX THIS PARAGRAPH INVITES IS RULED OUT. A reader who
//	accepts that the digs are each other's only source of space will reach
//	for the obvious response: let one of them put its blocker back down at
//	the mouth slot its own dig emptied — the one space famine cannot take —
//	and dissolve to re-select. It was proposed and REJECTED (§R.76). A
//	blocker is what stands between the mouth and the target bin; returning
//	one undoes the dig and re-buries that bin. No bin ever
//	returns to a dug lane, in any scenario. The invariant is asserted at
//	bindChosenDestination (dig_dwell.go) rather than left to the `c.ID ==
//	laneID` skip two passes below, so that rebuilding it fails loudly instead
//	of quietly widening a pool.
//
// 3. THE FREE-MOUTH-SLOT PREMISE IS THE TRAP, NOT THE EXCEPTION. "Its target is
// buried anyway, so parking changes nothing" is true of the lane and false of the
// system: what the parked bin blocks is not that dig's target, it is that dig's
// ability to put its OWN next blocker down — which is the precise shape both
// halves of the rig specimen took.
//
// TestCrossFlow_TwoDigsOneLane_BsLegWinsTheRace is the test that recorded the
// rejection, and it is not silently re-pointed: it is inverted in place, under
// the name TestCrossFlow_TwoDigsOneLane_ANeverStarts, with the premise it used to
// assert quoted in its own header.
func findShuffleSlots(db *store.DB, laneID, groupID int64, count int, asker reservations.DigAsker, exclude map[int64]bool) ([]*nodes.Node, error) {
	children, err := db.ListChildNodesUnlocked(groupID, asker)
	if err != nil {
		return nil, err
	}
	return shuffleSlotsFrom(db, laneID, groupID, children, count, asker, consultTheMouth, exclude)
}

// shuffleSlotsFrom is findShuffleSlots' body over a candidate list it is HANDED.
//
// Split out for exactly one caller: the shortfall path, which has to answer "would
// the pool have been wide enough without right of way" and can only answer it by
// running the same walk against the unfiltered group. Asking it any other way —
// counting empty slots in the excluded lanes, say — would be a second, simpler,
// and disagreeing spelling of what a shuffle slot is (law 3). This is the walk
// itself, run twice, on the refusal path only.
// consultTheMouth / skipTheMouth name shuffleSlotsFrom's last filter, because a
// bare bool at a call site is exactly the kind of parameter that gets passed
// wrong once and is never noticed.
//
// The ONE caller that skips it is digHeldParking, and it must: that walk exists
// to ask "would the pool have been wide enough WITHOUT the exclusions", so
// re-applying an exclusion inside it answers the opposite question and reports
// every right-of-way refusal as an honestly full group. Caught by three fixtures
// the first time it was written the other way.
const (
	consultTheMouth = true
	skipTheMouth    = false
)

func shuffleSlotsFrom(db *store.DB, laneID, groupID int64, children []*nodes.Node, count int, asker reservations.DigAsker, askTheMouth bool, exclude map[int64]bool) ([]*nodes.Node, error) {

	// A GATED DIG MAY PARK ITS BLOCKER IN ANOTHER GATED LANE. IT USED NOT TO.
	//
	// ── WHY THE EXCLUSION EXISTED, AND WHY IT IS GONE ────────────────────
	//
	// It was added on the lane-stress rig 2026-08-09, and the failure was real:
	// spliceLaneWait then allowed ONE gated lane per plan and refused a second
	// outright, so a dig out of a marked lane whose blocker landed in a marked
	// empty lane failed at the splice — which failed the parent, the two-robot
	// swap it was supplying, and the evac. Four terminal orders from one
	// unexpressible plan, and nothing self-cleared: both marks stay where they
	// are, so the re-plan picks the same slot and fails the same way.
	//
	// THAT PLAN IS EXPRESSIBLE NOW. lane_gate_dispatch.go rule 2 became "a wait
	// per gated lane the plan enters" — the leg dispatches cleanly with a wait at
	// each mark, each released by its own lane's admission. The exclusion has
	// outlived the refusal that forced it, and the comment that stood here said
	// so, in these words: "this is now a CONSERVATISM rather than an
	// impossibility … the next person to widen the shuffle pool will come looking
	// here … Neither is guessed at cheaply — measure it."
	//
	// ── SO IT WAS MEASURED, AND THE CONSERVATISM COSTS MORE THAN IT SAVES ─
	//
	// demo.yaml 2026-08-31, all 16 lanes marked for the first time IN THIS
	// FIXTURE. With every lane gated, "park in an ungated lane" names no slot in
	// the plant, so EVERY dig held:
	//
	//	complex: could not read Lane_01 while planning a dig for demand 9
	//	  (find shuffle slots: ... this dig is 2 slot(s) short) — holding
	//
	// Six digs stuck from the first minute of the run, each with a partner leg
	// stuck behind it, and the lines starved behind those. The refusal is a wait
	// and it is honest, but a wait whose releaser is "somebody un-marks a lane"
	// has no releaser at all.
	//
	// ── AND "NO FIXTURE HAD EVER CARRIED A MARK" WAS FALSE WHEN IT WAS WRITTEN ─
	//
	// This paragraph used to say that, and it was believable because the fixture
	// in front of the author had none. The two lane-stress rigs each declared six
	// lane gate_points and had since before this exclusion existed — which is the
	// whole reason the rig could meet the two-gated-lane plan on 2026-08-09 (WALL)
	// and find the defect the exclusion was added for.
	//
	// THE KEY HAS SINCE MOVED, AND THE MARKS WITH IT. No plant spec in this repo
	// declares a per-lane gate_point any more: the waiting spots belong to the
	// GROUP (`wait_points` on the NGRP), and all three specs migrated — demo.yaml
	// included, which lists fifteen for SYN_MARKET at plants/demo.yaml:165. So the
	// shipped demo fixture IS gated today; what changed is only which key says so.
	// The per-lane key still resolves first where a human sets it through the API,
	// as the documented legacy fallback. store/nodes/lanes.go carries the same
	// fact at the query that depends on it.
	//
	// The plant half is now DATA rather than inference: Springfield and
	// Hopkinsville were queried directly on 2026-08-31 and carry ZERO
	// lane_gate_point rows. That is a read of both plant cores, not a comment
	// quoting another comment.
	//
	// ── WHAT THE EXCLUSION WAS PROTECTING, AND WHICH HALF IS STILL UNMEASURED ─
	//
	// The objection stands and is real: a dig holds its lane EXCLUSIVELY, so a leg
	// dwelling at a second lane's mark keeps the dug corridor shut for as long as
	// the second lane is congested. It is lawful and self-clearing — the second
	// lane's own admission releases it.
	//
	// It is NOT known to be bounded, and this paragraph used to say it was. The
	// owed measurement had two halves. Pool width was measured, above: six stuck
	// digs, decisive. LANE-HOLD DURATION — the old comment's actual objection —
	// was never measured, and the all-16-marked demo run is the one fixture shape
	// that cannot answer it, because when every lane is gated there is no ungated
	// control to compare against. It runs on plants/lane-stress.yaml, which exists
	// for exactly this and is already marked.
	//
	// So: pool width wins on evidence, because a dig that never starts is
	// unconditional and a longer hold is conditional on congestion. The trade is
	// argued, not measured, and the second number is still owed.
	//
	// The dug-lane exclusion below is NOT this one and stays: never park a blocker
	// back into the lane being dug out of.

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
	// SUPERSEDED IN PART, AND ONLY IN PART — read the right-of-way block in this
	// function's header before this one. The lane-level skip this paragraph argues
	// against is now the rule, for reasons answered there point by point; what
	// survives here unchanged is that THIS exclusion, the burial one, stays
	// per-slot and owner-blind. They are two facts and the header says why folding
	// them is still wrong.
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
	// is precisely what let a parent bury its own target bin. Owner-blind on purpose.
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
	// WHAT A DIG MAY NOT BURY, asked of the claims rather than of the bridge.
	//
	// This used to read pending_lane_extensions — a row an expose-mode dig left
	// behind naming the bin it had just uncovered, which was then protected until
	// the complex parent came back for it. That bridge is gone with the hand-back
	// it existed for. The FACT is unchanged and now comes from where it always
	// really lived: a bin with a hard claim is a bin a robot is already on its way
	// to, and parking a blocker in front of it walls that robot out.
	//
	// ONE SPELLING with the store selector's own burial clause — both go through
	// helpers.ShallowerInSameLane (see SlotsBlockedByHardClaims, and the drift test
	// that keeps them agreeing).
	// Hold B across the group: which lanes have a robot in them right now. Read
	// once, for the same reason the burial set is — it is asked per candidate and
	// the answer cannot change mid-pass without the pass being wrong anyway.
	var occupiedSkipped []*nodes.Node
	// Lanes dropped by the mouth consultation, kept apart because they are two
	// different findings: one is a lane somebody owns, the other is a lane nobody
	// could read. See the consultation in Pass 2.
	var mouthHeld, unseen []*nodes.Node
	occupied, oErr := db.LanesOccupiedInGroup(groupID)
	if oErr != nil {
		// Same disposition as the burial read below: cannot tell who is inside, so
		// cannot safely offer anything. Congestion, which waits and retries.
		return nil, fmt.Errorf("%w: could not read lane occupancy: %v", ErrNoShuffleSlot, oErr)
	}

	blocked, hErr := db.SlotsBlockedByHardClaims(groupID)
	if hErr != nil {
		// Cannot tell what is protected, so cannot safely offer anything. Reported
		// as congestion (which waits and retries) rather than geometry (which kills
		// the order) — the same disposition every other shortfall here takes.
		return nil, fmt.Errorf("%w: could not read hard claims: %v", ErrNoShuffleSlot, hErr)
	}

	// AND THE MIRROR OF IT: slots whose use would seal an EMPTY slot deeper in
	// the same lane that somebody is driving to fill. The burial set protects a
	// bin a robot is coming to collect; this protects a slot a robot is coming to
	// fill, and the cost of getting it wrong is worse — a walled bin can still be
	// dug out, a walled empty slot is capacity gone for the life of the plant
	// (nothing is in anybody's way, so no dig is ever raised against it).
	//
	// Read once, beside the burial set, for the same reason.
	entombing, eErr := db.SlotsThatWouldEntombASpokenForSlot(groupID)
	if eErr != nil {
		return nil, fmt.Errorf("%w: could not read spoken-for slots: %v", ErrNoShuffleSlot, eErr)
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
		if !c.Enabled || c.IsSynthetic || exclude[c.ID] {
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
		// ONE ROBOT PER LANE, ASKED HERE AND NOT ONLY AT ADMISSION.
		//
		// Admission already refuses a destination in a lane somebody is inside
		// (CauseLaneOccupied). Asking it only there is not enough, because of what
		// the resolver does with a refusal: it excludes that SLOT and asks again,
		// and the next candidate is the next slot shallower IN THE SAME LANE.
		// Placing there buries the deeper slot it just walked past.
		//
		// The closing run left LS_C4 — a four-slot empty lane, the group's whole
		// spare capacity — as X X . X, with two digs having taken turns in it.
		// Dropping the lane at the source sends the resolver to a DIFFERENT lane
		// instead of to a worse slot in this one.
		if occupied[c.ID] {
			occupiedSkipped = append(occupiedSkipped, c)
			continue
		}
		// ── AND THE MOUTH IS ASKED, WHICH IT WAS NOT (§R.96 stage 2) ──────
		//
		// The pool already dropped lanes held by a foreign DIG — in SQL, inside
		// ListChildNodesUnlocked, whose exclusion clause reads `mode = 'dig'` and
		// nothing else. That was the whole consultation, and it is half a question:
		// admitMouth's rule is that a dig excludes everyone and IS EXCLUDED BY
		// EVERYONE, either side. A lane carrying an ordinary inbound hold — a store
		// on its way in — refuses this dig at admission and was still being offered
		// here as parking.
		//
		// What that costs is not a refusal; it is a WORSE SLOT. The resolver takes
		// the refusal, drops that slot, and walks on to the next candidate, which is
		// the next slot shallower in the same lane — the LS_C4 shape, arrived at
		// through the mouth instead of through occupancy. So the lane leaves the
		// pool at the source, exactly as Hold B does one line above.
		//
		// DigAdmissible is the cheap pre-check with admitMouth's own rule, read off
		// the same rows, asked on this dig's behalf — so a lane the requester itself
		// holds does not refuse its own rescue (the 16,947 exemption).
		//
		// SKIPPED on the shortfall re-walk, and only there — see consultTheMouth.
		if askTheMouth {
			// NIL IS RIGHT HERE, AND IT IS A DIFFERENT QUESTION — but the physics
			// argue the other way, so the reason is recorded rather than assumed.
			// The dig-lock sites ask "may this excavation take the lane it must dig";
			// this asks "is lane c a good place to PARK A BLOCKER", and a lane with a
			// robot queued at its mark is a worse parking spot whether or not that
			// robot is in the corridor yet. Exempting here would widen the shuffle
			// pool, which is e2352c32's subject and carries its own measurement.
			//
			// This is NOT the pre-check half of a pre-check/acquire pair, which is
			// what would make a disagreement the 16,947 shape: the acquire that
			// follows takes the PARK lane as ModeInbound, a mode admitMouth never
			// consults the exemption for. The two answers are about different lanes
			// and different modes, so they cannot contradict each other here.
			//
			// The honest counter-argument, left for whoever measures it: a robot in
			// the group's staging area obstructs a blocker being parked exactly as
			// little as it obstructs an excavation, so the same fact says the same
			// thing here. Threading it would only ADD candidates, and this function's
			// own note says a refusal costs a worse slot rather than a wedge. It is
			// left nil because widening the pool is a change with its own population
			// and its own run, not because the physics differ.
			admissible, mErr := reservations.DigAdmissible(db.DB, c.ID, asker, nil)
			switch {
			case mErr != nil:
				// ── FAIL CLOSED, AND "CANNOT SEE" IS NOT "FULL" ───────────
				//
				// A lane whose mouth could not be read is not a candidate. Offering
				// it would be sending a robot into a corridor whose ownership is
				// unknown, which is the one direction this file never takes.
				//
				// But it is not a full group either, and that is the whole point of
				// counting it separately: "the group is full" sends somebody to make
				// room, and there may be a whole empty lane behind the read that
				// failed. The shortfall below reports the two apart.
				unseen = append(unseen, c)
				continue
			case !admissible:
				mouthHeld = append(mouthHeld, c)
				continue
			}
		}
		slots, err := db.ListLaneSlots(c.ID)
		if err != nil {
			unreadable++
			continue
		}
		for i := len(slots) - 1; i >= 0; i-- {
			slot := slots[i]
			if !slot.Enabled || exclude[slot.ID] {
				continue
			}
			// IN FRONT OF A BIN SOMEBODY IS COMING FOR. Strictly shallower is the
			// whole test: deeper slots in the same lane are behind it and cannot
			// bury it, so they stay usable. The set is computed once above, by the
			// same clause the store selector uses.
			if blocked[slot.ID] {
				continue
			}
			// AND NOT IN FRONT OF AN EMPTY SLOT SOMEBODY IS DRIVING TO. Parking
			// here would seal it, and a sealed empty slot is not recoverable the
			// way a sealed bin is: nothing stands in anybody's way, so no dig is
			// ever raised against it. LSD_003 on the lane-stress rig 2026-08-13 is
			// this exactly — a dig leg parked at d2 while order 57 was still
			// driving to d3.
			if entombing[slot.ID] {
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
		// WHICH SHORTFALL IS THIS? "The group is full" and "the group has room, but
		// only inside a lane another dig is holding" are different investigations
		// and clear on different events, so they must not share a cause (law 8).
		// The exclusion happens in SQL, so this is the only place that can tell:
		// re-run this same walk against the unfiltered group and see whether the
		// lanes right of way removed would have made the count. Refusal path only —
		// the happy path still runs one query.
		if held := digHeldParking(db, laneID, groupID, children, count, len(available), asker, exclude); held != nil {
			return nil, held
		}
		// AND THE SAME QUESTION FOR HOLD B. Dropping occupied lanes at the source is
		// what stops the resolver falling back to a shallower slot in a lane it was
		// just refused from — but a lane silently missing from the pool would leave
		// the waiter reporting "the group is full", which sends an operator to make
		// room when what is happening is that a robot is in the way and about to
		// leave. Different releaser, so a different cause (law 8).
		if len(occupiedSkipped) > 0 {
			return nil, &LaneOccupiedParkingError{Lane: occupiedSkipped[0].Name, Short: count - len(available)}
		}
		// AND THE MOUTH'S TWO, IN THAT ORDER. A lane somebody OWNS is a wait with a
		// named releaser; a lane nobody could READ is a wait with no releaser at
		// all, which is the worse finding and the one an engineer has to go and
		// look at. Reporting either as "the group is full" sends an operator to
		// make room that already exists (law 8: different releaser, different
		// cause).
		if len(mouthHeld) > 0 {
			return nil, &LaneMouthHeldParkingError{
				Lane:  mouthHeld[0].Name,
				Short: count - len(available),
			}
		}
		if len(unseen) > 0 {
			names := make([]string, 0, len(unseen))
			for _, c := range unseen {
				names = append(names, c.Name)
			}
			return nil, &MouthUnreadableError{Lanes: names, Short: count - len(available)}
		}
		detail := ""
		if unreadable > 0 {
			detail = fmt.Sprintf(" (%d candidate(s) unreadable, treated as unusable)", unreadable)
		}
		return nil, fmt.Errorf("%w: need %d shuffle slots but only %d available%s", ErrNoShuffleSlot, count, len(available), detail)
	}
	return available, nil
}

// ErrLaneOccupiedParking is Hold B refusing: the parking this dig needs is in a
// lane a robot is currently inside.
//
// TRANSIENT, and the shortest-lived of the parking refusals — it clears when that
// robot places its bin and drives out, which is a matter of seconds rather than
// the length of another dig. That is exactly why it must not be reported as
// ErrNoShuffleSlot: "the group is full" sends somebody to make room, and nothing
// needs making.
var ErrLaneOccupiedParking = errors.New("the parking this dig needs is in a lane a robot is inside")

// LaneOccupiedParkingError names the lane that has to clear.
type LaneOccupiedParkingError struct {
	Lane  string
	Short int
}

func (e *LaneOccupiedParkingError) Error() string {
	return fmt.Sprintf("%s: %s is occupied and this dig is %d slot(s) short without it",
		ErrLaneOccupiedParking, e.Lane, e.Short)
}

func (e *LaneOccupiedParkingError) Unwrap() error { return ErrLaneOccupiedParking }

// ErrLaneMouthHeld is the mouth refusing: the parking this dig needs is in a lane
// whose entry another order owns.
//
// TRANSIENT, with a releaser as real as Hold B's — that order finishes with the
// lane and the row goes. It is kept apart from ErrLaneOccupiedParking because
// they are different facts about different moments: occupancy is a robot INSIDE
// the corridor, a mouth hold is an order that owns the right to enter it and may
// not have set off yet. Same shape of wait, different thing to go and look at.
var ErrLaneMouthHeld = errors.New("the parking this dig needs is in a lane whose mouth another order holds")

// LaneMouthHeldParkingError names the lane whose mouth has to free.
type LaneMouthHeldParkingError struct {
	Lane  string
	Short int
}

func (e *LaneMouthHeldParkingError) Error() string {
	return fmt.Sprintf("%s: %s is held at the mouth and this dig is %d slot(s) short without it",
		ErrLaneMouthHeld, e.Lane, e.Short)
}

func (e *LaneMouthHeldParkingError) Unwrap() error { return ErrLaneMouthHeld }

// ErrMouthUnreadable is "I CANNOT SEE", and it is deliberately not "the group is
// full" (§R.96 stage 2: fail-closed, and "cannot see" ≠ "full").
//
// The two get confused because they produce the same shortfall, and confusing
// them is expensive in one specific direction: "full" is an instruction to an
// operator to go and make room, and there may be a whole empty lane sitting
// behind a read that failed. This says the pool could not be counted, names the
// lanes it could not count, and waits.
//
// TRANSIENT, like every other shortfall here — a read that failed once is
// retried on the next firing. What it must never be is TERMINAL: a database
// hiccup is not a fact about the plant's geometry, and geometry is the only
// thing in this file that kills an order.
var ErrMouthUnreadable = errors.New("the parking pool could not be counted: a lane's mouth could not be read")

// MouthUnreadableError names the lanes that went unseen.
type MouthUnreadableError struct {
	Lanes []string
	Short int
}

func (e *MouthUnreadableError) Error() string {
	return fmt.Sprintf("%s: %d slot(s) short with %d lane(s) unread (%s) — this is NOT a full group",
		ErrMouthUnreadable, e.Short, len(e.Lanes), strings.Join(e.Lanes, ", "))
}

func (e *MouthUnreadableError) Unwrap() error { return ErrMouthUnreadable }

// ErrDigHoldsTheParking is right of way refusing: this dig could park its
// blockers if it were allowed into a lane another dig holds, and it is not.
//
// TRANSIENT, and it is the more specific sibling of ErrNoShuffleSlot rather than
// a fault — same disposition (wait and retry), different releaser. Callers that
// only need "congestion, park it" may test either; callers that name a cause to
// an operator must test this one FIRST, because it names an order.
// It read "…is inside a lane another dig holds". Since §R.101 the holder is not
// always a dig — see DigParkingHeldError.HolderIsExcavation.
var ErrDigHoldsTheParking = errors.New("the parking this dig needs is inside a lane another order holds")

// DigParkingHeldError carries who is holding the parking, so the wait can name
// its releaser instead of describing a shortage.
type DigParkingHeldError struct {
	Lane     string // a lane that would have satisfied the count, held by another order
	HolderID int64  // the order holding it, or 0 if it could not be read back
	Short    int    // how many slots short the pool came
	// HolderIsExcavation says which KIND of hold removed the lane, and the two
	// have different releasers: a reshuffle finishes, a demand's robot carries one
	// bin out. Before §R.101 only a dig could take mode='dig', so this sentence
	// said "held by dig %d" unconditionally — and then said it about every
	// ordinary retrieve sourcing from the lane. Measured on the rig this session:
	// a plain source lock produced "lane PROBE2-SIB is held by dig 2".
	HolderIsExcavation bool
}

func (e *DigParkingHeldError) Error() string {
	holder := "a dig"
	if !e.HolderIsExcavation {
		holder = "a demand sourcing from it"
	}
	if e.HolderID != 0 {
		return fmt.Sprintf("%s: %d slot(s) short, and lane %s is held by %s (order %d)",
			ErrDigHoldsTheParking, e.Short, e.Lane, holder, e.HolderID)
	}
	return fmt.Sprintf("%s: %d slot(s) short, and lane %s is held by %s",
		ErrDigHoldsTheParking, e.Short, e.Lane, holder)
}

func (e *DigParkingHeldError) Unwrap() error { return ErrDigHoldsTheParking }

// parkingLaneOf digs the refused lane's name back out for the queue params,
// falling back to the lane being excavated when the error is not the typed one.
//
// The fallback is never silently wrong: a caller only reaches it when the
// outcome already said right of way refused, so a missing name means the typed
// error was lost in a wrap, and naming the dug lane is a poorer message rather
// than an incorrect one.
func parkingLaneOf(err error, fallback string) string {
	var held *DigParkingHeldError
	if errors.As(err, &held) && held.Lane != "" {
		return held.Lane
	}
	return fallback
}

// digHeldParking answers "would the pool have been wide enough without right of
// way", and names the lane the rule removed if so. Returns nil when the shortfall
// is an honestly full group — the caller then reports ErrNoShuffleSlot.
//
// IT ASKS BY RUNNING THE WALK, NOT BY GUESSING FROM THE DIFF. A first cut named
// any dig-held lane in the group and called that the reason, which over-attributes:
// on a genuinely full group with one dig running, every shortfall would have been
// reported as right of way's doing, and the operator would be sent to look at a
// dig that was not the problem. The exact question is whether the UNFILTERED pool
// satisfies the count, and the only honest way to ask it is the walk itself
// (shuffleSlotsFrom) — the same one, over the wider list. Refusal path only.
//
// unlocked is what the caller already walked; the diff against the unfiltered
// group is set subtraction on ids, NOT a second spelling of the dig-lock predicate
// (that is asked once, in SQL — this reads its answer back out). The diff is used
// only to NAME the lane, after the walk has established there was one that helped.
//
// A read failure loses the better cause and nothing else, so it is silent: the
// caller falls through to ErrNoShuffleSlot, which waits on a superset of the
// events this one waits on.
func digHeldParking(db *store.DB, laneID, groupID int64, unlocked []*nodes.Node, count, have int, asker reservations.DigAsker, exclude map[int64]bool) error {
	all, err := db.ListChildNodes(groupID)
	if err != nil {
		return nil
	}
	seen := make(map[int64]bool, len(unlocked))
	for _, c := range unlocked {
		seen[c.ID] = true
	}
	var removed []*nodes.Node
	for _, c := range all {
		if seen[c.ID] || c.ID == laneID || !c.Enabled {
			continue
		}
		removed = append(removed, c)
	}
	if len(removed) == 0 {
		return nil // nothing was excluded; the group really is full
	}
	// THE RE-WALK CANNOT RECURSE PAST ONE LEVEL, and that is structural rather than
	// lucky: it is handed `all`, so if it comes up short it re-enters here with
	// unlocked == all, the diff is empty, and this returns at the len(removed)==0
	// line above without walking again.
	if _, wErr := shuffleSlotsFrom(db, laneID, groupID, all, count, asker, skipTheMouth, exclude); wErr != nil {
		return nil // the wider pool comes up short too — right of way is not the reason
	}

	// The pool WAS wide enough. Name a lane the rule removed, and the dig on it.
	c := removed[0]
	holder := int64(0)
	excavation := false
	if holds, hErr := reservations.ActiveMouthRows(db.DB, c.ID); hErr == nil {
		for _, h := range holds {
			if h.Mode == reservations.ModeDig {
				holder = h.OrderID
				excavation = reservations.IsExcavation(h.ReservedBy)
				break
			}
		}
	}
	return &DigParkingHeldError{
		Lane: c.Name, HolderID: holder, Short: count - have, HolderIsExcavation: excavation,
	}
}

// shuffleSlotFree reports whether a dig may park a blocker in this node.
//
// Shuffle slots are a GROUP-scoped shared resource, but the lane lock is keyed on
// the lane being dug (planBuriedReshuffle → laneLock.TryLock(buried.LaneID)). Two
// digs in DIFFERENT lanes therefore take different locks, both proceed, and then
// compete for the same shuffle slots. This used to test "is the node empty RIGHT
// NOW" (CountBinsByNode == 0) and nothing else — so a slot with another dig's
// blocker already in flight to it looked free. Both digs picked it, the second
// blocker landed on the first, and ApplyArrival's EvictStaleGhostBinsTx threw the
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
// ErrSlotNotInLane is the ONE genuine configuration fault the excavation planner
// can hit: a storage slot that is not a child of any lane, so there is no
// corridor to dig and nowhere to park a blocker.
//
// It is a SENTINEL rather than a bare error because of what sits next to it.
// Every other way planUnbury can fail is a DATABASE READ — and the disposition
// for those is now to WAIT (PLAN §R.45). Telling the two apart by readFailed()
// alone does not work in this direction: a plain fmt.Errorf is non-nil and is not
// sql.ErrNoRows, so the config fault would read as a stutter and park forever
// under a cause that never clears. The geometry has to name itself, and then
// everything else is free to be treated as I/O.
var ErrSlotNotInLane = errors.New("target slot is not in a lane")

var ErrNoShuffleSlot = errors.New("no free shuffle slot")
