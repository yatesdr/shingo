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
	"shingocore/store/reservations"
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

// ReshuffleDissolveFolderDetail is the dissolve's OTHER ending: a service-dig
// parent, which is cancelled rather than re-queued (§R.93.1).
//
// A separate string on purpose. The two endings are different facts — one demand
// went back to be re-planned, one folder was thrown away — and a reader counting
// dissolves needs to be able to tell them apart. soakstat counts dissolves off
// ReshuffleDissolveDetail; folding this into it would report a cancelled folder
// as a demand re-planning, which is the conflation this whole ruling is about.
const ReshuffleDissolveFolderDetail = "reshuffle dissolved: the dig's plan went stale; the dig is cancelled (a folder does not re-plan)"

// reshuffleDissolveFolderDetail is the in-package spelling of the same constant.
const reshuffleDissolveFolderDetail = ReshuffleDissolveFolderDetail

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
// state (Pending, Sourcing, Queued). It is the ONLY compound-creation path.
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
	if err := d.lifecycle.BeginReshuffle(parentOrder, reshuffleBeginDetail(plan)); err != nil {
		log.Printf("dispatch: begin reshuffle order %d: %v", parentOrder.ID, err)
	}
	return d.AdvanceCompoundOrder(parentOrder.ID)
}

// reshuffleBeginDetail is the history line a compound parent carries into
// `reshuffling`. Two shapes, because there are now two kinds of dig: one that
// exists to reach a BIN, and one that exists to reach a SLOT (window 3's mouth
// clear, whose plan carries no target bin — see ReshufflePlan.TargetBin).
//
// It is a function rather than an inline Sprintf because the inline form
// dereferenced plan.TargetBin unconditionally, which is a nil panic the moment a
// second kind of plan exists. The nil arm is the reason this is here.
func reshuffleBeginDetail(plan *ReshufflePlan) string {
	if plan.TargetBin == nil {
		return fmt.Sprintf("reshuffling: %d steps to clear the path to %s",
			len(plan.Steps), nodeName(plan.TargetSlot))
	}
	return fmt.Sprintf("reshuffling: %d steps to unbury bin %d", len(plan.Steps), plan.TargetBin.ID)
}

// CreateCompoundChildrenOnly WAS HERE AND IS DELETED. It was
// CreateCompoundOrder minus the BeginReshuffle transition, for a parent already
// sitting at StatusReshuffling where that transition would log a spurious
// "illegal transition: reshuffling → reshuffling".
//
// Its one production caller was the synthetic restore-blockers parent, and that
// subsystem went with the restore deletion. What kept it alive afterwards was
// three TEST fixtures that placed a parent at Reshuffling themselves — so an
// exported second creation door survived, in the busiest lifecycle in the
// package, to serve callers that do not ship.
//
// The fixtures drive CreateCompoundOrder from a live status now, which is what
// production does. THERE IS ONE COMPOUND-CREATION PATH: every compound in the
// system, test or not, writes its children and then moves its parent through
// BeginReshuffle. Uniformity applied to creation — a second door is a second
// place for the two halves to get out of order, which is exactly the ordering
// this function's neighbour spends fifteen lines explaining.

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
		// PlanReshuffleUnburyOnly emits no retrieve step at all — the
		// complex parent resumes and runs its own pickup against the
		// now-accessible original slot.
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

		// SNAPSHOT THE LANES BEFORE ANY DISPOSITION RUNS. Two of the three below
		// terminalize the parent, and terminalizing deletes its reservations in the
		// same transaction — so a lane read after the disposition is a lane already
		// gone, and the dwellers behind it are never woken. One read here covers all
		// three arms; see unlockLaneForCompound.
		heldLanes := d.digLanesHeld(parentOrderID)

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
				// ── A FOLDER IS NOT A DEMAND. ONLY DEMANDS RE-PLAN. (§R.93.1) ──
				//
				// The arm above says "Reshuffling → Queued for BOTH kinds", meaning a
				// plain parent and a coordinated one. There is a THIRD kind, and it
				// had no arm: a SERVICE DIG parent is a folder — no bin, no payload,
				// no demand of its own, and no destination at all once its plan is
				// gone. Returning it to the acquiring set hands the fulfillment
				// scanner a folder to source material for.
				//
				// Measured on the rig 2026-08-14, dig 51:
				//
				//   DISSOLVING dig 51 — child 52 is walled by a bin no order is
				//                       coming for ... the demand survives.
				//   engine: order 51 queued for payload ""
				//   fulfillment: dest node "" not found for order 51   ← then forever
				//
				// It sourced a bin ("bin found, soft-holding"), soft-held it away from
				// real demands, parked on dest-node-unresolved — a wait nothing in the
				// world can end — and retried the lookup roughly once a second for the
				// rest of the run.
				//
				// AND THE CORPSE BLOCKED ITS OWN REPLACEMENT, which is the part that
				// actually stops the plant. A phantom stays NON-TERMINAL and keeps its
				// dig_target_node, so arm 3's one-dig-per-episode gate counts it as a
				// live excavation and refuses to raise another dig for that episode.
				// Order 29 — the gate dweller the dig existed to free — sat at the
				// mark for the whole 17-minute window with its only releaser dissolved
				// into a bin-sourcing order.
				//
				// CANCEL IS THE WHOLE FIX, and it works by subtraction: the ordinary
				// cancel path releases the soft-held bin, terminalizing clears arm 3's
				// gate, and window 3 re-proposes for the dweller on its next
				// evaluation. Nothing new runs.
				//
				// "the demand survives" still holds — it was never this row. The demand
				// is the DWELLER, which is untouched by this cancel and is precisely
				// what gets its dig re-proposed.
				if parent.DigTargetNode != "" {
					d.lifecycle.CancelOrder(parent, parent.StationID, reshuffleDissolveFolderDetail)
				} else if err := d.lifecycle.Queue(parent, "dispatcher", reshuffleDissolveDetail); err != nil {
					log.Printf("dispatch: dissolved compound %d could not return its parent to the "+
						"acquiring set: %v (the demand is stranded in reshuffling; the reconciliation "+
						"sweep is the backstop)", parentOrderID, err)
				}
			}
			// Idempotent — the dissolve released it already. Repeated because this
			// arm is also reachable when a leg that was already in flight terminates
			// after the dissolve, and a lane held past a re-plan is a wedge.
			d.unlockLaneForCompound(parentOrderID, heldLanes)
			return nil
		}

		if hasFailedOrCancelled {
			log.Printf("dispatch: compound order %d has failed/cancelled children — marking parent failed", parentOrderID)
			if parent != nil {
				if err := d.lifecycle.Fail(parent, parent.StationID, "reshuffle_failed", reshuffleFailDetail); err != nil {
					log.Printf("dispatch: fail compound order %d: %v", parentOrderID, err)
				}
			}
			d.unlockLaneForCompound(parentOrderID, heldLanes)
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

		// A SERVICE DIG THAT UNCOVERED A BIN USED TO BE HELD HERE, and is not any
		// more. The reasoning was that its legs carry blockers out and nothing else,
		// so when the last one confirms the bin the excavation was raised to expose
		// is standing at an open mouth with only its claim over it — and completing
		// drops the lane, whereupon the next shuffle search finds the very slots
		// this dig emptied.
		//
		// The exposure is real and the hold was the wrong instrument for it: it made
		// a finished order into a permanent non-terminal row, holding a corridor on
		// behalf of a demand it could not ask about, and demands re-resolve while a
		// dig runs. So the corridor CHANGES HANDS instead — to the live demand in
		// the episode this dig was raised for, as that demand's own outbound hold —
		// and the dig terminates on the ordinary path like every other compound.
		//
		// IT HAPPENS HERE AS WELL AS AT THE LAST BLOCKER'S EXIT, and that is not a
		// second spelling: it is the same call, at the second of the two events that
		// can arrive first. The exit fires when a bin enters transit and this fires
		// when the last leg terminalizes, and which one wins depends on the leg. A
		// handoff on the losing path is a no-op — the dig row it moves is already
		// gone — so asking twice costs a read and asking once loses the corridor
		// whenever the other event got there first.
		//
		// BEFORE THE TERMINALIZATION BELOW, necessarily: TerminalizeOrder deletes
		// the parent's reservations in the same transaction as its status write, so
		// a handoff attempted after it finds nothing to hand over.
		if parent != nil {
			for _, laneID := range heldLanes {
				d.handOffDugLane(parent, laneID)
			}
		}
		//
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
		// Lane-lock handling (v7 Step 4.5), now a two-way split:
		//   - COORDINATED parent (a complex order's dig — always expose mode):
		//     TRANSFER the lock from the compound parent to the complex parent
		//     and register a listener that releases on EventBinEnteredTransit
		//     for the target bin, or on parent cancel/fail. Closes the
		//     post-compound / pre-pickup re-burial window.
		//   - non-complex parents (simple-retrieve, restore): unlock
		//     immediately — the bin leaves the lane inside the compound.
		//
		// It used to be a three-way split, and the third arm was decided by
		// planUsedExposeMode, which recovered the plan's mode by string-matching
		// each child's PayloadDesc against "reshuffle retrieve". A human-readable
		// description field was load-bearing for a lock decision. Target-node
		// mode is gone, so expose is the only shape a complex dig takes and the
		// question the sniff answered no longer has two answers.
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

		// ── THE LOCK IS ALWAYS RELEASED HERE NOW ──────────────────────────
		//
		// This used to fork: a COORDINATED parent's lock was TRANSFERRED to it and
		// released later, when it came back through ResumeCompound for the bin the
		// dig had just exposed. That transfer was the expose bridge, and it is gone
		// with the shape that needed it — a complex demand is no longer re-parented
		// into its own dig (task 1), so nothing comes back and there is nothing to
		// hold the lane for.
		//
		// Verified before collapsing rather than assumed: every compound creator's
		// parent is now Coordinated=false — the service dig parent
		// (createServiceDigParent), and the plain retrieve, whose two entry points
		// both sit below the scanner's IsCoordinated early return.
		//
		// LOUD, NOT SILENT, if that ever stops being true. A coordinated parent
		// arriving here would previously have kept its lane; unlocking it instead is
		// the safe direction (a released lane is recoverable, a stuck one is not),
		// but it is a changed answer and it says so rather than passing quietly.
		if parent != nil && IsCoordinated(parent) {
			log.Printf("dispatch: compound parent %d is COORDINATED at terminal — the expose bridge "+
				"that used to transfer its lane lock was deleted with the two-shape ruling, so the "+
				"lane is being released instead. Nothing should create a coordinated compound any "+
				"more; find what did.", parentOrderID)
		}
		d.unlockLaneForCompound(parentOrderID, heldLanes)
		return nil
	}

	// Dispatch the child to fleet
	//
	// A MISSING DELIVERY NODE IS NOW A SHAPE, NOT A FAULT — for a compound child.
	//
	// An unbury leg carries no destination on purpose: planUnbury stopped binding
	// shuffle slots at plan time, so the leg ships unsealed, dwells in the lane it
	// is digging, and Core chooses where the blocker goes at release
	// (awaitsReleaseTimeDestination, dig_dwell.go). Failing it here would fail
	// every dig in the plant.
	//
	// The loud arm does not disappear, it MOVES to a question with an answer: a
	// destination-deferred leg must have somewhere to stand, and the dwell plan
	// fails with ErrNoDwellSlot when its pickup is not in a lane or the lane has no
	// usable slot. That is the same geometry fault this arm was catching, named
	// properly — see the dispatch call at the end of this function.
	if next.SourceNode == "" {
		if err := d.db.FailOrderAtomic(next.ID, "missing source node"); err != nil {
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

	// THE DESTINATION READ IS SKIPPED WHEN THERE IS NO DESTINATION YET. A
	// destination-deferred leg has nothing to resolve here — asking for node ""
	// would come back empty and land in the config-fault arm below, failing every
	// dig. destNode stays nil and every reader beneath is written for that: the
	// admission call treats a nil node as "this move does not touch a lane here",
	// enteredAtDispatch skips it, and destName carries the empty string into the
	// operator sentences, which is honest — the destination is not chosen.
	var destNode *nodes.Node
	if next.DeliveryNode != "" {
		destNode, err = d.db.GetNodeByDotName(next.DeliveryNode)
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
	}
	destName := ""
	if destNode != nil {
		destName = destNode.Name
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
			QueueParams{Destination: destName})
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
		// NAME THE OCCUPANT. This line printed the CAUSE and nothing else, and the
		// cause is the one thing a reader already knows — they are reading a
		// refusal. What they need is WHO, and on 2026-08-14 its absence cost the
		// whole diagnosis: "compound 51 child 52 held at its lane (lane-occupied)"
		// could not distinguish a sibling leg of the same dig dwelling inside the
		// corridor (self-clearing, benign) from a complex order that entered before
		// the dig took the lane and is invisible to CanTake, which reads mouth rows
		// only and a complex order takes none. Those are opposite diagnoses and the
		// log could not tell them apart.
		//
		// One read, on a path that has ALREADY refused, so it is off the hot path by
		// construction. A read failure prints nothing extra rather than failing the
		// refusal — the disposition is unchanged either way, and a diagnostic must
		// never be the thing that breaks a wait.
		occupantNote := ""
		if v.Cause() == CauseLaneOccupied {
			if sourceNode != nil && sourceNode.ParentID != nil {
				if occ, oerr := reservations.OccupantsOf(d.db.DB, *sourceNode.ParentID); oerr == nil {
					occupantNote = fmt.Sprintf(" occupants=%v", occ)
				}
			}
		}
		d.dbg("dispatch: compound %d child %d held at its lane (%s)%s",
			parentOrderID, next.ID, v.Cause(), occupantNote)
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
			QueueParams{Destination: destName})
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
	// AND IT IS THE LANES THIS DISPATCH ENTERS, NOT THE PLAN'S TWO ENDPOINTS.
	//
	// This seam used to pass sourceNode and destNode raw, and that is the one thing
	// every other dispatch path does not do: they pass what the create ENTERS.
	// dispatchGated passes d.planNodes(preWait) — the segment before the mark — and
	// says why (fleet_handover.go: a gated create "passes nothing and takes nothing,
	// and its row is taken by the tail append that actually enters").
	//
	// On an ungated lane the two sets are identical, which is why the difference
	// went years without showing. On a GATED lane they are not: the create ends at
	// the mark, OUTSIDE the corridor, so a row taken here declares a robot inside a
	// lane it is standing next to. That PHANTOM ROW refuses every other order the
	// lane, and on the lane-stress rig 2026-08-12 it refused the one order that
	// could have broken the cycle it had joined — four orders of one episode frozen
	// 997 seconds, unwedged in two minutes by deleting that single row
	// (PLAN §R.54/R.55).
	//
	// FILTERED HERE AND NOT INSIDE TakeLaneOccupancy, which was tried and is wrong:
	// that function is also the general "this order IS in this lane" primitive, and
	// a gated lane holds rows like any other — one robot inside at a time is the
	// whole point of the gate. What is gated is not the LANE's ability to hold a
	// row, it is whether THIS DISPATCH is the moment the robot goes in. Only the
	// seam knows that, so only the seam can answer it.
	//
	// AND BEFORE THE STATUS MOVE, which is what makes the failure arm survivable.
	// The read above fails closed by holding the child; this has to hold it the
	// same way, and a child can only be held while it is still `pending` —
	// GetNextChildOrder selects `status='pending'`, so a child parked at
	// `sourcing` is invisible to every re-drive, and no transition out of
	// sourcing goes back to pending (protocol.validTransitions). Holding it one
	// line further down would strand the leg and leave the parent in
	// `reshuffling` forever: fail-closed on paper, wedged in fact.
	if err := d.TakeLaneOccupancy(next.ID, d.enteredAtDispatch(sourceNode, destNode)...); err != nil {
		// AND THE CAUSE GOES ON THE ROW, like every other arm in this function. This
		// one was missed: the leg holds at `pending` with nothing written, so a leg
		// stalled because Core could not RECORD its presence looked identical to one
		// nobody had reached. Seen on the lane-stress rig 2026-08-10 as a `pending`
		// order carrying no cause for 15 minutes.
		//
		// A failed occupancy write is a database error, so it is the read-failed
		// class and its releaser is the database answering again.
		log.Printf("dispatch: compound %d child %d — could not record lane occupancy: %v (holding the child)",
			parentOrderID, next.ID, err)
		d.setQueueReason(next, protocol.QueueWaitingForSlot, CauseReadFailed,
			QueueParams{Destination: destName})
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
	//
	// THE CLAIM IS TAKEN ONCE, NOT ONCE PER ATTEMPT. A leg being re-driven after a
	// refused fleet create is ALREADY `sourcing` — it never gave the claim up, only
	// its create failed — and `sourcing → sourcing` is not a legal edge. Claiming
	// again would land in the arm below as an IllegalTransition, release the
	// occupancy taken above and return, so the leg would never retry: the park
	// would be a wedge wearing a cause. The claim is skipped, and the CAS in
	// handoverToFleet (`sourcing → dispatched`, before the fleet call) is what
	// arbitrates two callers over an already-claimed leg — which is the job it
	// already does for every other arm.
	if next.Status == StatusPending {
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
	}
	log.Printf("dispatch: advancing compound order %d, step %d (seq %d)", parentOrderID, next.ID, next.Sequence)

	// Build a synthetic envelope for the child dispatch
	env := d.syntheticEnvelope(next.StationID)
	if err := d.dispatchToFleet(next, env, sourceNode, destNode); err != nil {
		// NOWHERE TO STAND IS GEOMETRY, AND IT FAILS LOUD.
		//
		// This is where the old "missing source or delivery node" arm went. A
		// destination-deferred leg builds its plan at the create seam, and that plan
		// needs a lane and a slot in it to wait at; when either is absent the leg can
		// never be dispatched, however long it waits. Parking it as congestion would
		// hold it under a cause nothing can clear — the same trap the read-vs-missing
		// split exists to avoid — so it takes the config-fault disposition every
		// other unresolvable node on this path takes, naming the lane.
		if errors.Is(err, ErrNoDwellSlot) {
			if dbErr := d.db.FailOrderAtomic(next.ID, configFailure("dwell slot", next.SourceNode)); dbErr != nil {
				log.Printf("dispatch: atomic fail child order %d: %v", next.ID, dbErr)
			}
			return d.AdvanceCompoundOrder(parentOrderID)
		}
		d.parkLegOnFleetRefusal(parentOrderID, next, destNode, err)
	}
	return nil
}

// parkLegOnFleetRefusal is a dig leg's disposition when the fleet will not take
// it: WAIT, with a cause, exactly as every other congestion in this system does.
//
// ── WHY THIS IS NOT A FAILURE ─────────────────────────────────────────────
//
// The leg used to be failed here, and a failed leg fails its parent through the
// sibling cascade (HandleChildOrderFailure) — and the parent is the DEMAND. So a
// fleet that was busy for a second terminated a line's request for material,
// which is the wait-not-fail rule broken on the path that can least afford it:
// `51a97a56` drew this exact distinction for the plain path ("a fleet that will
// not take an order right now is congestion") and the compound caller was the
// arm it did not reach.
//
// ── IT UNDOES THE CLAIM, BECAUSE NOTHING ELSE WILL ────────────────────────
//
// handoverToFleet CASes the leg to `dispatched` BEFORE it calls the fleet and
// deliberately does not roll that back, leaving the order "claimed for its
// caller to dispose of". This is that caller. A leg left at `dispatched` with no
// vendor order is worse than a failed one: nothing tracks it (loadActiveOrders
// selects on a non-empty vendor_order_id), no re-drive selects it, and the
// abandon sweep DOES cover `dispatched` — so the quiet ending is a cancelled
// child an hour later, which fails the parent anyway, just slower and with no
// trace of why.
//
// `sourcing` is the rollback, and it is the same edge and the same helper the
// plain path uses (DispatchDirect). It is NOT `pending`: {sourcing → pending} is
// not in the transition table, and the entry-point write that would force it is
// the shape lifecycle.go refuses by name ("an entry-point write that skips the
// state machine is exactly what a future caller reaches for when a transition is
// refused, and the refusal is usually right"). The re-drive was taught to see a
// claimed-but-unsent leg instead — orders.AwaitingFleetSQL — which is where the
// question belonged, since vendor_order_id was always the authority on it.
//
// ── THE RELEASERS ARE THE ONES THAT ALREADY EXIST ─────────────────────────
//
// GetNextChildOrder selects it again on the next advance, and
// RedriveHeldCompoundLegs finds it on any lane-clearing event — both through
// AwaitingFleetSQL. Occupancy is already released by commitToFleet's non-CAS
// failure arm, so the leg holds nothing while it waits.
//
// What this does NOT give it is a floor: with no sibling in flight and a quiet
// lane, nothing re-asks. That is the same edge-triggered gap F-22 names for the
// gate evaluator, it is not this fix's to close, and it is strictly better than
// what it replaces — a wait with a readable cause instead of a dead demand.
func (d *Dispatcher) parkLegOnFleetRefusal(parentOrderID int64, leg *orders.Order, destNode *nodes.Node, cause error) {
	// A LOST CAS OWNS NOTHING. Another caller won this leg and is dispatching it;
	// rolling its status back or writing a cause on it would describe the loser's
	// failure on the winner's live row. Same rule, same reason, as the occupancy
	// release in commitToFleet.
	if IsConcurrentTransition(cause) {
		log.Printf("dispatch: compound %d child %d — fleet handover lost the CAS: %v (another caller "+
			"owns this leg; leaving it alone)", parentOrderID, leg.ID, cause)
		return
	}

	log.Printf("dispatch: compound %d child %d — the fleet refused the create: %v (parking the leg; "+
		"the dig waits and the demand survives)", parentOrderID, leg.ID, cause)

	if leg.Status == StatusDispatched {
		if rbErr := d.lifecycle.MoveToSourcing(leg, "dispatcher", "fleet refused the create"); rbErr != nil {
			log.Printf("dispatch: compound %d child %d could not be rolled back after a fleet refusal: %v "+
				"(it is `dispatched` with no vendor order; the stuck sweep is the backstop)",
				parentOrderID, leg.ID, rbErr)
			return
		}
	}

	dest := ""
	if destNode != nil {
		dest = destNode.Name
	}
	d.setQueueReason(leg, protocol.QueueFleetUnavailable, CauseFleetRefusedCreate,
		QueueParams{Destination: dest})
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
	// nodeName, not destNode.Name: this arm is reached on a SOURCE-side
	// reachability refusal, which a destination-deferred leg can take as readily
	// as a sealed one — and that leg's destination node is legitimately nil until
	// its release. The sentence names what is known.
	dest := nodeName(destNode)
	claimed, err := d.obstructionIsSpokenFor(leg, sourceNode)
	if err != nil {
		log.Printf("dispatch: compound %d child %d is walled and its obstruction could not be read: %v "+
			"(holding the leg; a dissolve on an unreadable lane would re-plan on a hiccup)",
			parentOrderID, leg.ID, err)
		d.setQueueReason(leg, protocol.QueueWaitingForSlot, CauseAdmissionError,
			QueueParams{Destination: dest})
		return nil
	}
	if claimed {
		d.dbg("dispatch: compound %d child %d is walled by a bin another order has claimed — holding "+
			"(the robot carrying it out is what clears this)", parentOrderID, leg.ID)
		d.setQueueReason(leg, protocol.QueueWaitingForSlot, CauseLaneTargetBuried,
			QueueParams{Destination: dest})
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
	heldLanes := d.digLanesHeld(parentOrderID)
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

	// Before the parent can re-plan, and unconditionally: see above. The parent
	// stays `reshuffling` through a dissolve, so nothing has terminalized it and
	// the snapshot could equally be taken here — it is taken above the cancels for
	// uniformity with the other teardowns, where the ordering IS load-bearing.
	d.unlockLaneForCompound(parentOrderID, heldLanes)
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
	// locked forever. Release it on every path — the release is owner-scoped, so
	// it does not depend on the children being readable.
	//
	// THE SNAPSHOT IS TAKEN NOW AND THE RELEASE IS DEFERRED, which is the whole
	// point of splitting them: the deferred call runs after lifecycle.Fail has
	// terminalized the parent and deleted its reservations, so a lane read inside
	// it would find nothing and wake nobody.
	heldLanes := d.digLanesHeld(parentOrderID)
	defer d.unlockLaneForCompound(parentOrderID, heldLanes)

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

// CancelOrderWithCascade is THE cancel door. Both entry points that cancel an
// order on somebody's instruction go through it: the Edge/wire cancel
// (HandleOrderCancel) and the web-UI cancel (engine.TerminateOrder).
//
// ── WHY IT IS ONE DOOR AND NOT TWO CALLS ──────────────────────────────────
//
// engine.TerminateOrder called lifecycle.CancelOrder and stopped. No cascade, no
// lane release, no wake. Cancelling a dig parent from the operations page
// therefore left its legs RUNNING — with live vendor orders, claimed bins, and a
// lane still held by a parent that no longer exists. The wire door had gotten
// the cascade (deliberately unconditional, see HandleOrderCancel); the UI door
// was never given it, and the two doors are indistinguishable from the operator's
// side.
//
// Three things have to happen in one order and it is not the obvious one, which
// is exactly why they belong in a function rather than in a convention:
//
//  1. SNAPSHOT THE LANES FIRST, while the parent still holds them. The rows ARE
//     the lock, and TerminalizeOrder deletes an order's reservations in the same
//     transaction as its status write — so a snapshot taken after step 2 is
//     EMPTY, and an empty snapshot is indistinguishable from "held nothing".
//     That is the whole defect: the wake loop ran over an empty set and every
//     dweller behind the lane waited out the 60-second floor.
//  2. CANCEL THE PARENT, before the children. The reverse order raced: the
//     redrive that a child's cancellation triggers admitted the next leg, hit a
//     reachability refusal, DISSOLVED the dig, and the terminal arm raced the
//     parent's own cancel to a `failed` finish — an operator asked for cancelled
//     and got failed. Cancelling the parent first makes the teardown atomic from
//     every observer's point of view.
//  3. CASCADE, then release the lanes from the snapshot taken in step 1.
//
// Steps 1 and 2 are in tension — the snapshot must precede the very write that
// makes step 2 atomic — and reading the lanes inside the cascade got that
// tension backwards.
func (d *Dispatcher) CancelOrderWithCascade(order *orders.Order, stationID, reason string) {
	heldLanes := d.digLanesHeld(order.ID)
	d.lifecycle.CancelOrder(order, stationID, reason)
	d.cancelCompoundChildren(order, stationID, reason, heldLanes)
}

// cancelCompoundChildren cancels all non-terminal children of a compound order.
// Unlike HandleChildOrderFailure (which only cancels pending/sourcing children),
// this method also cancels in-flight children (dispatched, in_transit, staged)
// and their fleet orders. Called when an operator cancels a compound parent directly.
//
// heldLanes IS TAKEN BY THE CALLER, and that is not a style choice. This used to
// read it here — `heldLanes := d.digLanesHeld(parent.ID)` as its first line —
// which is before the CHILDREN are cancelled but AFTER the parent already was,
// on the only path that calls it. The parent's cancel deletes its reservations
// in the same transaction as its status write, and the reservations are the
// lane lock, so the snapshot read an already-emptied set every time. The wake
// loop then iterated nothing and every dweller behind that lane waited out the
// 60-second floor.
//
// unlockLaneForCompound's own header predicted this failure in general terms
// ("two of the three dispositions above terminalize the parent BEFORE releasing
// its lane — so reading the rows here returns nothing on exactly the paths that
// matter") and the fix it describes is passing the snapshot in. This is the
// caller that had not been converted.
func (d *Dispatcher) cancelCompoundChildren(parent *orders.Order, stationID, reason string, heldLanes []int64) {
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

	d.unlockLaneForCompound(parent.ID, heldLanes)
}

// The expose-bridge lock transfer used to live here
// (extendLaneLockForExposeMode). It kept a dig's lane held past the compound's
// completion so the complex parent could still get in when ResumeCompound brought
// it back for the exposed bin, and it read the target bin out of a
// pending_lane_extensions row written at intake.
//
// All three of those things are gone together: the demand is no longer re-parented
// into its own dig, so nothing is resumed, so nothing needs the lane held, so the
// row has no reader. Named here in the past tense because a reader who finds
// unlockLaneForCompound called unconditionally might otherwise wonder whether the
// post-compound re-burial window was ever considered. It was — the window closed
// when the hand-back did.

// unlockLaneForCompound ends a compound's claim on every lane it holds, and
// re-evaluates each one it freed.
//
// ── A DIG LOCK DROPPING IS A LANE-CLEARING EVENT ──────────────────────────
//
// It is the one such event the gate's trigger set cannot produce for itself.
// Every other trigger fires from a BIN or an ORDER changing, and a dig's last
// leg emits all of them — the pickout, the placement, the child completions, the
// parent's own terminal — strictly BEFORE this runs, because this is what the
// completion arm does last. Each of those passes therefore ran while the lock
// was still held, refused every dweller with lane-dig-active, and left nothing
// to re-ask afterwards. A robot dwelling at the mark then waits for unrelated
// traffic to wake the gate, which on a quiet lane never comes.
//
// Not window 3's bug, and older than it: any gate-staged order refused behind
// ANY dig has had this gap since the evaluator landed. Window 3 makes it
// load-bearing, because a heal dig's whole purpose is to release the dweller
// that asked for it.
//
// ── IT ASKS THE LOCK, NOT THE CHILDREN ────────────────────────────────────
//
// This used to resolve the lane by walking the children and taking the first
// one's source node, with a release-by-owner fallback underneath for when that
// walk failed. The walk was archaeology — re-deriving at teardown a fact the
// reservation row already IS — and it was wrong three ways at once:
//
//   - IT STOPPED AT THE FIRST LANE. `return` inside the loop, so a parent
//     holding dig locks on more than one lane released one and leaked the rest.
//     That is not hypothetical: F-19 measured order 1 holding three at once.
//   - AFTER A RE-PLAN IT NAMED THE WRONG LANE. ListChildOrders returns every
//     generation ordered by sequence, so the first child can belong to a
//     superseded one and point at a lane this dig no longer holds — releasing
//     nothing and evaluating a lane that did not change.
//   - THE FALLBACK WOKE NOBODY. UnlockByOwner released the lock and returned;
//     every dweller behind it lost its only releaser. The path that exists
//     precisely for when things have gone wrong was the path with no wake-up.
//
// ── WHY THE LANES ARE PASSED IN RATHER THAN READ HERE ─────────────────────
//
// Because by the time this runs they can already be gone, and an empty answer
// is indistinguishable from "held nothing". TerminalizeOrder deletes an order's
// reservations in the same transaction as its status write
// (TestMouth_TerminalizeOrderDeletesRow), and two of the three dispositions
// above terminalize the parent BEFORE releasing its lane — so reading the rows
// here returns nothing on exactly the paths that matter, and every dweller stays
// parked. Found by TestWindow3_UnclaimedMouthBinIsDugOutWithNobodyAsking, which
// is the test that exists for this failure.
//
// The disposition could have been reordered instead, and deliberately was not:
// evaluating a lane can create a dig and dispatch its first leg synchronously,
// and running that re-entrant machinery while the parent is still mid-teardown
// trades a liveness bug for a concurrency one. The snapshot is taken first, the
// parent reaches its final state, and the wake-up happens last.
//
// held may be empty — a compound that never took a lane, or a second call after
// the first already released everything. Both are no-ops.
//
// Safe to evaluate from here: the evaluator's per-lane mutex is not held on any
// path that reaches this function, and the pass is idempotent and
// level-triggered, so a lane with nobody dwelling pays one query. It cannot
// recur — the pass it drives can only propose another dig if bins still wall the
// lane, and if any do the newly created dig takes the lock again and the next
// pass stops at IsLocked.
func (d *Dispatcher) unlockLaneForCompound(parentOrderID int64, held []int64) {
	if d.laneLock == nil {
		return
	}
	// Release whatever is still there. The snapshot is what gets evaluated: it
	// was taken while the rows existed and is therefore the complete set, and the
	// release can only ever return a subset of it.
	d.laneLock.UnlockByOwner(parentOrderID)
	for _, laneID := range held {
		d.EvaluateLaneReleases(laneID)
		// THE GROUP, NOT JUST THE LANE. A dweller waiting under
		// `dig-holds-parking` names this lane from inside a different one, and the
		// terminal event that would otherwise reach it through the engine's
		// fan-out has already fired — TerminalizeOrder writes the status before
		// this runs, so that firing saw the lock still held and refused. Without
		// this the wake is the 60-second floor.
		d.EvaluateDwellersSharingGroupWith(laneID)
	}
}

// digLanesHeld snapshots the lanes a compound parent holds, for the teardown that
// is about to release them. CALL IT BEFORE THE DISPOSITION — see
// unlockLaneForCompound for why reading it afterwards answers nothing.
func (d *Dispatcher) digLanesHeld(parentOrderID int64) []int64 {
	if d.laneLock == nil {
		return nil
	}
	return d.laneLock.LanesHeldBy(parentOrderID)
}
