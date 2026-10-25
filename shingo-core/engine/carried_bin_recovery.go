// carried_bin_recovery.go — asking the robot to put it down.
//
// stranded_transit.go's branch B parks a bin on the robot's own carrier node
// when the deck reports loaded, and sweepCarriedBins places it as soon as the
// deck reports empty. That is a WATCH: it waits for the robot to finish
// whatever it is doing and set the bin down on its own.
//
// Sometimes it never will. The order that was carrying the bin is terminal, so
// nothing is going to tell that robot to unload; it goes on to its next job
// with a bin on its deck, and the only exits are an operator with a pallet jack
// or the robot happening to unload somewhere Core can name. This file is the
// third exit: a real order, pinned to that robot, whose whole content is "set
// the bin down at X".
//
// It is NOT a second inference. Nothing here guesses where the bin is — it is
// on that deck, which is the one thing branch B established for certain. What
// it decides is where the bin should GO, and then the ordinary order lifecycle
// observes where it actually went.

package engine

import (
	"errors"
	"fmt"
	"strings"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/dispatch/plan"
	"shingocore/fleet"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// CarriedBinNotRecoverable explains why no recovery order was created. It is
// returned rather than logged-and-swallowed because the caller is a person: the
// operator who pressed Recover on the bins page is owed the reason, and "ok, no"
// is not one.
//
// TWO CALLERS, ONE DOOR (orderCarriedBinDown). The operator, through
// www/handlers_diagnostics.go's recover_carried_bin action; and the watch, whose
// loaded arm asks the cancel-return policy (cancel_return.go) to order a bin
// down after its order was cancelled. The watch caller turns a refusal into a
// carried_bin_return_held row rather than a sentence on a screen.
type CarriedBinNotRecoverable struct {
	BinID  int64
	Reason string
}

func (e *CarriedBinNotRecoverable) Error() string {
	return fmt.Sprintf("bin %d cannot be recovered by order right now: %s", e.BinID, e.Reason)
}

// RecoverCarriedBin is the operator-triggered surface, alongside the other
// recovery actions. The body lives on Engine because it needs the bin service,
// the node service and the robot cache; this is the door.
func (s *RecoveryService) RecoverCarriedBin(binID int64, actor string) (*orders.Order, string, error) {
	return s.engine.RecoverCarriedBin(binID, actor)
}

// RecoverCarriedBin is the bins page's Return button: the HUMAN trigger of the
// same chooser the cancel-return watch uses (chooseDeclared,
// engine/cancel_return.go). One policy, two triggers. The bin goes where a
// claim declares it is used from, or it stays on the deck and the person is
// told why — there is no ladder of its own, because every rung that had one
// chose a place nobody declared, and an engineer who wants a bin at a specific
// node has the bin move.
//
// What differs from the watch, and only this: no cancelled-order precondition
// (a press is a person's ask, whatever the bin's last order was, and the door
// already supersedes a parked order), no once-per-episode memo (a press is a
// fresh ask), the actor is the person, and the audit verb is the button's. The
// return is linked to the bin's last order when that order is over
// (recoversTerminalCarrier), so the watch's restart guard and the station's
// notice see it as that order's return.
//
// The DETAIL is returned and not only recorded: the person pressing the button
// is owed which robot, which node and WHICH DECLARATION chose it.
//
// Every refusal is a *CarriedBinNotRecoverable naming the reason. None of them
// are failures of this function — a robot that is charging, or a plant with no
// free slot, is a legitimate "not now", and the caller retries later or shows
// the sentence to the operator.
func (e *Engine) RecoverCarriedBin(binID int64, actor string) (*orders.Order, string, error) {
	if actor == "" {
		return nil, "", fmt.Errorf("actor is required for a recovery order")
	}
	return e.orderCarriedBinDown(binID, "", onDeckRequest{
		actor:                   actor,
		by:                      dispatch.Person(actor),
		action:                  "carried_bin_recovery_ordered",
		choose:                  e.chooseDeclared,
		recoversTerminalCarrier: true,
	})
}

// onDeckRequest is what differs between the doors that ask a robot to put down
// the bin it is holding. Everything else — the deck check, the stand-down, the
// claim, the robot gate, the uuid, the pin, the dispatch and the audit row — is
// one sequence in orderCarriedBinDown, so the policies cannot drift apart on
// any of it.
//
//	actor     who the audit row names: the operator, for the button;
//	          system:cancel-return for the cancel-return policy.
//	by        who asked (dispatch.Need.By): a person, for the button; Core's own
//	          errand (the zero value) for the cancel-return policy.
//	action    the recovery_actions verb, which says WHICH policy chose the
//	          destination.
//	recovers  the cancelled order this one returns the bin of
//	          (orders.recovers_order_id), when the caller already knows it.
//	recoversTerminalCarrier  link to the carrier the door reads when that
//	          order is over — the button's case, which is handed only a bin.
//	choose    the policy: where this bin goes. It is handed the CARRIER — the
//	          newest order that named the bin and was not itself an on-deck
//	          order — from the read the door makes anyway, so no policy re-reads
//	          it and none can mistake a dead recovery order for the bin's job.
type onDeckRequest struct {
	actor, action           string
	by                      dispatch.Actor
	recovers                *int64
	recoversTerminalCarrier bool
	choose                  func(bin *bins.Bin, robot fleet.RobotStatus, carrier *orders.Order) (*nodes.Node, string, error)
}

// orderCarriedBinDown is the door: a vehicle-pinned unload-only order for a bin
// riding a robot's deck, to wherever req.choose says.
//
// robotID is the deck the caller saw the bin on, or "" for "whichever deck it is
// on now" (the button, which is handed only a bin). A caller that names a robot
// and finds the bin on another deck is refused: what it decided was about a
// deck the bin has left.
//
// ONE CALL AT A TIME, ACROSS EVERY CALLER. The policy chooses a concrete slot
// before the order exists, and the slot is only reserved further down, in
// dispatchRecoveryOrder; between the two, the slot is free to anyone else who
// asks. The callers are the button and both hosts of the watch (the robot poll
// and the reconciliation sweep), each on its own goroutine, and the per-bin
// attempt record serializes nothing across two bins. On the sim rig a swap
// cancelled with both decks loaded put the two hosts on the two bins in the
// same instant: both chose SMN_001, the second lost the reservation, and its
// bin's one attempt was spent (evidence S2, 2026-10-02). Holding this across
// choose-to-reserve makes the second caller choose after the first has
// reserved. Nothing inside the door calls back into it — the lane park's
// synchronous scanner pass and the cancel and fail handlers do not — and
// attempts are rare, so the lock costs nothing in steady state.
func (e *Engine) orderCarriedBinDown(binID int64, robotID string, req onDeckRequest) (*orders.Order, string, error) {
	e.onDeckMu.Lock()
	defer e.onDeckMu.Unlock()
	bin, err := e.BinService().GetBin(binID)
	if err != nil {
		return nil, "", fmt.Errorf("read bin %d: %w", binID, err)
	}
	// A nil bin with a nil error is "no such row", not a failure to read. The
	// combined branch formatted the nil through %w and put "%!w(<nil>)" on an
	// operator-facing door.
	if bin == nil {
		return nil, "", fmt.Errorf("bin %d does not exist", binID)
	}
	// ONLY A BIN ON A DECK. A bin at _TRANSIT is a bin nobody knows the location
	// of, and pinning an unload to the robot that last carried it would be a
	// guess wearing an order's clothes — the robot may have set it down hours
	// ago. That population is the A/B/C inference's, and it stays there.
	if !strings.HasPrefix(bin.NodeName, bins.CarrierNodePrefix) {
		return nil, "", &CarriedBinNotRecoverable{BinID: binID,
			Reason: fmt.Sprintf("it is at %s, not on a robot's deck — a recovery order can only "+
				"ask a robot to put down what it is holding", bin.NodeName)}
	}
	onDeck := strings.TrimPrefix(bin.NodeName, bins.CarrierNodePrefix)
	if onDeck == "" || onDeck == bin.NodeName {
		return nil, "", &CarriedBinNotRecoverable{BinID: binID,
			Reason: fmt.Sprintf("carrier node %q names no robot", bin.NodeName)}
	}
	if robotID != "" && robotID != onDeck {
		return nil, "", &CarriedBinNotRecoverable{BinID: binID,
			Reason: fmt.Sprintf("it is on %s's deck now, not %s's", onDeck, robotID)}
	}
	robotID = onDeck
	// IDEMPOTENCE FIRST, because it is the more useful sentence. A dispatched
	// recovery order has already CLAIMED the bin, so the claim check below
	// would catch a second press too — and answer "order 7 already holds it",
	// which reads like a conflict with somebody else's work. Asked in this
	// order the operator is told the truth: the thing they are trying to do is
	// already happening.
	//
	// The same read names the CARRIER for the policy (onDeckHistory).
	live, carrier, lerr := e.onDeckHistory(binID)
	if lerr != nil {
		return nil, "", fmt.Errorf("read orders for bin %d: %w", binID, lerr)
	}
	if live != nil {
		if !isParkedOnDeckOrder(live) {
			return nil, "", &CarriedBinNotRecoverable{BinID: binID,
				Reason: fmt.Sprintf("recovery order %d is already in flight (%s)", live.ID, live.Status)}
		}
		// A PARKED ONE IS SUPERSEDED, NOT DEFERRED TO. An on-deck order waiting
		// before dispatch (a lane that could not be read,
		// queueRecoveryForUnreadableLane, or a destination not yet secured) never
		// goes terminal on its own. A person
		// pressing the button is the releaser that wait was promised: the press
		// cancels it and this attempt runs its policy afresh. A dispatched one
		// has a robot acting on it and still refuses above.
		//
		// CANCELLED BEFORE THE POLICY RUNS, because the parked order is aimed
		// at its slot and every tier refuses a slot another live order is aimed
		// at — asked first, the press could never choose the slot it was pressed
		// to retry. The cost: a press whose policy then finds nowhere leaves the
		// bin riding with no order, exactly as any refused press does, and says
		// so.
		if cerr := e.cancelParkedOnDeckOrder(live, req.actor); cerr != nil {
			return nil, "", &CarriedBinNotRecoverable{BinID: binID, Reason: cerr.Error()}
		}
		// Re-read: the cancel released the parked order's hold on the bin, and
		// the claim check below must see the bin as it is now.
		if bin, err = e.BinService().GetBin(binID); err != nil || bin == nil {
			return nil, "", fmt.Errorf("re-read bin %d after cancelling parked order %d: %v", binID, live.ID, err)
		}
	}
	if bin.ClaimedBy != nil {
		// Something else owns this bin. Two orders moving one bin is the shape
		// that produces a phantom, and the live order is the one with a robot
		// actually en route.
		return nil, "", &CarriedBinNotRecoverable{BinID: binID,
			Reason: fmt.Sprintf("order %d already holds it", *bin.ClaimedBy)}
	}

	robot, haveRobot := e.GetCachedRobotStatus(robotID)
	if err := robotCanTakeARecoveryOrder(robotID, robot, haveRobot); err != nil {
		return nil, "", &CarriedBinNotRecoverable{BinID: binID, Reason: err.Error()}
	}

	dest, tier, err := req.choose(bin, robot, carrier)
	if err != nil {
		return nil, "", &CarriedBinNotRecoverable{BinID: binID, Reason: err.Error()}
	}

	carrierNode, err := e.db.GetNodeByName(bin.NodeName)
	if err != nil {
		return nil, "", fmt.Errorf("read carrier node %s: %w", bin.NodeName, err)
	}
	if carrierNode == nil {
		return nil, "", fmt.Errorf("carrier node %s does not exist", bin.NodeName)
	}

	// STATION-LESS on purpose. No Edge asked for this order and no Edge should
	// be shown it as one of its own; it is Core reconciling its own bookkeeping
	// with the floor. So no station has a row for it, and Build writes it
	// station-unaware: it goes pending -> dispatched (B3, under the orc's B2
	// Stop 1 and 2 rulings).
	//
	// BUT A BIN LANDING AT A LINE IS THAT LINE'S TO BIND, so the order projects
	// its material notices (order.delivered) to the station whose position it
	// lands on, and to nobody else: recoveryStation. A recovery to storage
	// projects nowhere (B3 close-out, stop 5).
	need := dispatch.Need{
		Type:     dispatch.OrderTypeMove,
		Quantity: 1,
		// The order is for the bin: its issue holds it softly, and the row's
		// source is the carrier node the bin sits on. That node is recorded
		// because it is true and because the lane and slot machinery below take a
		// source node, NOT because a robot goes there: the on-deck plan has no
		// pickup step at all (plan.UnloadOnlyLegs).
		Cart:        bin.ID,
		By:          req.by,
		PayloadCode: bin.PayloadCode,
		// The pin: this robot and no other, in the INSERT (for every other order
		// the column records the robot the fleet assigned). See
		// dispatch.pinnedVehicleFor for why the intent and not robot_id alone is
		// what makes this a pin.
		Robot:        robotID,
		SourceIntent: dispatch.SourceIntentOnDeck,
		// NO_DEMAND, stamped at the literal like its siblings at the other
		// caller-less doors. A recovery is Core reconciling its own books with
		// the floor — it is not a place asking for material, so there is no
		// episode and its absence is not a finding. Left blank it landed in the
		// '' vacuum, which is worse than either honest answer: the no_demand
		// bucket does not count it and the orphan surface does not show it.
		OriginClass: protocol.OriginClassNoDemand,
		// ONE UUID FOR BOTH DOORS, per (bin, robot). It is what refuses the
		// second of two concurrent creates (the watch's two hosts, or the
		// watch and a press) at the unique index, and what makes the button's
		// "already in flight" true of an automatic return too.
		Ref:      recoveryEdgeUUID(binID, robotID),
		Projects: e.recoveryStation(dest),
		Recovers: req.recovers,
	}
	if need.Recovers == nil && req.recoversTerminalCarrier &&
		carrier != nil && protocol.IsTerminal(carrier.Status) {
		need.Recovers = &carrier.ID
	}
	// FREE THE UUID A DEAD ATTEMPT IS HOLDING.
	//
	// recoveryEdgeUUID is deterministic per (bin, robot), which is what makes a
	// racing double-create collide on idx_orders_uuid rather than put two robots
	// on one bin. The cost is that there is exactly ONE such uuid per pair, so a
	// terminal order still holding it makes this bin unrecoverable-by-order
	// forever — and the refusals that terminalize one (slot_unavailable,
	// lane_held, fleet_failed) are the ordinary "not now" path this function's
	// own doc comment promises the caller can retry after.
	//
	// Only TERMINAL rows are cleared. A live order holding the uuid is the
	// in-flight case, and it was already refused above with a better sentence;
	// clearing it would be minting a second order for a bin somebody is already
	// moving. The dead row itself is kept — status, error_detail and history are
	// the record of the failed attempt — and only its index entry is released.
	if freed, cerr := e.db.ReleaseTerminalEdgeUUID(need.Ref); cerr != nil {
		// Not fatal on its own: the create below either succeeds (nothing was
		// holding the uuid) or fails with the constraint error, which is the
		// same outcome as before and still names the problem.
		e.dbg("engine: carried bin recovery: release terminal uuid %q for bin %d: %v",
			need.Ref, binID, cerr)
	} else if freed > 0 {
		e.logFn("engine: carried bin recovery: bin %d — freed %d terminal order(s) holding %q "+
			"so this attempt can be created; the earlier attempt(s) failed and their rows are kept",
			binID, freed, need.Ref)
	}
	// THE ISSUE IS WHOLE AT ITS INSERT, the pin in the row (need.Robot), so it
	// needs no scan lock around it: a pass that reaches the `pending` order
	// before its dispatch takes it to the robot it names.
	order, unitErr := e.issueAndDispatchRecovery(need, bin, carrierNode, dest)
	if unitErr != nil {
		return nil, "", unitErr
	}
	// A REAL ORDER NOW OWNS THE QUESTION. Whatever the jack watch had frozen
	// about where this bin might have been set down is superseded: this order
	// names a destination somebody chose, and the ordinary arrival path will
	// record where the bin actually landed. Leaving the older sample in place
	// would let it compete with that answer if the order later fails.
	e.forgetDrop(binID)

	// THE AUDIT ROW IS PART OF THE UNIT, not decoration. Its ACTION NAME is
	// what distinguishes this from the inference beside it: transit_bin_on_robot
	// means "Core worked out where the bin probably is", an *_ordered action
	// means "Core asked the robot to put it somewhere", and which *_ordered verb
	// it is says which policy chose where. Deduced versus commanded is exactly
	// the question an operator investigating a misplaced bin is asking. The
	// ACTOR is the request's — an operator's name belongs on an order they
	// pressed a button for.
	//
	// A bin that moves with no operator behind it has to be explainable
	// afterwards, and recovery_actions is where the rest of this subsystem
	// already explains itself (parkOnCarrier writes transit_bin_on_robot through
	// the same call). The detail line carries the robot, the destination and
	// WHICH TIER chose it, because "why did it go there" is the question a
	// misplaced bin raises and the tier is the whole answer.
	detail := fmt.Sprintf("order %d: %s unloads at %s (%s)", order.ID, robotID, dest.Name, tier)
	if aerr := e.db.RecordRecoveryAction(req.action, "bin", binID, detail, req.actor); aerr != nil {
		// Logged, not fatal: the order exists and the robot is about to move.
		// Failing here would leave a live order with no record, which is worse
		// than a record-less log line.
		e.logFn("engine: carried bin recovery: audit row for bin %d: %v", binID, aerr)
	}
	e.logFn("engine: carried bin recovery: %s", detail)
	return order, detail, nil
}

// issueAndDispatchRecovery is the recovery door's unit: the issue, whole at its
// INSERT (the cart held softly, the robot pinned in the row: need.Robot), then
// the dispatch in its own call.
func (e *Engine) issueAndDispatchRecovery(need dispatch.Need, bin *bins.Bin, carrierNode, dest *nodes.Node) (*orders.Order, error) {
	binID := bin.ID
	order, _, err := e.dispatcher.IssueOrder(need, plan.Plan{Outcome: plan.OutcomeFound, Bin: bin, Node: carrierNode, Dest: dest})
	if errors.Is(err, reservations.ErrReservationConflict) {
		// Another order holds the bin: nothing was written.
		return nil, &CarriedBinNotRecoverable{BinID: binID, Reason: fmt.Sprintf("bin %d was taken by another order", binID)}
	}
	if err != nil {
		return nil, fmt.Errorf("create recovery order for bin %d: %w", binID, err)
	}

	// ── DISPATCHED HERE, WITH ITS OWN DISPOSITION ───────────────────────
	//
	// The order is issued `pending` with its bin stamped and its robot pinned,
	// so a scanner pass that reaches it first takes it into the held-bin arm,
	// which dispatches it to that robot (and then this call yields). This door
	// dispatches it at once all the same, because a person is waiting on the
	// answer, and the refusals are the door's to dispose of: a fleet refusal of
	// a recovery stays terminal, where the scanner's arm parks.
	if derr := e.dispatchRecoveryOrder(order, bin.ID, carrierNode); derr != nil {
		return nil, derr
	}
	return order, nil
}

// isParkedOnDeckOrder: waiting (`queued`) and never handed to the fleet, so no
// robot is acting on it. A `pending` one is mid-door, about to be asked, and is
// not parked (census delta §2).
func isParkedOnDeckOrder(o *orders.Order) bool {
	return protocol.IsWaiting(o.Status) && o.VendorOrderID == ""
}

// cancelParkedOnDeckOrder cancels a parked on-deck order on the caller's behalf
// and confirms it went terminal. CancelOrder logs a refused transition rather
// than returning it, so the re-read is the only way to know the bin is free.
func (e *Engine) cancelParkedOnDeckOrder(o *orders.Order, actor string) error {
	e.dispatcher.Lifecycle().CancelOrder(o, o.StationID, "superseded by a new recovery request",
		dispatch.CancelCause{Code: protocol.TermOperatorCancelled, Actor: actor})
	after, err := e.db.GetOrder(o.ID)
	if err != nil || after == nil || !protocol.IsTerminal(after.Status) {
		return fmt.Errorf("parked recovery order %d could not be cancelled (%v)", o.ID, err)
	}
	e.logFn("engine: carried bin recovery: parked order %d cancelled by %s to try again", o.ID, actor)
	return nil
}

// dispatchRecoveryOrder hands one recovery order to the fleet through the
// held-bin body the scanner's arm uses (dispatch.HandHeld: the destination
// secured and settled, the lanes asked, then the hand and the fleet), and
// disposes of each refusal the door's own way, cleaning up after itself: a
// person is waiting on the answer. The bin is held from the order's issue
// (dispatch.Need.Cart), in the INSERT's own transaction, so a bin another order
// holds refused the issue before anything was written.
func (e *Engine) dispatchRecoveryOrder(order *orders.Order, binID int64, sourceNode *nodes.Node) error {
	fail := func(code, detail string, err error) error {
		e.failOrderAndEmit(order.ID, code, detail)
		if rerr := e.db.ReleaseReservation(order.ID, binID); rerr != nil {
			e.dbg("engine: carried bin recovery: release reservation for bin %d: %v", binID, rerr)
		}
		if cerr := e.db.ReleaseClaimForBin(binID, order.ID); cerr != nil {
			e.dbg("engine: carried bin recovery: release claim for bin %d: %v", binID, cerr)
		}
		return &CarriedBinNotRecoverable{BinID: binID, Reason: detail}
	}
	h, err := dispatch.HandHeld(e.dispatcher, order, binID, sourceNode)
	// THE DESTINATION, OPERATOR-FACING. Every refusal used to collapse into
	// "slot_unavailable / no slot at X right now", so a failed database read told
	// the person holding the tablet to go and look at a slot that was probably
	// empty, and an unresolved GROUP told them to look at one slot out of a set.
	// The verdict names the cause; this maps it to the code and the sentence the
	// operator sees.
	if dest := h.Dropoff; dest.Refused() {
		code, sentence := recoveryDropoffRefusal(dest.Cause, order.DeliveryNode)
		return fail(code, fmt.Sprintf("%s: %v", sentence, dest.Err), dest.Err)
	}
	// The lane question: a robot driving to the destination is a robot in a
	// lane. The SOURCE is synthetic and has no lane, and the destination's lane
	// is marked (every lane is), so the robot stops at the mark and the gate
	// decides its entry: no lane refuses this order here. What is left is a lane
	// that could not be READ, which parks rather than kills.
	if h.Lanes != nil {
		return e.queueRecoveryForUnreadableLane(order, h.Dropoff.Node, h.Lanes)
	}
	// THE HAND: slot, bin, occupancy and status in one transaction, then the
	// fleet. It takes no lane hold for this order (the source has no lane), so a
	// refusal is a slot or bin refusal, an ended order or a gone named bin: the
	// claim failure. Either way the hand held nothing.
	if dispatch.IsConcurrentTransition(err) {
		// Another dispatcher took the order first and holds it (a scanner pass,
		// with the pin in the row); the hand held nothing, so there is nothing to
		// fail or undo (SYNTH §2.4: the loser yields). The order is returned as
		// it now stands.
		e.logFn("engine: recovery order %d was dispatched by another caller first — yielding", order.ID)
		if fresh, rerr := e.db.GetOrder(order.ID); rerr == nil {
			*order = *fresh
		}
		return nil
	}
	if _, refused := dispatch.AsHandRefusal(err); refused || dispatch.NotAWait(err) {
		return fail("claim_failed", fmt.Sprintf("could not claim bin %d and slot %s: %v", binID, h.Dropoff.Node.Name, err), err)
	}
	if err != nil {
		// A FLEET REFUSAL STAYS TERMINAL (owner, 2026-10-24: "we want engineers to
		// handle it; it's usually for a good reason"), and it is not the lane
		// arm's twin. The person who pressed Recover is standing at the door
		// awaiting the answer, and the robot system saying no is an answer. The
		// lane arm above parks because a corridor clears on its own and something
		// re-drives the order when it does; nothing re-drives a plant whose fleet
		// is down, so a row left live here would be an orphan on the board wearing
		// a cause with no releaser.
		return fail("fleet_failed", fmt.Sprintf("the fleet did not accept the order: %v", err), err)
	}
	return nil
}

// queueRecoveryForUnreadableLane parks a recovery order whose lane could not be
// read, instead of killing it: Core declining to answer is the undetermined
// family, a wait and not a fault. It used to be queueRecoveryForLane, which also
// parked a refusal on a busy corridor; every lane is marked now, so no lane
// refuses this order before dispatch and that arm is gone.
//
// ── WHAT RE-DRIVES IT ─────────────────────────────────────────────────
//
// A recovery order is born with orders.bin_id set, so the scanner routes it to
// dispatchHeldBin (fulfillment/scanner.go, the `order.BinID != nil` branch),
// which reuses the held bin and never consults a finder:
//
//   - ListAcquiring selects {pending, queued}, so a parked recovery order is in
//     the scan set (store/orders/orders.go).
//   - It carries no StepsJSON and no parent, so it takes the plain path.
//   - dispatchHeldBin resolves the SOURCE by name. The carrier node is a real
//     row and `_ROBOT:<id>` carries no dot, so GetNodeByDotName falls through to
//     GetByName and finds it (store/nodes/nodes.go).
//   - It re-reserves the destination (owner-idempotent), re-asks the lane, and
//     on success calls the same DispatchDirect this door would have called. The
//     plan the scanner sends is the plan this door would have sent.
//
// The bin is SOFT (a pending reservation taken at its issue) and the
// destination reservation is owner-idempotent, which is what that arm expects.
func (e *Engine) queueRecoveryForUnreadableLane(order *orders.Order, destNode *nodes.Node, aerr error) error {
	code := protocol.QueueStorageRearranging
	e.logFn("engine: carried bin recovery %d — lane admission could not be read: %v (holding)", order.ID, aerr)
	cause := dispatch.CauseLaneAcquireError
	params := dispatch.QueueParams{Lane: destNode.Name, Payload: order.PayloadCode}
	if lerr := e.dispatcher.ReleaseLanesForOrder(order.ID); lerr != nil {
		e.logFn("engine: release lanes for parked recovery order %d: %v", order.ID, lerr)
	}
	// Through the dispatcher's helper so the sentence and the history row's code
	// come from one computation: a direct store write would set the code without
	// the history row that says why.
	e.dispatcher.SetQueueReason(order, code, cause, params)
	if err := e.dispatcher.Lifecycle().Park(order, "carried-bin-recovery"); err != nil {
		// The park was refused (the order moved under us, or the cause did not
		// land). Say so loudly and fail it rather than hand back a row whose wait
		// nobody can read.
		e.logFn("engine: parked recovery order %d could not be queued: %v", order.ID, err)
		e.failOrderAndEmit(order.ID, "lane_park_failed", "the route was not clear and the order could not be queued")
		if rerr := e.db.ReleaseReservation(order.ID, *order.BinID); rerr != nil {
			e.dbg("engine: carried bin recovery: release reservation for bin %d: %v", *order.BinID, rerr)
		}
		return &CarriedBinNotRecoverable{BinID: *order.BinID,
			Reason: fmt.Sprintf("the route to %s was not clear and the order could not be queued: %v", destNode.Name, err)}
	}
	// No scan trigger here: this park runs under the scan lock the door holds
	// (a synchronous pass would deadlock on it). The periodic pass re-asks it.
	return nil
}

// robotCanTakeARecoveryOrder is the DISPATCHABLE-ROBOTS-ONLY gate.
//
// A pinned order goes to one robot or nowhere. Pinned to a robot the fleet will
// not dispatch, it does not fall through to somebody else — it sits in the
// fleet's queue indefinitely, holding the bin's claim and hiding the bin from
// every finder, which is a worse state than the one it was trying to fix.
//
// So the checks are about DISPATCHABILITY, not health in general: a robot with
// a low battery is fine (it will charge and then work), a robot the plant has
// taken out of the dispatch pool is not.
func robotCanTakeARecoveryOrder(robotID string, robot fleet.RobotStatus, haveRobot bool) error {
	if !haveRobot {
		return fmt.Errorf("no telemetry for %s", robotID)
	}
	if !robot.Connected {
		return fmt.Errorf("%s is offline", robotID)
	}
	if !robot.Available {
		// fleet.RobotStatus.Available is mapped from the vendor's
		// `dispatchable` flag — the plant's own switch for taking a robot out
		// of service. Honouring it is the difference between a recovery order
		// and an order that fights the floor's decision.
		return fmt.Errorf("%s is not dispatchable — the plant has taken it out of the pool", robotID)
	}
	if robot.Emergency {
		return fmt.Errorf("%s is in emergency stop", robotID)
	}
	if robot.IsError {
		return fmt.Errorf("%s is in an error state", robotID)
	}
	return nil
}

// liveRecoveryOrderForBin returns an unfinished recovery order for this bin, if
// one exists. Keyed on the ON-DECK INTENT rather than on the bin's claim: the
// order is created `pending` and claims the bin only at dispatch, so there is a
// window in which a claim check would say "free" about a bin an order is
// already about to move.
func (e *Engine) liveRecoveryOrderForBin(binID int64) (*orders.Order, error) {
	live, _, err := e.onDeckHistory(binID)
	return live, err
}

// onDeckHistory is the one read the door makes about a bin's orders, and both
// answers it needs come out of it (onDeckSplit).
//
// The last ten rows: a bin's on-deck orders are its own recovery attempts, and a
// carrier buried under ten of them belongs to a bin whose problem is not where
// to send it.
func (e *Engine) onDeckHistory(binID int64) (live, carrier *orders.Order, err error) {
	ords, err := e.db.ListOrdersByBin(binID, 10)
	if err != nil {
		return nil, nil, err
	}
	live, carrier = onDeckSplit(ords)
	return live, carrier, nil
}

// onDeckSplit reads a bin's orders, newest first, for the LIVE on-deck order, if
// any, and the CARRIER — the newest order that named the bin and was not an
// on-deck order, which is the job the bin was on when it ended up riding the
// deck.
func onDeckSplit(ords []*orders.Order) (live, carrier *orders.Order) {
	for _, o := range ords {
		if o.SourceIntent == dispatch.SourceIntentOnDeck {
			if live == nil && !protocol.IsTerminal(o.Status) {
				live = o
			}
			continue
		}
		if carrier == nil {
			carrier = o
		}
	}
	return live, carrier
}

// recoveryEdgeUUID is the order's external id. Deterministic per (bin, robot)
// so a duplicate create is refused by the edge_uuid unique index even if the
// in-flight check above raced — the index is the backstop, the check is the
// good error message.
func recoveryEdgeUUID(binID int64, robotID string) string {
	return fmt.Sprintf("recovery-bin-%d-%s", binID, robotID)
}

// recoveryStation is the station a recovery's material notices go to: the one
// whose position the bin lands on, or "" when it lands in storage or on a node
// Core cannot tie to one Edge.
//
// The chooser (chooseDeclared) puts the bin where a claim declares the line's
// bins come from or its empties go: a storage group's slot, which tells nobody,
// or a loader's position, which asks stationClaiming.
//
// Before B3 the recovery broadcast order.delivered to station "" and every Edge
// tried to bind by delivery node; the one modelling the node did. This is that
// Edge, named: one message instead of one per Edge.
func (e *Engine) recoveryStation(dest *nodes.Node) string {
	if dest == nil || dest.NodeTypeCode == protocol.NodeClassSTOR {
		return ""
	}
	return e.stationClaiming(dest)
}

// stationClaiming is the Edge that models a node no order named: the plant's
// only Edge, or, when there are several, the one station whose processes claim
// the node in the keeper's mirror (one read). Several stations, or none, and
// nobody is told: a notice to the wrong Edge binds a bin it does not hold.
//
// node_stations is not asked. It is an admin allow-list, empty on a plant that
// never set it, and says which stations may order from a node, not which one
// models it.
func (e *Engine) stationClaiming(n *nodes.Node) string {
	edges, err := e.db.ListEdges()
	if err != nil {
		e.logFn("engine: recovery to %s: list edges: %v — no station told", n.Name, err)
		return ""
	}
	switch len(edges) {
	case 0:
		return ""
	case 1:
		return edges[0].StationID
	}
	sts, err := e.db.StationsClaimingNode(n.Name)
	if err != nil {
		e.logFn("engine: recovery to %s: read the claims mirror: %v — no station told", n.Name, err)
		return ""
	}
	if len(sts) != 1 {
		e.logFn("engine: recovery to %s: %d stations claim it (%v) — no station told", n.Name, len(sts), sts)
		return ""
	}
	return sts[0]
}

// recoveryDropoffRefusal turns a ReserveStorageDropoff cause into the code and
// the sentence the operator recovering a carried bin sees.
//
// THREE ANSWERS, THREE DIFFERENT THINGS TO DO. "No slot at X right now" is only
// true for one of them, and it was printed for all three:
//
//	contended   a carrier is in the slot. Go and look; it will clear.
//	unresolved  the destination is a GROUP and nothing in it can take the bin.
//	            Looking at any one slot tells them nothing.
//	unreadable  a read failed. Nothing about the slot is known, and telling
//	            somebody it is occupied is inventing a fact.
//
// The default is the undetermined reading rather than the contended one: an
// unrecognised cause has not earned a confident sentence.
func recoveryDropoffRefusal(cause dispatch.QueueCause, deliveryNode string) (code, sentence string) {
	switch cause {
	case dispatch.CauseStoreSlotContended:
		return "slot_unavailable", fmt.Sprintf("no slot at %s right now", deliveryNode)
	case dispatch.CauseNGRPResolve:
		return "dest_unresolved", fmt.Sprintf("%s is a set of positions and none of them can take this bin right now", deliveryNode)
	default:
		return "dest_check_failed", fmt.Sprintf("could not read whether %s can take this bin", deliveryNode)
	}
}
