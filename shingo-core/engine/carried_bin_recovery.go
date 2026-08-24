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
	"fmt"
	"strings"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/fleet"
	"shingocore/service"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// CarriedBinNotRecoverable explains why no recovery order was created. It is
// returned rather than logged-and-swallowed because both callers — an operator
// pressing a button and a sweep — need the sentence: one to read, one to
// record.
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
func (s *RecoveryService) RecoverCarriedBin(binID int64, actor string) (*orders.Order, error) {
	return s.engine.RecoverCarriedBin(binID, actor)
}

// RecoverCarriedBin creates a vehicle-pinned unload-only order for a bin riding
// a robot's deck, and returns it.
//
// Every refusal is a *CarriedBinNotRecoverable naming the reason. None of them
// are failures of this function — a robot that is charging, or a plant with no
// free slot, is a legitimate "not now", and the caller retries later or shows
// the sentence to the operator.
func (e *Engine) RecoverCarriedBin(binID int64, actor string) (*orders.Order, error) {
	if actor == "" {
		return nil, fmt.Errorf("actor is required for a recovery order")
	}
	bin, err := e.BinService().GetBin(binID)
	if err != nil {
		return nil, fmt.Errorf("read bin %d: %w", binID, err)
	}
	// A nil bin with a nil error is "no such row", not a failure to read. The
	// combined branch formatted the nil through %w and put "%!w(<nil>)" on an
	// operator-facing door.
	if bin == nil {
		return nil, fmt.Errorf("bin %d does not exist", binID)
	}
	// ONLY A BIN ON A DECK. A bin at _TRANSIT is a bin nobody knows the location
	// of, and pinning an unload to the robot that last carried it would be a
	// guess wearing an order's clothes — the robot may have set it down hours
	// ago. That population is the A/B/C inference's, and it stays there.
	if !strings.HasPrefix(bin.NodeName, bins.CarrierNodePrefix) {
		return nil, &CarriedBinNotRecoverable{BinID: binID,
			Reason: fmt.Sprintf("it is at %s, not on a robot's deck — a recovery order can only "+
				"ask a robot to put down what it is holding", bin.NodeName)}
	}
	robotID := strings.TrimPrefix(bin.NodeName, bins.CarrierNodePrefix)
	if robotID == "" || robotID == bin.NodeName {
		return nil, &CarriedBinNotRecoverable{BinID: binID,
			Reason: fmt.Sprintf("carrier node %q names no robot", bin.NodeName)}
	}
	// IDEMPOTENCE FIRST, because it is the more useful sentence. A dispatched
	// recovery order has already CLAIMED the bin, so the claim check below
	// would catch a second press too — and answer "order 7 already holds it",
	// which reads like a conflict with somebody else's work. Asked in this
	// order the operator is told the truth: the thing they are trying to do is
	// already happening.
	if live, lerr := e.liveRecoveryOrderForBin(binID); lerr == nil && live != nil {
		return nil, &CarriedBinNotRecoverable{BinID: binID,
			Reason: fmt.Sprintf("recovery order %d is already in flight (%s)", live.ID, live.Status)}
	}
	if bin.ClaimedBy != nil {
		// Something else owns this bin. Two orders moving one bin is the shape
		// that produces a phantom, and the live order is the one with a robot
		// actually en route.
		return nil, &CarriedBinNotRecoverable{BinID: binID,
			Reason: fmt.Sprintf("order %d already holds it", *bin.ClaimedBy)}
	}

	robot, haveRobot := e.GetCachedRobotStatus(robotID)
	if err := robotCanTakeARecoveryOrder(robotID, robot, haveRobot); err != nil {
		return nil, &CarriedBinNotRecoverable{BinID: binID, Reason: err.Error()}
	}

	dest, tier, err := e.resolveCarriedBinDestination(bin, robot)
	if err != nil {
		return nil, &CarriedBinNotRecoverable{BinID: binID, Reason: err.Error()}
	}

	carrier, err := e.db.GetNodeByName(bin.NodeName)
	if err != nil {
		return nil, fmt.Errorf("read carrier node %s: %w", bin.NodeName, err)
	}
	if carrier == nil {
		return nil, fmt.Errorf("carrier node %s does not exist", bin.NodeName)
	}

	order := &orders.Order{
		// STATION-LESS on purpose. No Edge asked for this order and no Edge
		// should be shown it as one of its own; it is Core reconciling its own
		// bookkeeping with the floor.
		OrderType: dispatch.OrderTypeMove,
		Status:    protocol.StatusPending,
		Quantity:  1,
		BinID:     &bin.ID,
		// SourceNode is the carrier node — the bookkeeping row the bin sits on.
		// It is recorded because it is true and because the lane and slot
		// machinery below take a source node, NOT because a robot goes there:
		// the on-deck plan has no pickup step at all (buildUnloadOnlyPlan).
		SourceNode:   carrier.Name,
		PayloadCode:  bin.PayloadCode,
		DeliveryNode: dest.Name,
		// The pin. See dispatch.pinnedVehicleFor for why the intent and not
		// robot_id alone is what makes this a pin.
		SourceIntent: dispatch.SourceIntentOnDeck,
		RobotID:      robotID,
		EdgeUUID:     recoveryEdgeUUID(binID, robotID),
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
	if freed, cerr := e.db.ReleaseTerminalEdgeUUID(order.EdgeUUID); cerr != nil {
		// Not fatal on its own: the create below either succeeds (nothing was
		// holding the uuid) or fails with the constraint error, which is the
		// same outcome as before and still names the problem.
		e.dbg("engine: carried bin recovery: release terminal uuid %q for bin %d: %v",
			order.EdgeUUID, binID, cerr)
	} else if freed > 0 {
		e.logFn("engine: carried bin recovery: bin %d — freed %d terminal order(s) holding %q "+
			"so this attempt can be created; the earlier attempt(s) failed and their rows are kept",
			binID, freed, order.EdgeUUID)
	}
	if err := e.db.CreateOrder(order); err != nil {
		return nil, fmt.Errorf("create recovery order for bin %d: %w", binID, err)
	}

	// ── DISPATCHED HERE, NOT LEFT FOR THE SCANNER ───────────────────────
	//
	// The scanner would never pick this up: it sources a move order by FINDING
	// a bin at the source node, and every finder excludes synthetic nodes —
	// which is the property that makes a carrier node safe in the first place
	// (a bin riding a deck must not be sourceable). An order that waits for a
	// find that cannot happen is an order that sits in sourcing forever,
	// holding the bin.
	//
	// So this door dispatches, in the same shape as the bin-move door: reserve,
	// ask the lanes, confirm, hand over — with the same rollback on refusal,
	// because there is a person waiting on the answer either way.
	if derr := e.dispatchRecoveryOrder(order, bin.ID, carrier, dest); derr != nil {
		return nil, derr
	}

	// THE AUDIT ROW IS PART OF THE UNIT, not decoration. Its ACTION NAME is
	// what distinguishes this from the inference beside it: transit_bin_on_robot
	// means "Core worked out where the bin probably is",
	// carried_bin_recovery_ordered means "Core asked the robot to put it
	// somewhere". Deduced versus commanded is exactly the question an operator
	// investigating a misplaced bin is asking. The ACTOR stays the caller's —
	// an operator's name belongs on an order they pressed a button for.
	// A bin that moves with
	// no operator behind it has to be explainable afterwards, and
	// recovery_actions is where the rest of this subsystem already explains
	// itself (parkOnCarrier writes transit_bin_on_robot through the same call).
	// The detail line carries the robot, the destination and WHICH TIER chose
	// it, because "why did it go there" is the question a misplaced bin raises
	// and the tier is the whole answer.
	detail := fmt.Sprintf("order %d: %s unloads at %s (%s)", order.ID, robotID, dest.Name, tier)
	if aerr := e.db.RecordRecoveryAction("carried_bin_recovery_ordered", "bin", binID, detail, actor); aerr != nil {
		// Logged, not fatal: the order exists and the robot is about to move.
		// Failing here would leave a live order with no record, which is worse
		// than a record-less log line.
		e.logFn("engine: carried bin recovery: audit row for bin %d: %v", binID, aerr)
	}
	e.logFn("engine: carried bin recovery: %s", detail)
	return order, nil
}

// dispatchRecoveryOrder hands one recovery order to the fleet, and cleans up
// after itself if the fleet will not take it.
//
// The sequence mirrors the bin-move door (bin_move.go) because it is the same
// situation: a named bin, no finder involved, and a caller who needs the answer
// now. Deviating would mean a second, less-tested spelling of soft-reserve →
// admit → hard-claim → dispatch.
func (e *Engine) dispatchRecoveryOrder(order *orders.Order, binID int64, sourceNode, destNode *nodes.Node) error {
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
	if err := e.dispatcher.Lifecycle().MarkPending(order, "carried-bin recovery"); err != nil {
		e.dbg("engine: carried bin recovery: mark order %d pending: %v", order.ID, err)
	}
	if err := e.binManifest.ReserveForDispatch(binID, order.ID); err != nil {
		return fail("bin_taken", fmt.Sprintf("bin %d was taken by another order: %v", binID, err), err)
	}
	// RESERVE THE SLOT BEFORE CONFIRMING IT. ConfirmForDispatch hard-claims a
	// storage dropoff through the reservation-guarded CAS, which requires a
	// PENDING reservation to exist — the scanner's plain path gets one from
	// ReserveStorageDropoff on the way in, and a door that dispatches directly
	// has to make the same call or the claim is refused for having no
	// reservation rather than for the slot being taken.
	//
	// It also SETTLES the destination (group → child, off a dug lane), so the
	// node it returns is the one to dispatch against and the earlier read is
	// stale from here on.
	settled, rerr := e.dispatcher.ReserveStorageDropoff(order)
	if rerr != nil {
		return fail("slot_unavailable", fmt.Sprintf("no slot at %s right now: %v", order.DeliveryNode, rerr), rerr)
	}
	if settled != nil {
		destNode = settled
	}
	// The lane question, asked for the same reason the bin-move door asks it: a
	// robot driving to the destination is a robot in a lane. EntryHeldBin
	// because the bin is named and no finder answered the reachability
	// question. The SOURCE is synthetic and has no lane, so in practice only
	// the destination's lane can hold this order — which is correct.
	if admitted, cause, laneName, aerr := e.dispatcher.AcquireLanesForOrder(
		order, sourceNode, destNode, dispatch.EntryHeldBin); aerr != nil || !admitted {
		return fail("lane_held", fmt.Sprintf("the route to %s is not clear right now (%s%s)",
			destNode.Name, cause, laneSuffix(laneName)), aerr)
	}
	if err := e.dispatcher.ConfirmForDispatch(order, binID, sourceNode, destNode); err != nil {
		return fail("claim_failed", fmt.Sprintf("could not claim bin %d and slot %s: %v", binID, destNode.Name, err), err)
	}
	if _, err := e.dispatcher.DispatchDirect(order, sourceNode, destNode); err != nil {
		// The door fails the row rather than leaving it queued, for the same
		// reason the bin-move door does: there is nobody here to wait it out,
		// and a live order nothing is driving is an orphan on the board.
		return fail("fleet_failed", fmt.Sprintf("the fleet did not accept the order: %v", err), err)
	}
	return nil
}

// laneSuffix renders the lane name when there is one, so the refusal reads as a
// sentence either way.
func laneSuffix(laneName string) string {
	if laneName == "" {
		return ""
	}
	return " in " + laneName
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

// resolveCarriedBinDestination is the three-tier fallback, in the order a
// person would try them.
//
// The tiers descend from most-informed to least, and each one is a strictly
// weaker claim than the last:
//
//	1  WHERE IT WAS GOING. The order that was carrying this bin named a
//	   destination, and that destination is still the best answer available:
//	   somebody wanted the bin there and the robot was on its way. Requires the
//	   node to still exist, be enabled, be concrete, and be empty — a stale
//	   destination is worth nothing.
//	2  WHERE A BIN OF ITS KIND BELONGS. No original destination, or it is taken.
//	   A free storage slot that accepts this payload puts the bin somewhere the
//	   system will find it again by ordinary means.
//	3  WHERE THE ROBOT ALREADY IS. No slot anywhere. If the robot is parked at a
//	   node we can name and that node is empty, unloading there is the shortest
//	   possible job and it converts a bin nobody can reach into a bin at a known
//	   place. Weakest, because the node was chosen by where the robot stopped
//	   rather than by anything about the bin.
//
// AND THERE IS NO FOURTH. When none of the three answer, no order is created
// and the bin keeps riding — which is exactly what happens today, and is
// better than unloading a bin into a slot the floor does not expect it in. The
// refusal names the tiers that were tried.
func (e *Engine) resolveCarriedBinDestination(bin *bins.Bin, robot fleet.RobotStatus) (*nodes.Node, string, error) {
	if node := e.tierOriginalDestination(bin); node != nil {
		return node, "tier 1: the destination its order was carrying it to", nil
	}
	if node, err := e.db.FindEmptyStorageNodeForPayload(bin.PayloadCode); err == nil && node != nil {
		return node, "tier 2: a free storage slot for " + bin.PayloadCode, nil
	}
	if node := e.tierRobotsCurrentStation(robot); node != nil {
		return node, "tier 3: the node the robot is parked at", nil
	}
	return nil, "", fmt.Errorf(
		"nowhere to put it: its order's destination is gone or occupied, no free storage slot accepts %q, "+
			"and the robot is not parked at an empty node we can name",
		bin.PayloadCode)
}

// tierOriginalDestination is tier 1. nil for every reason it does not apply.
func (e *Engine) tierOriginalDestination(bin *bins.Bin) *nodes.Node {
	ord, _, ok := e.lastClaimingOrder(bin.ID)
	if !ok || ord == nil || ord.DeliveryNode == "" {
		return nil
	}
	node, err := e.db.GetNodeByDotName(ord.DeliveryNode)
	if err != nil || node == nil {
		return nil
	}
	return e.usableDropPoint(node)
}

// tierRobotsCurrentStation is tier 3. It requires the robot to be PARKED, for
// the same reason branch A does: ResolveRobotStation falls back to LastStation,
// and a robot under way resolves to a node it merely passed.
func (e *Engine) tierRobotsCurrentStation(robot fleet.RobotStatus) *nodes.Node {
	if robot.Busy {
		return nil
	}
	node, ok := service.ResolveRobotStation(e.NodeService(), robot)
	if !ok {
		return nil
	}
	return e.usableDropPoint(node)
}

// usableDropPoint returns the node if a bin can actually be set down on it, and
// nil otherwise. One place for the three conditions every tier needs, so a tier
// added later cannot forget one.
func (e *Engine) usableDropPoint(node *nodes.Node) *nodes.Node {
	if node == nil || !node.Enabled || node.IsSynthetic || node.ClaimedBy != nil {
		return nil
	}
	cnt, err := e.db.CountBinsByNode(node.ID)
	if err != nil || cnt > 0 {
		return nil
	}
	return node
}

// liveRecoveryOrderForBin returns an unfinished recovery order for this bin, if
// one exists. Keyed on the ON-DECK INTENT rather than on the bin's claim: the
// order is created `pending` and claims the bin only at dispatch, so there is a
// window in which a claim check would say "free" about a bin an order is
// already about to move.
func (e *Engine) liveRecoveryOrderForBin(binID int64) (*orders.Order, error) {
	ords, err := e.db.ListOrdersByBin(binID, 10)
	if err != nil {
		return nil, err
	}
	for _, o := range ords {
		if o.SourceIntent == dispatch.SourceIntentOnDeck && !protocol.IsTerminal(o.Status) {
			return o, nil
		}
	}
	return nil, nil
}

// recoveryEdgeUUID is the order's external id. Deterministic per (bin, robot)
// so a duplicate create is refused by the edge_uuid unique index even if the
// in-flight check above raced — the index is the backstop, the check is the
// good error message.
func recoveryEdgeUUID(binID int64, robotID string) string {
	return fmt.Sprintf("recovery-bin-%d-%s", binID, robotID)
}
