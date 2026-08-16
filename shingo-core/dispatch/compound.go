package dispatch

import (
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// reshuffleFailDetail is shared between the parent's status update and the
// EmitOrderFailed event payload so they can't drift. Used in
// AdvanceCompoundOrder's hasFailed branch when one or more child orders
// failed and the parent must be marked failed.
const reshuffleFailDetail = "reshuffle failed: child order failed"

// ReshuffleDissolveDetail marks a child cancelled because its DIG was abandoned,
// not because anything went wrong with it.
//
// It is read in two places and must be one string in both: the terminal arm below
// tells a dissolve from a failure by it, and the engine's cancel wiring keys the
// dissolve re-drive on it. A dissolved compound reaching the failure cascade
// would terminate the demand the dissolve exists to keep alive, and a re-drive
// that fired on every cancel would advance compounds in the middle of an operator
// teardown — both are one string apart from correct, which is why there is one
// constant and no second spelling.
const ReshuffleDissolveDetail = "reshuffle dissolved: the dig's plan went stale; re-planning"

// reshuffleDissolveDetail is the in-package spelling of the same constant.
const reshuffleDissolveDetail = ReshuffleDissolveDetail

// compoundGenerations splits a compound's children into the CHAPTER STILL OPEN
// and the closed ones behind it, and reports whether anything was superseded.
//
// ── WHY THIS EXISTS ────────────────────────────────────────────────────────
//
// A parent that dissolves and re-plans accumulates children across generations,
// and ListChildOrders returns all of them at once. Every current-state question
// asked of that list then answers about two digs simultaneously. Concretely: the
// SECOND dig finishes clean, the first dig's marker-cancelled legs are still in
// the set, "was this dissolved" says yes again, and the parent is returned to the
// queue — where the scanner retries a retrieve for a bin that already left. The
// work happened, the demand never closes and never fails. Found on the rig's
// row-5.6 work; silent, permanent, and it sits on the branch's headline feature.
//
// ── THE RULE ───────────────────────────────────────────────────────────────
//
// A SUPERSEDED GENERATION IS A CLOSED CHAPTER. The parent's completion
// arithmetic reads only the open one; the demand's ledger keeps everything. Two
// questions, two scopes: "is the work done" reads the current chapter, "what did
// this demand cost the cell" reads the whole book — and the second is untouched
// here, because origin inheritance stamps every dig child with the demand's
// origin and that is the cost-of-demand record.
//
// ── THE BOUNDARY IS THE MARKER, AND IT NEEDS NO COLUMN ─────────────────────
//
// A child belongs to a closed chapter iff it is TERMINAL and its id is at or
// below the newest marker-cancelled child's. The re-plan opening a new
// generation is itself the proof the old one ended — the same grain
// CloseReasonSuperseded already uses for demand episodes.
//
// Both halves of that predicate are load-bearing:
//
//   - TERMINAL, because a dissolve cancels every leg it can and a cancel CAN be
//     refused. A leg still executing under an old plan has no marker and must
//     stay visible, or the parent re-plans a lane a live robot is still
//     changing. It joins the closed chapter when it lands, not before.
//   - AT OR BELOW THE NEWEST MARKER, rather than "carries the marker". A
//     generation that got one leg confirmed before it went stale has that
//     confirmed leg closed with the rest of its chapter. Its work stands — the
//     blockers it moved stay where they landed and the re-plan simply sees fewer
//     of them — but it is not evidence about the dig now running.
//
// Order ids, not wall-clock: they come from a monotonic sequence, which the
// order map's eviction already depends on. Sequence numbers cannot serve — they
// restart per plan, so generations interleave in a list ordered by them.
// NO MARKER MEANS NOTHING IS CLOSED, and that is checked separately from the
// boundary VALUE rather than folded into it. Treating "boundary == 0" as "no
// generation closed" reads fine and is wrong twice over: it makes the predicate
// depend on ids being positive, and it silently changes meaning for a caller
// holding un-persisted orders. `superseded` carries the fact; `boundary` only
// carries where.
func compoundGenerations(children []*orders.Order) (open []*orders.Order, superseded bool) {
	var boundary int64
	for _, c := range children {
		if c.Status == StatusCancelled && c.ErrorDetail == reshuffleDissolveDetail {
			if !superseded || c.ID > boundary {
				boundary = c.ID
			}
			superseded = true
		}
	}
	for _, c := range children {
		if superseded && protocol.IsTerminal(c.Status) && c.ID <= boundary {
			continue // a closed chapter
		}
		open = append(open, c)
	}
	return open, superseded
}

// digWasDissolved reports that the open chapter has closed and no new one has
// been opened yet — so the parent belongs back in the acquiring set for the
// scanner to re-plan.
//
// IT IS NO LONGER ARCHAEOLOGY OVER THE WHOLE FAMILY. It used to scan every child
// the parent ever had for cancels-carrying-the-marker, which is what made a
// completed second dig read as a second dissolve. It now asks one question of the
// generation split: something was superseded, and nothing is left open.
//
// THE FAILURE VETO IS DELIBERATELY STILL WHOLE-SET. A single FAILED child
// anywhere in the family vetoes the reading, exactly as before. A dissolve and a
// real fault cannot both be the honest account of the same compound, and the
// failure cascade is the honest one — narrowing the veto to the open chapter
// would let a fault in a superseded generation disappear, which is a bigger
// change than the ruling asked for and not one a scoping fix should smuggle in.
//
// The dissolve itself still cannot route the parent at dissolve time, and that is
// a constraint rather than a preference: dissolveCompound is reachable from
// inside the fulfillment scanner under a non-reentrant scanMu, so transitioning
// there would self-deadlock on the first leg of a freshly planned dig. The
// cancels land, their events fire asynchronously, and this arm — on a later
// goroutine — is where the routing happens.
func digWasDissolved(children []*orders.Order) bool {
	for _, c := range children {
		if c.Status == StatusFailed {
			return false
		}
	}
	open, superseded := compoundGenerations(children)
	return superseded && len(open) == 0
}

// CreateCompoundOrder creates a parent order with child orders for a reshuffle plan.
// All children and bin claims are created in a single transaction. The parent
// is transitioned into StatusReshuffling via lifecycle.BeginReshuffle, so the
// caller must pass a parent in a status that has Reshuffling as a legal next
// state (Pending, Sourcing, Queued). Synthetic restore parents that already
// hold StatusReshuffling at creation use CreateCompoundChildrenOnly instead.
//
// THE CHILDREN ARE WRITTEN BEFORE THE PARENT MOVES, and the order is
// load-bearing. BeginReshuffle used to run first, so a refused claim left the
// parent sitting in `reshuffling` with no compound under it — a status outside
// the acquiring set (IsAcquiring = queued|sourcing), which means the fulfillment
// scanner never looks at that order again. That was survivable only while every
// creation failure was terminal anyway. It stops being survivable the moment one
// of them becomes a WAIT (a blocker bin claimed by another order — see
// store.BlockerClaimedError): a wait that leaves the order un-scannable is a
// permanent stall, which is worse than the terminal fail it replaced.
//
// Putting the transition after the write also makes the crash window the benign
// one. Interrupted here, the parent is still queued and re-plannable; the old
// order left it reshuffling and childless, which nothing recovers.
//
// The parent must NOT be moved back out of reshuffling as a repair instead:
// {Reshuffling → Queued} fires fireRequeued, which runs the scanner
// synchronously, and the scanner is where this refusal is discovered on the
// replay path — it holds a non-reentrant scanMu, so the "repair" would deadlock.
func (d *Dispatcher) CreateCompoundOrder(parentOrder *orders.Order, plan *ReshufflePlan) error {
	if err := d.writeCompoundChildren(parentOrder, plan); err != nil {
		return err
	}
	if err := d.lifecycle.BeginReshuffle(parentOrder,
		fmt.Sprintf("reshuffling: %d steps to unbury bin %d", len(plan.Steps), plan.TargetBin.ID)); err != nil {
		log.Printf("dispatch: begin reshuffle order %d: %v", parentOrder.ID, err)
	}
	return d.AdvanceCompoundOrder(parentOrder.ID)
}

// CreateCompoundChildrenOnly creates the compound's child orders and advances
// the first one — CreateCompoundOrder minus the lifecycle.BeginReshuffle call,
// for a parent already sitting at StatusReshuffling, where the transition would
// log a spurious "illegal transition: reshuffling → reshuffling".
//
// IT HAS NO PRODUCTION CALLER. Its one caller was the synthetic restore-blockers
// parent, and that subsystem is deleted; what is left uses it are three test
// fixtures that want children written against a parent they placed at
// Reshuffling themselves. Recorded rather than removed: deleting it means either
// rewriting those fixtures to drive CreateCompoundOrder from a live status, or
// accepting the warning line, and neither is a decision this cleanup should make
// on its own. C5's report carries it.
func (d *Dispatcher) CreateCompoundChildrenOnly(parentOrder *orders.Order, plan *ReshufflePlan) error {
	if err := d.writeCompoundChildren(parentOrder, plan); err != nil {
		return err
	}
	return d.AdvanceCompoundOrder(parentOrder.ID)
}

// writeCompoundChildren builds the compound's child orders from the plan and
// writes them, with their bin claims, in the store's single transaction. It
// touches the parent's STATUS not at all — that is the caller's, and the two
// callers sequence it differently (see CreateCompoundOrder).
func (d *Dispatcher) writeCompoundChildren(parentOrder *orders.Order, plan *ReshufflePlan) error {
	var children []store.CompoundChild
	for _, step := range plan.Steps {
		// The payload the child is moving, read off the bin it names. The column
		// was blank on every child ever written, which made "how much does
		// payload X move in reshuffling" unanswerable from the orders table, and
		// gave every reshuffle move the default load sequence — the dispatcher
		// picks the robot's bin-task sequence from PayloadCode.
		//
		// A missing bin is not fatal here. The child still names the bin id and
		// the claim below still runs; only the payload label is lost, and
		// failing the whole compound over a label would trade a reporting gap
		// for a stopped reshuffle.
		var payloadCode string
		if bin, err := d.db.GetBin(step.BinID); err != nil {
			log.Printf("dispatch: compound child for bin %d: cannot read payload code: %v", step.BinID, err)
		} else if bin != nil {
			payloadCode = bin.PayloadCode
		}

		child := &orders.Order{
			// A minted identity, like every other order. It used to be BUILT from
			// the parent's — "<parent-uuid>-step-<sequence>" — which is a
			// structural name, not an identity, and re-planning a parent's steps
			// produced the same string again. That is a duplicate in the column
			// migration v71 makes unique, and the two facts cannot both hold: a
			// plant that has run reshuffling cannot apply the index, and a plant
			// that applied it stops reshuffling at the next re-plan.
			//
			// Nothing is lost, because nothing read the name. Children are found
			// by parent_order_id and ordered by sequence (ListChildOrders /
			// GetNextChildOrder); both facts are columns on this struct, four
			// lines down. The only Sscanf against an edge_uuid anywhere is the
			// restore parent's, which is a different format and keeps its
			// exemption. Verified by grep before this changed: one construction
			// site (here) and zero readers.
			//
			// The fleet still gets it as ExternalID, where it is an opaque
			// correlation token — a real UUID suits that better than a name that
			// looks parseable.
			EdgeUUID:      uuid.New().String(),
			StationID:     parentOrder.StationID,
			OrderType:     OrderTypeMove,
			Status:        StatusPending,
			ParentOrderID: &parentOrder.ID,
			Sequence:      step.Sequence,
			PayloadDesc:   fmt.Sprintf("reshuffle %s: bin %d", step.StepType, step.BinID),
			PayloadCode:   payloadCode,
			BinID:         &step.BinID,
			// How this child sources its bin: locally, by name. Children were
			// written with "", which is the DEFAULT-FULL value rather than an
			// unset one — it reads as "find any full bin of this payload,
			// plant-wide", the opposite of what a reshuffle child does. It names
			// its exact bin four lines up; there is nothing to find.
			//
			// Harmless until now only because children never reach the finder:
			// the scanner skips any order with a parent. That skip is intended
			// behavior, and TestCompoundChild_StaysOutOfTheSourceFinder pins it,
			// so a later change here cannot quietly route reshuffle work into
			// plant-wide FIFO selection and hand it a different bin than the one
			// it was planned to move.
			SourceIntent: SourceIntentForType(OrderTypeMove),
			// The derivative site — now the only one (the restore-blockers parent
			// that was the second is retired by this merge). An order created in
			// service of another order inherits its origin AND ITS CLASS: a dig
			// move exists only because the parent needed a buried bin, so it is
			// part of the cost of the parent's demand and belongs in that
			// episode's count.
			//
			// STAMPED FORWARD, NOT WALKED. parent_order_id is set four lines up,
			// which makes a read-time walk look available here, and it is still
			// the wrong instrument: it is one level deep, so it cannot reach an
			// episode from a grandchild, and it would re-derive at read time a
			// value this order was handed at creation. Copying the class too is
			// what stops a no_demand parent's digs arriving as fresh orphans.
			//
			// There WAS a second derivative site — the synthetic restore parent,
			// which set no parent_order_id at all and so dead-ended the walk
			// outright. It went with the reshuffle restore subsystem
			// (cb74bfdc); the rule it motivated is unchanged, but the sharpest
			// argument for it no longer has a live example. Do not weaken this
			// to a walk on the grounds that the remaining sites all set a
			// parent: one level is still not enough.
			OriginID:    parentOrder.OriginID,
			OriginClass: parentOrder.OriginClass,
		}

		if step.FromNode != nil {
			child.SourceNode = step.FromNode.Name
		}
		if step.ToNode != nil {
			child.DeliveryNode = step.ToNode.Name
		}
		// Simple-retrieve reshuffles emit a "retrieve" step with no
		// ToNode and rely on this fallback to land the bin at the
		// parent retrieve's lineside DeliveryNode.
		//
		// Complex-order reshuffles do NOT go through this branch:
		//   - Expose mode (PlanReshuffleUnburyOnly) emits no retrieve
		//     step at all — the complex parent resumes and runs its
		//     own pickup against the now-accessible original slot.
		//   - Target-node mode (PlanReshuffleToTarget) sets ToNode
		//     explicitly so DeliveryNode is non-empty when we reach
		//     this check and the fallback never fires.
		//
		// Adding a "retrieve" step to PlanReshuffleUnburyOnly without
		// a ToNode would silently inherit parentOrder.DeliveryNode,
		// which for a complex parent is the *last* step's node
		// (extractEndpoints in complex_steps.go) — wrong destination
		// for the unbury step's deliverable. Don't.
		if step.StepType == protocol.StepRetrieve && child.DeliveryNode == "" {
			child.DeliveryNode = parentOrder.DeliveryNode
		}

		children = append(children, store.CompoundChild{Order: child, BinID: step.BinID})
	}

	// %w, not %v: store.BlockerClaimedError has to survive this wrap for the
	// planners to tell a congestion refusal from a fault.
	if err := d.db.CreateCompoundChildren(children); err != nil {
		return fmt.Errorf("create compound children: %w", err)
	}
	return nil
}

// AdvanceCompoundOrder dispatches the next pending child order in a compound sequence.
func (d *Dispatcher) AdvanceCompoundOrder(parentOrderID int64) error {
	// THE SIBLING-IN-FLIGHT GUARD IS GONE. It used to refuse to advance while any
	// sibling was non-pending and non-terminal, and it was carrying two
	// properties fused into one condition:
	//
	//   1. exactly-once dispatch per child — load-bearing, and accidental
	//   2. one child at a time — the serialization being retired here
	//
	// (1) now stands on its own, below: a child that already carries a
	// VendorOrderID is never dispatched again, keyed on a durable witness rather
	// than on what its siblings happen to be doing. That separation landed
	// first, deliberately, because removing this loop with the two still fused
	// would dispatch every child twice — fireCompleted fires on BOTH
	// (*, Delivered) and (Delivered, Confirmed), so this function re-enters
	// across one sibling's lifecycle. The 2026-05-27 incident was one robot's
	// worth of work dispatched three times, not three legitimate legs.
	//
	// (2) is replaced by the LANE gate below, which is a different and narrower
	// rule: a child waits for the lane it needs to be empty, not for its
	// siblings to be finished. THAT IS THE WHOLE GAIN. An unbury leg releases its
	// occupancy when it PLACES its blocker, while it is still driving back and
	// still non-terminal — so the next leg enters the lane during that return
	// trip instead of after it. Several children in flight, one inside.
	next, err := d.db.GetNextChildOrder(parentOrderID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		// A real DB error (connection drop, query/scan failure) is NOT the same
		// as "no more pending children". Bail without transitioning the parent or
		// releasing the lane so the next completion/failure event retries —
		// instead of prematurely completing/failing/resuming the parent (and
		// unlocking the lane) while child reshuffle steps are still queued, the
		// 2026-05-27 three-robots-in-one-corridor failure class.
		log.Printf("dispatch: get next child for compound %d: %v", parentOrderID, err)
		return err
	}
	if err != nil {
		// sql.ErrNoRows — no more PENDING children. But "not pending" doesn't mean "done".
		// Children that are dispatched / in_transit / staged / delivered are
		// in flight. We only confirm or fail the compound parent when every
		// child has reached a terminal status (confirmed / failed / cancelled).
		// Without this check, redundant child-completion events (sim FINISHED
		// + HandleOrderReceipt firing back-to-back) can advance through the
		// pending children fast enough that this branch runs before any
		// child has actually confirmed — and CompleteCompound then races
		// ahead of the still-in-flight legs, leaving the lifecycle gate to
		// reject a later child failure with `confirmed -> failed`.
		children, listErr := d.db.ListChildOrders(parentOrderID)
		if listErr != nil {
			log.Printf("dispatch: list children for compound %d: %v", parentOrderID, listErr)
		}
		// A child that FAILED or was CANCELLED means the reshuffle's housekeeping did
		// NOT complete, so the parent must fail — NOT take the success branch below.
		// Owner-visible decision (2026-07-09): a cancelled reshuffle leg fails the
		// compound. Rationale: a coordinated parent that resumed would re-run its
		// original pickup against a still-buried bin (re-reshuffle / livelock risk);
		// a plain-retrieve parent that "completed" would be wrongly marked Confirmed
		// though its retrieve never ran. A cancelled leg reaches here from the
		// AbandonStuck sweep, a fleet-fault sibling teardown, or (with the liveness
		// backstop) a lone cancelled last child. SKIPPED stays success — a skipped
		// leg is a moot no-op, not an incomplete one. Revisit if a legitimately
		// skippable-but-cancelled leg is ever introduced.
		//
		// SCOPED TO THE OPEN CHAPTER. All three current-state questions below —
		// did anything go wrong, is everything finished, and which mode was
		// planned — read the generation still in play, not every child the parent
		// has ever had. A superseded generation's legs are cancelled ON PURPOSE;
		// counting them here is what let a re-planned dig read as a failed one.
		// One split, four readers, so the four cannot drift apart again.
		openChildren, _ := compoundGenerations(children)
		hasFailedOrCancelled := false
		allTerminal := true
		for _, c := range openChildren {
			if c.Status == StatusFailed || c.Status == StatusCancelled {
				hasFailedOrCancelled = true
			}
			if !protocol.IsTerminal(c.Status) {
				allTerminal = false
			}
		}

		// Load parent for both branches.
		parent, pErr := d.db.GetOrder(parentOrderID)
		if pErr != nil {
			log.Printf("dispatch: load parent compound order %d: %v", parentOrderID, pErr)
		}

		// THE PARENT HAS ALREADY LEFT. Every disposition below writes a transition
		// out of `reshuffling` — Fail, CompleteCompound, ResumeCompound all assume
		// the parent is still in it — so a parent that has moved on is one this
		// compound no longer speaks for.
		//
		// Two ways that happens, and neither is an error: an operator cancelled the
		// parent (cancelCompoundChildren cancelled the legs; a late child completion
		// arrives here afterwards), or a dissolve already returned it to the
		// acquiring set and it is re-planning. Without this guard the first case
		// tried to Fail a cancelled order — rejected by the state machine, logged as
		// an error, harmless but wrong — and the second would FAIL A LIVE
		// RE-PLANNING ORDER, which is the demand the dissolve just saved.
		if parent != nil && parent.Status != StatusReshuffling {
			d.dbg("dispatch: compound %d is finished with, its parent is %s — nothing to dispose of",
				parentOrderID, parent.Status)
			return nil
		}

		// DISSOLVED, NOT FAILED. Checked before the cascade because a dissolve
		// cancels legs on purpose and would otherwise read as a leg going wrong —
		// one string apart, opposite outcomes for the demand.
		//
		// The parent goes back to the acquiring set and the scanner re-plans from
		// live lane state: a plain parent through the finder's buried outcome, a
		// coordinated one through its own replay. Either way the new plan contains
		// the blocker that made the old one stale, which is the whole point.
		//
		// Reshuffling → Queued for BOTH kinds here, unlike the success arm below
		// that splits them. A dissolved dig is not a completed one: a plain parent
		// has NOT been retrieved, so Confirmed would be a lie.
		if digWasDissolved(children) {
			// WAIT FOR THE LEGS THAT ARE STILL MOVING. A dissolve cancels every
			// non-terminal leg, so all-terminal is the ordinary case here — but a
			// cancel can be refused (an illegal transition from some state), and
			// returning the parent while a robot is still executing an old leg would
			// have it re-plan a lane a stale leg is still changing. The next
			// terminal event brings us back.
			if !allTerminal {
				d.dbg("dispatch: dissolved compound %d still has a leg in flight — waiting for it to land",
					parentOrderID)
				return nil
			}
			// SEALEDNESS IS NOT CONSULTED, and that is a statement rather than an
			// omission: OpenForChildren asks "are more legs coming", and for a
			// dissolved dig the answer is no by construction — the dissolve cancelled
			// the set and no writer adds to it. (Nothing opens a compound today; when
			// the fold makes that real, a dissolve must also seal the parent, or the
			// re-plan inherits an open marker and this arm stops being reachable.)
			if parent != nil {
				if err := d.lifecycle.Queue(parent, "dispatcher", reshuffleDissolveDetail); err != nil {
					log.Printf("dispatch: dissolved compound %d could not return its parent to the "+
						"acquiring set: %v (the demand is stranded in reshuffling; the reconciliation "+
						"sweep is the backstop)", parentOrderID, err)
				}
			}
			// Idempotent — the dissolve released it already. Repeated because this
			// arm is also reachable when a leg that was already in flight terminates
			// after the dissolve, and a lane held past a re-plan is a wedge.
			d.unlockLaneForCompound(parentOrderID)
			return nil
		}

		if hasFailedOrCancelled {
			log.Printf("dispatch: compound order %d has failed/cancelled children — marking parent failed", parentOrderID)
			if parent != nil {
				if err := d.lifecycle.Fail(parent, parent.StationID, "reshuffle_failed", reshuffleFailDetail); err != nil {
					log.Printf("dispatch: fail compound order %d: %v", parentOrderID, err)
				}
			}
			d.unlockLaneForCompound(parentOrderID)
			return nil
		}

		// In-flight children remain. Wait for the next real completion or
		// failure event to call us back. CompleteCompound below would
		// otherwise transition the parent to Confirmed prematurely.
		if !allTerminal {
			return nil
		}

		// SEALEDNESS. Everything above asked "is anything running right now" and
		// "did anything go wrong"; from here down the question is "is this
		// reshuffle FINISHED", and that is the only one of the three that a
		// terminal child set cannot answer on its own.
		//
		// It answers it today because every compound writes all of its children in
		// one transaction, so no-pending-children means no-more-children. Under the
		// fold that stops being true — a reshuffle commits one move at a time, and
		// all-terminal becomes the ordinary state BETWEEN moves. Completing here
		// would finish a half-dug lane and release its dig lock with blockers still
		// standing in it.
		//
		// PLACED HERE AND NOT AT THE TOP OF THIS BLOCK, which is the part worth
		// reading twice. The two arms above are asking the other question and must
		// keep running for an open compound: a failed leg still fails the whole
		// reshuffle, and in-flight legs are still worth waiting for. A guard at the
		// top would be wrong in both directions at once.
		//
		// RETURNS BEFORE THE LANE HANDLING BELOW, deliberately. An open compound is
		// mid-dig and still needs its lane; releasing it between moves is the
		// re-burial window Hold A exists to close.
		//
		// Narrow on purpose: only a parent we actually READ and that actually says
		// open. A nil parent (load error) stays on the pre-existing path rather
		// than acquiring a new fail-closed arm here — that path already handles an
		// unreadable parent, badly but consistently, and widening it is a separate
		// decision from this one.
		//
		// No-op today: nothing opens a compound yet, so every parent reaching this
		// line is sealed. That is what makes it safe to land before the fold rather
		// than during it.
		if parent != nil && parent.OpenForChildren {
			d.dbg("dispatch: compound %d has no children running but is still OPEN — not finishing it "+
				"(the dig continues; its lane stays held)", parentOrderID)
			return nil
		}

		// All children reached a terminal status with none failed -> compound
		// order is complete. Route on whether the parent has its OWN work to
		// resume after the reshuffle — Stage 4 keys this on the coordinated-plan
		// signal (IsCoordinated == parent carries a step plan), not OrderType:
		//   - coordinated parent (a complex order carries StepsJSON): a buried-bin
		//     reshuffle whose parent still owes its original pickup. ResumeCompound
		//     transitions Reshuffling → Queued so the scanner re-resolves that
		//     pickup against the now-accessible slot. Do NOT call CompleteOrder —
		//     the parent hasn't finished, it's resuming.
		//   - plain parent (simple-retrieve compounds, restock compounds for the
		//     dual-mode reshuffle — no step plan): CompleteCompound transitions
		//     Reshuffling → Confirmed and fires fireCompleted.
		//
		// Sequencing dependency on fulfillment.RunOnce being synchronous
		// — see lifecycle.go's {Reshuffling, Queued} actionMap entry.
		//
		// Lane-lock handling (v7 Step 4.5):
		//   - target-node mode (PlanReshuffleToTarget): unlock immediately
		//     — the target bin has already moved out of the lane, no
		//     re-burial risk.
		//   - expose mode (PlanReshuffleUnburyOnly): TRANSFER the lock
		//     from the compound parent to the complex parent, register a
		//     listener that releases on EventBinEnteredTransit for the
		//     target bin or on parent cancel/fail. Closes the
		//     post-compound / pre-pickup re-burial window.
		//   - non-complex parents (simple-retrieve, restore): unlock
		//     immediately (existing behavior).
		if parent != nil {
			if IsCoordinated(parent) {
				if err := d.lifecycle.ResumeCompound(parent); err != nil {
					log.Printf("dispatch: resume compound order %d: %v", parentOrderID, err)
				}
			} else {
				if err := d.lifecycle.CompleteCompound(parent); err != nil {
					log.Printf("dispatch: confirm compound order %d: %v", parentOrderID, err)
				}
				if err := d.db.CompleteOrder(parentOrderID); err != nil {
					log.Printf("dispatch: complete compound order %d: %v", parentOrderID, err)
				}
			}
		}

		if parent != nil && IsCoordinated(parent) && planUsedExposeMode(openChildren) {
			d.extendLaneLockForExposeMode(parentOrderID, parent, children)
		} else {
			d.unlockLaneForCompound(parentOrderID)
		}
		return nil
	}

	// Dispatch the child to fleet
	if next.SourceNode == "" || next.DeliveryNode == "" {
		if err := d.db.FailOrderAtomic(next.ID, "missing source or delivery node"); err != nil {
			log.Printf("dispatch: atomic fail child order %d: %v", next.ID, err)
		}
		return d.AdvanceCompoundOrder(parentOrderID)
	}

	// THE SAME THREE-WAY SPLIT the reshuffle planners make (read_vs_missing.go),
	// applied to the two node reads a leg cannot proceed without. Both used to
	// fail the leg on any error, so a database that did not answer killed a leg —
	// and a failed leg fails the whole dig and the demand behind it, which makes
	// this the most expensive place in the family to get wrong.
	//
	// Releaser for the hold: the leg stays `pending`, which is what
	// GetNextChildOrder selects, so the next lane-clearing redrive or completion
	// event brings it straight back.
	sourceNode, err := d.db.GetNodeByDotName(next.SourceNode)
	if readFailed(err) {
		log.Printf("dispatch: compound %d child %d — could not read source node %q: %v (holding the child)",
			parentOrderID, next.ID, next.SourceNode, err)
		d.setQueueReason(next, protocol.QueueWaitingForSlot, CauseReadFailed, QueueParams{})
		return nil
	}
	if err != nil || sourceNode == nil {
		if dbErr := d.db.FailOrderAtomic(next.ID, configFailure("source node", next.SourceNode)); dbErr != nil {
			log.Printf("dispatch: atomic fail child order %d: %v", next.ID, dbErr)
		}
		return d.AdvanceCompoundOrder(parentOrderID)
	}

	destNode, err := d.db.GetNodeByDotName(next.DeliveryNode)
	if readFailed(err) {
		log.Printf("dispatch: compound %d child %d — could not read delivery node %q: %v (holding the child)",
			parentOrderID, next.ID, next.DeliveryNode, err)
		d.setQueueReason(next, protocol.QueueWaitingForSlot, CauseReadFailed, QueueParams{})
		return nil
	}
	if err != nil || destNode == nil {
		if dbErr := d.db.FailOrderAtomic(next.ID, configFailure("delivery node", next.DeliveryNode)); dbErr != nil {
			log.Printf("dispatch: atomic fail child order %d: %v", next.ID, dbErr)
		}
		return d.AdvanceCompoundOrder(parentOrderID)
	}

	// MAY THIS MOVE HAPPEN NOW. One decision, asked in one place — this used to
	// ask only "is anyone inside the lane" (laneOccupiedForChild, now deleted),
	// which is a strict subset of the question and was never the whole of it.
	//
	// A subset was safe while a dig excluded everyone else from the lane by
	// construction, so the answers it did not ask for could not come back no.
	// Under the fold that stops holding: this runs per move rather than once per
	// compound, against a lane that other work has had time to change.
	//
	// What delegating ADDS here, all three of them behaviour:
	//
	//   - a lane that cannot be RESOLVED now holds the child. lanesFor logged the
	//     failure and continued, so an unresolvable lane contributed no checks and
	//     the leg went out against a lane nothing had looked at.
	//   - a foreign dig on the DESTINATION lane now holds it. Occupancy alone
	//     misses this: a dig claims a lane for a whole reshuffle without anyone
	//     being inside it at that instant, so a leg could place its blocker into a
	//     lane another reshuffle owns. Plan-time destination filtering
	//     (ListChildNodesUnlocked) excluded dig-held lanes when the plan was
	//     built, which is exactly the guarantee the fold removes.
	//   - a bin that has become unreachable now holds the leg instead of sending
	//     a robot to a slot behind another bin.
	//
	// The dig on the leg's OWN parent still admits -- ownsDig (lane_gate.go)
	// routes the leg's question to its parent, brief 3's defect 1. Without that
	// exemption a leg parks behind the lock that only its own completion clears.
	//
	// Staying pending is not a failure and not a retry: the sibling's dropoff
	// completion releases its occupancy and re-enters this function, so the wait
	// is exactly as long as the lane is busy.
	// No skip set: a compound leg asks every physical question, which is the
	// zero value. Declaring it would be noise; forgetting it is safe by design
	// (admissionSkips).
	v, err := d.admit(admissionSituation{order: next, sourceNode: sourceNode, destNode: destNode})
	if err != nil {
		// FAIL CLOSED. An unreadable lane is a busy lane: refusing costs a retry
		// on the next event, and dispatching into a lane whose state could not be
		// read costs a collision.
		//
		// AND THE CAUSE GOES ON THE ROW, exactly as the refusal arm below does it.
		// This arm held the child and wrote nothing, so a leg stalled on a lane
		// Core could not READ looked identical to a leg nobody had reached yet —
		// and the two are investigated differently. The distinct cause is the
		// point: an undetermined answer is Core declining, not a busy lane.
		log.Printf("dispatch: admission for compound %d child %d: %v (holding the child)",
			parentOrderID, next.ID, err)
		d.setQueueReason(next, protocol.QueueWaitingForSlot, CauseAdmissionError,
			QueueParams{Destination: destNode.Name})
		return nil
	}
	if !v.Admitted() {
		// A REACHABILITY REFUSAL IS NOT A WAIT — it is a plan that went stale.
		//
		// Occupancy and a foreign dig both self-clear: somebody is inside, or
		// somebody owns the lane, and when they leave the lane-clearing redrive
		// re-admits this leg. Reachability is different in kind. It means a bin
		// landed in front of this leg's pickup AFTER the dig was planned, and the
		// dig's blocker list was written once, at planning. Nothing in this
		// compound will ever move that bin, because it is not in the plan — and the
		// demand that would plan a dig for it is the parent, imprisoned in
		// `reshuffling` inside this very compound. The leg holds forever.
		//
		// So the disposition keys on whether anyone is coming for the obstruction.
		if v.Cause() == CauseLaneTargetBuried {
			return d.handleStaleDigLeg(parentOrderID, next, sourceNode, destNode)
		}
		d.dbg("dispatch: compound %d child %d held at its lane (%s)", parentOrderID, next.ID, v.Cause())
		// THE WAIT GOES ON THE ROW. Every other wait in the system records why it
		// is waiting; this one wrote nothing at all — no status (the leg stays
		// `pending`), no queue_code, no queue_cause — so its only trace was a debug
		// log that is nil unless DebugLog is wired. A held leg was therefore
		// indistinguishable from a leg nobody had looked at yet, on the row an
		// operator and every diagnostic query actually read.
		//
		// The status deliberately does NOT move. `pending` is what makes the leg
		// re-drivable — GetNextChildOrder selects it, and no transition out of
		// sourcing goes back — so the cause is written ALONGSIDE the status rather
		// than instead of it. Advisory metadata, never a gate.
		d.setQueueReason(next, protocol.QueueWaitingForSlot, v.Cause(),
			QueueParams{Destination: destNode.Name})
		return nil
	}

	// Admitted — clear any cause a previous pass left, so the row does not keep
	// claiming a wait that has ended.
	d.setQueueReason(next, "", "", QueueParams{})

	// EXACTLY-ONCE, per child, independent of any serialization.
	//
	// This is the property the sibling-in-flight guard above has been carrying
	// as a side effect, and it is the load-bearing half of that guard.
	// fireCompleted fires on BOTH (*, Delivered) and (Delivered, Confirmed), so
	// AdvanceCompoundOrder re-enters across one sibling's lifecycle; the
	// createCompound→advanceCompound path used to add a second entry within
	// milliseconds of creation. Those are re-entries, not parallelism — the
	// 2026-05-27 incident was ONE robot's worth of work dispatched three times.
	//
	// VendorOrderID is the durable witness: it is non-empty once and only once a
	// child has been handed to the fleet, it survives a crash, and it does not
	// depend on what any sibling is doing. Re-reading it here rather than
	// trusting `next` closes the window between GetNextChildOrder and this line.
	//
	// Deliberately belt-and-braces for now: the sibling loop still serializes, so
	// this guard should never fire, and nothing about the plant's behaviour
	// changes today. It is stated separately so that when the serialization half
	// is removed, exactly-once does not go with it.
	if fresh, rErr := d.db.GetOrder(next.ID); rErr != nil {
		log.Printf("dispatch: re-read child %d before dispatch: %v", next.ID, rErr)
		return rErr // fail closed: an unverifiable child is not dispatched
	} else if fresh.VendorOrderID != "" {
		log.Printf("dispatch: child order %d already dispatched as %s — refusing a second dispatch",
			next.ID, fresh.VendorOrderID)
		return nil
	}

	// Hold B: this child is now inside the lane(s) it was sent into. Recorded
	// before the fleet call, so the fact exists from the instant Core commits to
	// it rather than from whenever the fleet answers.
	//
	// AND BEFORE THE STATUS MOVE, which is what makes the failure arm survivable.
	// The read above fails closed by holding the child; this has to hold it the
	// same way, and a child can only be held while it is still `pending` —
	// GetNextChildOrder selects `status='pending'`, so a child parked at
	// `sourcing` is invisible to every re-drive, and no transition out of
	// sourcing goes back to pending (protocol.validTransitions). Holding it one
	// line further down would strand the leg and leave the parent in
	// `reshuffling` forever: fail-closed on paper, wedged in fact.
	if err := d.TakeLaneOccupancy(next.ID, sourceNode, destNode); err != nil {
		log.Printf("dispatch: compound %d child %d — could not record lane occupancy: %v (holding the child)",
			parentOrderID, next.ID, err)
		return nil
	}

	// THE VERDICT IS THE POINT. transition() compare-and-swaps on the status this
	// caller loaded (lifecycle.go), so `pending → sourcing` is an ATOMIC CLAIM on
	// this child: GetNextChildOrder selects `status='pending' … LIMIT 1`, two
	// concurrent callers therefore resolve to the SAME child, and exactly one of
	// their CASes matches a row. The database has been answering this correctly
	// all along. This line logged the answer and dispatched anyway.
	//
	// Nothing downstream catches it. The loser's struct still reads `pending`, so
	// Dispatch fails with IllegalTransition (pending → dispatched is not a legal
	// edge) — while the orphan-mission guard that would cancel the vendor order is
	// scoped to IsConcurrentTransition (dispatcher.go). Wrong error type, so it
	// falls through to log-only, having already created a second real fleet order
	// and overwritten vendor_order_id with it. Two robots, one row, and the first
	// mission flying untracked: the 2026-05-27 shape, through a door the exactly-
	// once re-read cannot cover because vendor_order_id is written AFTER the fleet
	// call, not before.
	//
	// No serializer is needed here and one would be the wrong instrument — the
	// atomic operation already ran. Read its result.
	//
	// RELEASING IS SCOPED TO THE ARM THAT CAN WEDGE. Occupancy was taken above,
	// keyed on this child, and admission's occupancy arm (admitLane,
	// admission.go) refuses a lane held by any occupant other than the asker.
	// Return while that row stands and every OTHER leg reads the lane as "busy" --
	// busy with a leg that was never sent. So a FAILED STATUS WRITE must release:
	// nothing else clears it.
	//
	// A LOST CAS MUST NOT, and this used to get it backwards. The reasoning was
	// "the CAS-loss arm survives regardless (the winner consumes the row)". That
	// is false. AcquireOccupancy de-duplicates on (order_id, node_id)
	// (store/reservations/mouth.go), and two callers contending for one child
	// carry the SAME order_id — so there is exactly ONE occupancy row, inserted by
	// whichever caller arrived first, and it is the WINNER'S. There is no second
	// row for the winner to consume. ReleaseLaneOccupancy is order-keyed and drops
	// every occupancy row the order holds, so the loser deletes the winner's row
	// and the winner dispatches into a lane that reads EMPTY to the next leg's
	// admission check. That is precisely the collision Hold B exists to prevent,
	// and it is load-bearing now rather than belt-and-braces: 488729e0 retired the
	// sibling-in-flight guard, so this row is what keeps two legs out of one lane.
	//
	// The loser owns nothing at this point. The winner holds the row, the child,
	// and the dispatch; the loser's only correct action is to leave quietly.
	//
	// Fail/cancel/skip are unaffected: a terminalized child releases by order in
	// TerminalizeOrder. A hold is only fail-closed if the thing held can be
	// released — the same rule that put the take above this line, applied to the
	// take itself.
	if err = d.lifecycle.MoveToSourcing(next, "dispatcher", "dispatching reshuffle step"); err != nil {
		if IsConcurrentTransition(err) {
			log.Printf("dispatch: child order %d → sourcing lost the CAS: %v — NOT dispatching "+
				"(another caller won this child); its occupancy row is the winner's, leaving it in place",
				next.ID, err)
			return nil
		}
		log.Printf("dispatch: child order %d → sourcing refused: %v — NOT dispatching (the status "+
			"write failed); releasing its lane occupancy", next.ID, err)
		d.ReleaseLaneOccupancy(next.ID)
		return nil
	}
	log.Printf("dispatch: advancing compound order %d, step %d (seq %d)", parentOrderID, next.ID, next.Sequence)

	// Build a synthetic envelope for the child dispatch
	env := d.syntheticEnvelope(next.StationID)
	d.dispatchToFleet(next, env, sourceNode, destNode)
	return nil
}

// handleStaleDigLeg disposes of a dig leg refused on REACHABILITY, and the
// disposition turns on one question: is anyone coming for the bin in the way?
//
//   - SOMEONE IS. The obstruction carries a hard claim, so a robot is en route to
//     it and the lane frees itself. Hold the leg exactly as any other lane wait is
//     held; the lane-clearing redrive re-admits it when the bin leaves. Dissolving
//     here would thrash — re-planning a dig for a bin that is seconds from gone.
//   - NOBODY IS. Nothing in flight will move it, and this compound's plan does not
//     contain it, because the plan was written before it landed. The leg cannot
//     clear. DISSOLVE: abandon the dig, keep the demand, and let the scanner plan
//     the dig the lane now actually needs.
//
// Dissolve is never triggered BY a claim — only by the absence of anyone coming.
//
// UNREADABLE COUNTS AS "someone is coming". A dissolve throws away a plan and
// re-plans; doing that on a read that failed would turn a database hiccup into
// churn across every held leg at once. Holding costs one redrive.
func (d *Dispatcher) handleStaleDigLeg(parentOrderID int64, leg *orders.Order, sourceNode, destNode *nodes.Node) error {
	claimed, err := d.obstructionIsSpokenFor(leg, sourceNode)
	if err != nil {
		log.Printf("dispatch: compound %d child %d is walled and its obstruction could not be read: %v "+
			"(holding the leg; a dissolve on an unreadable lane would re-plan on a hiccup)",
			parentOrderID, leg.ID, err)
		d.setQueueReason(leg, protocol.QueueWaitingForSlot, CauseAdmissionError,
			QueueParams{Destination: destNode.Name})
		return nil
	}
	if claimed {
		d.dbg("dispatch: compound %d child %d is walled by a bin another order has claimed — holding "+
			"(the robot carrying it out is what clears this)", parentOrderID, leg.ID)
		d.setQueueReason(leg, protocol.QueueWaitingForSlot, CauseLaneTargetBuried,
			QueueParams{Destination: destNode.Name})
		return nil
	}
	return d.dissolveCompound(parentOrderID, fmt.Sprintf(
		"child %d is walled by a bin no order is coming for", leg.ID))
}

// obstructionIsSpokenFor reports whether ANY bin standing in front of this leg's
// pickup is hard-claimed — whether a robot is on its way to at least one of them,
// which is enough for the lane to change on its own.
//
// IT IS NOT THE ADMISSION QUESTION and must not be folded into it. Admission
// answers "may this move happen now" and has already said no; this asks "is what
// said no going to go away", which is a disposition, not a verdict. It reads the
// same two primitives admission's reachability arm does (pickupSlotNow,
// findBuriedBlockers) so the set it judges is the set that refused.
//
// Hard claims only, and that is the same line the burial guard draws: claimed_by
// is written at ConfirmForDispatch immediately before the fleet call, so it means
// a robot is in motion. A soft reservation is a plan, and a plan does not move a
// bin — the dig outranks it and the holder recalculates.
func (d *Dispatcher) obstructionIsSpokenFor(leg *orders.Order, sourceNode *nodes.Node) (bool, error) {
	if sourceNode == nil {
		return false, fmt.Errorf("stale-dig disposition: leg %d has no source node", leg.ID)
	}
	lane, err := d.db.LaneForNode(sourceNode.ID)
	if err != nil {
		return false, fmt.Errorf("stale-dig disposition: resolve lane for node %d: %w", sourceNode.ID, err)
	}
	if lane == nil {
		// The refusal named reachability, so the pickup was in a lane a moment ago.
		// It is not now, which is a state nothing here can judge — hold.
		return false, fmt.Errorf("stale-dig disposition: node %s is no longer in a lane", sourceNode.Name)
	}
	target, _, err := d.pickupSlotNow(leg, lane)
	if err != nil {
		return false, fmt.Errorf("stale-dig disposition: pickup slot for leg %d: %w", leg.ID, err)
	}
	blockers, err := findBuriedBlockers(d.db, target.ID)
	if err != nil {
		return false, fmt.Errorf("stale-dig disposition: blockers in front of slot %d: %w", target.ID, err)
	}
	for _, b := range blockers {
		if b.bin.ClaimedBy != nil {
			return true, nil
		}
	}
	return false, nil
}

// dissolveCompound abandons a dig without terminating the demand behind it.
//
// THIS IS THE EXIT THE COMPOUND LIFECYCLE DID NOT HAVE. A compound could finish
// (CompleteCompound / ResumeCompound) or fail (the sibling cascade), and a plan
// that has gone stale is neither: nothing went wrong, the lane simply changed
// under it. Failing was the only reachable exit, and failing kills the demand the
// dig exists to serve.
//
// IT ADDS NO STATUS, deliberately. The child set already carries the fact —
// cancelled legs marked reshuffleDissolveDetail — and the parent's own status is
// unchanged here. Everything a new "abandoned" status would encode is derivable
// from rows that exist.
//
// IT DOES NOT TRANSITION THE PARENT, and that is a hard constraint rather than a
// simplification. The parent's way back into the acquiring set is
// {Reshuffling → Queued}, which fires fireRequeued → EventOrderQueued → the
// SYNCHRONOUS fulfillment scanner. This function is reachable from inside that
// scanner: tryFulfill → PlanBuriedReshuffle → CreateCompoundOrder →
// AdvanceCompoundOrder, all under a non-reentrant scanMu. Transitioning here
// would self-deadlock the process on the first leg of a freshly planned dig. So
// the cancels land, their events fire asynchronously, and the terminal arm — on a
// later goroutine — reads digWasDissolved and returns the parent.
//
// THE LANE LOCK IS RELEASED, and this is required, not tidy. IsLocked is
// owner-blind: planBuriedReshuffle refuses to plan into a locked lane whoever
// holds it, including the very parent about to re-plan. Keeping the lock would
// park the re-plan on lane_locked with nothing left alive to release it — the
// wedge this whole disposition exists to remove, rebuilt one layer up.
//
// Blockers already moved STAY WHERE THEY LANDED. There is no restore; deepest-
// first parking keeps the lane packed without one, and the re-plan simply sees
// fewer blockers than the first plan did.
func (d *Dispatcher) dissolveCompound(parentOrderID int64, why string) error {
	children, err := d.db.ListChildOrders(parentOrderID)
	if err != nil {
		// Without the child list nothing can be cancelled, so a dissolve cannot be
		// completed — and a HALF-dissolved compound is worse than a held one. Leave
		// it; the next redrive tries again.
		log.Printf("dispatch: dissolve compound %d: list children: %v (leaving the dig held)", parentOrderID, err)
		return err
	}
	parent, err := d.db.GetOrder(parentOrderID)
	if err != nil || parent == nil {
		log.Printf("dispatch: dissolve compound %d: load parent: %v (leaving the dig held)", parentOrderID, err)
		return err
	}

	// A DIG BEING TORN DOWN IS NOT A DIG TO RE-PLAN. If the parent has left
	// `reshuffling` — an operator cancelled it, something failed it — then this
	// compound is ending, and dissolving would cancel its legs under a marker that
	// tells the terminal arm to resurrect the parent. The teardown paths cancel
	// children BEFORE the parent, so this window is real rather than theoretical.
	if parent.Status != StatusReshuffling {
		d.dbg("dispatch: not dissolving compound %d — its parent is %s, so the dig is being torn down, "+
			"not re-planned", parentOrderID, parent.Status)
		return nil
	}

	log.Printf("dispatch: DISSOLVING dig %d — %s. Cancelling its remaining legs and re-planning against "+
		"the lane as it now stands; the demand survives.", parentOrderID, why)

	for _, child := range children {
		if protocol.IsTerminal(child.Status) {
			continue
		}
		d.lifecycle.CancelOrder(child, parent.StationID, reshuffleDissolveDetail)
	}

	// Before the parent can re-plan, and unconditionally: see above.
	d.unlockLaneForCompound(parentOrderID)
	return nil
}

// HandleChildOrderComplete is called when a child order completes.
func (d *Dispatcher) HandleChildOrderComplete(childOrder *orders.Order) {
	if childOrder.ParentOrderID == nil {
		return
	}
	d.AdvanceCompoundOrder(*childOrder.ParentOrderID)
}

// HandleChildOrderFailure handles failure of a child in a compound order.
// Cancels ALL remaining non-terminal children (including in-flight ones)
// and fails the parent. Uses lifecycle.CancelOrder to ensure fleet orders
// are cancelled and bins are unclaimed — same approach as cancelCompoundChildren.
func (d *Dispatcher) HandleChildOrderFailure(parentOrderID, childOrderID int64) {
	log.Printf("dispatch: child order %d failed in compound %d, cancelling remaining", childOrderID, parentOrderID)

	// This handler fires once (engine wiring, on the failure event) with no
	// retry, so an early return on a transient DB error must not leave the lane
	// locked forever. Release it on every path — unlockLaneForCompound falls
	// back to an owner-based release when the children can't be read.
	defer d.unlockLaneForCompound(parentOrderID)

	// Cancel remaining non-terminal children (including in-flight)
	children, err := d.db.ListChildOrders(parentOrderID)
	if err != nil {
		log.Printf("dispatch: list children for failed compound %d: %v", parentOrderID, err)
		return
	}

	parent, err := d.db.GetOrder(parentOrderID)
	if err != nil {
		log.Printf("dispatch: load parent for failed compound %d: %v", parentOrderID, err)
		return
	}

	cancelReason := fmt.Sprintf("sibling order %d failed during reshuffle", childOrderID)
	for _, child := range children {
		if child.ID == childOrderID {
			continue
		}
		if protocol.IsTerminal(child.Status) {
			continue
		}
		d.lifecycle.CancelOrder(child, parent.StationID, cancelReason)
	}

	// Fail the parent — Fail handles the atomic transition + emit.
	if err := d.lifecycle.Fail(parent, parent.StationID, "reshuffle_failed",
		fmt.Sprintf("child order %d failed during reshuffle", childOrderID)); err != nil {
		log.Printf("dispatch: fail compound parent %d: %v", parentOrderID, err)
	}
}

// cancelCompoundChildren cancels all non-terminal children of a compound order.
// Unlike HandleChildOrderFailure (which only cancels pending/sourcing children),
// this method also cancels in-flight children (dispatched, in_transit, staged)
// and their fleet orders. Called when an operator cancels a compound parent directly.
func (d *Dispatcher) cancelCompoundChildren(parent *orders.Order, stationID, reason string) {
	children, err := d.db.ListChildOrders(parent.ID)
	if err != nil {
		log.Printf("dispatch: cancel compound children for order %d: %v", parent.ID, err)
		return
	}

	cancelReason := fmt.Sprintf("parent order cancelled: %s", reason)
	for _, child := range children {
		if protocol.IsTerminal(child.Status) {
			continue
		}
		d.lifecycle.CancelOrder(child, stationID, cancelReason)
	}

	d.unlockLaneForCompound(parent.ID)
}

// extendLaneLockForExposeMode runs in AdvanceCompoundOrder's terminal
// block when an expose-mode compound for a complex parent finishes.
// The lane lock is already held by the complex parent (the intake
// handler took the lock keyed by the parent's order ID); we arm a
// listener via d.laneHolds that releases the lock on
// EventBinEnteredTransit for the target bin. The target bin ID was
// persisted to pending_lane_extensions at intake time (the row we
// look up here) — the v7-era derivation by walking the lane and
// excluding blockers was replaced post-v7 because it coupled the
// listener to a contextual invariant (lane-locked-so-no-other-bins)
// that a future lane-lock refactor could silently break.
//
// On any failure to find the persisted row, fall back to the
// unconditional unlock — safer to release than to leave a stuck
// lane. The missing row indicates either (a) the intake-time
// persist failed (logged at the call site), or (b) something else
// already consumed the row.
func (d *Dispatcher) extendLaneLockForExposeMode(_ int64, complexParent *orders.Order, _ []*orders.Order) {
	if d.laneLock == nil {
		d.unlockLaneForCompound(complexParent.ID)
		return
	}
	row, err := d.db.GetPendingLaneExtensionByComplexParent(complexParent.ID)
	if err != nil || row == nil {
		log.Printf("dispatch: extendLaneLockForExposeMode: no pending_lane_extension for complex %d (%v); releasing lock unconditionally",
			complexParent.ID, err)
		d.unlockLaneForCompound(complexParent.ID)
		return
	}
	d.extendLaneLockForComplexParent(complexParent, row.LaneID, row.TargetBinID, row.ExpectedFromNodeID)
}

// unlockLaneForCompound finds and unlocks the lane associated with a compound order's children.
func (d *Dispatcher) unlockLaneForCompound(parentOrderID int64) {
	if d.laneLock == nil {
		return
	}
	children, err := d.db.ListChildOrders(parentOrderID)
	if err == nil {
		for _, child := range children {
			if child.SourceNode != "" {
				sourceNode, err := d.db.GetNodeByDotName(child.SourceNode)
				if err == nil && sourceNode.ParentID != nil {
					d.laneLock.Unlock(*sourceNode.ParentID, parentOrderID)
					return
				}
			}
		}
	}
	// Could not resolve the lane from children (DB error, no children, or no
	// child carries a source node). Release by owning order so a failed or
	// unreadable compound can't strand the lane lock forever.
	d.laneLock.UnlockByOwner(parentOrderID)
}
