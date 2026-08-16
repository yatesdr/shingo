package dispatch

import (
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/orders"
)

// reshuffleFailDetail is shared between the parent's status update and the
// EmitOrderFailed event payload so they can't drift. Used in
// AdvanceCompoundOrder's hasFailed branch when one or more child orders
// failed and the parent must be marked failed.
const reshuffleFailDetail = "reshuffle failed: child order failed"

// CreateCompoundOrder creates a parent order with child orders for a reshuffle plan.
// All children and bin claims are created in a single transaction. The parent
// is transitioned into StatusReshuffling via lifecycle.BeginReshuffle, so the
// caller must pass a parent in a status that has Reshuffling as a legal next
// state (Pending, Sourcing, Queued). Synthetic restore parents that already
// hold StatusReshuffling at creation use CreateCompoundChildrenOnly instead.
func (d *Dispatcher) CreateCompoundOrder(parentOrder *orders.Order, plan *ReshufflePlan) error {
	if err := d.lifecycle.BeginReshuffle(parentOrder,
		fmt.Sprintf("reshuffling: %d steps to unbury bin %d", len(plan.Steps), plan.TargetBin.ID)); err != nil {
		log.Printf("dispatch: begin reshuffle order %d: %v", parentOrder.ID, err)
	}
	return d.CreateCompoundChildrenOnly(parentOrder, plan)
}

// CreateCompoundChildrenOnly creates the compound's child orders and
// advances the first one — same as CreateCompoundOrder MINUS the
// lifecycle.BeginReshuffle call. Used by the synthetic restore-blockers
// parent, which is written directly at StatusReshuffling via a
// MarkReshuffling-style initial write and would log a spurious
// "illegal transition: reshuffling → reshuffling" warning every time
// CreateCompoundOrder's BeginReshuffle fired on an already-Reshuffling
// parent.
//
// The split keeps CreateCompoundOrder's call sites unchanged
// (simple-retrieve and complex-intake parents legitimately need the
// transition) and gives the restore path a method whose name reads
// as "wire up the children, parent is already in the right state."
func (d *Dispatcher) CreateCompoundChildrenOnly(parentOrder *orders.Order, plan *ReshufflePlan) error {
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

	if err := d.db.CreateCompoundChildren(children); err != nil {
		return fmt.Errorf("create compound children: %w", err)
	}

	// Start executing the first child
	return d.AdvanceCompoundOrder(parentOrder.ID)
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
		hasFailedOrCancelled := false
		allTerminal := true
		for _, c := range children {
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

		if parent != nil && IsCoordinated(parent) && planUsedExposeMode(children) {
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

	sourceNode, err := d.db.GetNodeByDotName(next.SourceNode)
	if err != nil {
		if dbErr := d.db.FailOrderAtomic(next.ID, fmt.Sprintf("source node %q not found", next.SourceNode)); dbErr != nil {
			log.Printf("dispatch: atomic fail child order %d: %v", next.ID, dbErr)
		}
		return d.AdvanceCompoundOrder(parentOrderID)
	}

	destNode, err := d.db.GetNodeByDotName(next.DeliveryNode)
	if err != nil {
		if dbErr := d.db.FailOrderAtomic(next.ID, fmt.Sprintf("delivery node %q not found", next.DeliveryNode)); dbErr != nil {
			log.Printf("dispatch: atomic fail child order %d: %v", next.ID, dbErr)
		}
		return d.AdvanceCompoundOrder(parentOrderID)
	}

	// HOLD B, now enforcing. Step 5 made occupancy durable and observable and
	// deliberately let it arbitrate nothing; this is where it starts to.
	//
	// A child may not be sent into a lane something is already inside. With the
	// sibling loop gone this is the ONLY thing keeping two legs of one reshuffle
	// out of one lane, and it is a strictly narrower rule: it asks about the
	// lane, not about the siblings.
	//
	// Staying pending is not a failure and not a retry — the sibling's dropoff
	// completion releases its occupancy and then re-enters this function, so the
	// wait is exactly as long as the lane is busy.
	if occupied, err := d.laneOccupiedForChild(sourceNode, destNode); err != nil {
		// FAIL CLOSED. An unreadable lane is a busy lane: refusing to dispatch
		// costs a retry on the next event, dispatching into a lane whose state
		// could not be read costs a collision.
		log.Printf("dispatch: occupancy read for compound %d child %d: %v (holding the child)", parentOrderID, next.ID, err)
		return nil
	} else if occupied {
		d.dbg("dispatch: compound %d child %d held — a sibling is inside its lane", parentOrderID, next.ID)
		return nil
	}

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
	// RELEASING FIRST IS NOT OPTIONAL. Occupancy was taken above, keyed on this
	// child, and laneOccupiedForChild counts ANY occupant including the child
	// itself. Return while that row stands and the next re-drive finds the lane
	// "busy" — busy with the very leg it is trying to send — and holds it forever.
	// The CAS-loss arm survives regardless (the winner consumes the row) and a
	// terminalized child releases by order in TerminalizeOrder, but a transient DB
	// error on the status write hits neither, and that is the arm that wedges. A
	// hold is only fail-closed if the thing held can be released — the same rule
	// that put the take above this line, applied to the take itself.
	if err = d.lifecycle.MoveToSourcing(next, "dispatcher", "dispatching reshuffle step"); err != nil {
		log.Printf("dispatch: child order %d → sourcing refused: %v — NOT dispatching (another caller "+
			"holds this child, or the status write failed); releasing its lane occupancy", next.ID, err)
		d.ReleaseLaneOccupancy(next.ID)
		return nil
	}
	log.Printf("dispatch: advancing compound order %d, step %d (seq %d)", parentOrderID, next.ID, next.Sequence)

	// Build a synthetic envelope for the child dispatch
	env := d.syntheticEnvelope(next.StationID)
	d.dispatchToFleet(next, env, sourceNode, destNode)
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
