// cancel_return.go — a bin left riding a robot after its order was cancelled
// goes back to where the plant will source it from next time.
//
// An order cancelled after its pickup leaves the bin on the deck; branch B
// (stranded_transit.go) moves the bin onto the robot's carrier node and the
// carried-bin watch keeps reading the jack. Until this file, a person ended that
// state: the bins page's Recover button, or a pallet jack. This is the
// automatic exit, and it is deliberately narrow:
//
//   - ONLY A CANCELLED ORDER'S BIN (returnEligible). A failed order keeps
//     today's behaviour — the bin stays on the deck for an engineer — because a
//     failure says nothing trustworthy about what the robot is holding.
//   - ONLY INTO A PLACE SOMEBODY DECLARED, and only storage: the inbound
//     sources the claims name for the bin's payload, or for an empty bin the
//     places the claims declare empties of its type go to and come from
//     (returnSources, store/return_sources.go). Never where the bin was lifted,
//     never where the cancelled order was taking it, never a press, a line or
//     staging, never a place the policy picked.
//   - ONCE PER EPISODE. One attempt per (bin, carrier order); a hold is final
//     for that episode, and the Recover button is the retry.
//   - THROUGH THE BUTTON'S DOOR (orderCarriedBinDown), so the deck check, the
//     stand-down, the robot gate, the pin, the uuid and the dispatch are the
//     button's own and cannot drift. Only the policy and the audit verb differ.
//
// THE TRIGGER IS THE WATCH, NOT THE CANCEL. Nothing is added to the cancel
// handler: a cancel can be emitted under the fulfillment scanner's lock (a
// reshuffle dissolve), and a lane-held return runs a synchronous scanner pass —
// so an attempt from the handler would self-deadlock. The watch
// (placeCarriedBinIfSettled) runs on the robot poll and the reconciliation
// loop, neither of which holds that lock.

package engine

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/eventbus"
	"shingocore/dispatch"
	"shingocore/dispatch/binresolver"
	"shingocore/fleet"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

const (
	// cancelReturnActor is who the audit rows name. NOT service.InferredActor:
	// that one means "Core deduced where the bin is" and the carrier guard
	// trusts it (bin_service.go). A return is Core COMMANDING a move.
	cancelReturnActor = "system:cancel-return"
	// The two recovery_actions verbs. carried_bin_recovery_ordered stays the
	// button's; these say the cancel-return policy chose, or declined.
	actionReturnOrdered = "carried_bin_return_ordered"
	actionReturnHeld    = "carried_bin_return_held"
)

// maybeReturnCarriedBin is the trigger: called from the watch's loaded arm, only
// for a jack that reads certain-and-carrying. Every early return before the
// test-and-set costs nothing to retry.
//
// The guards run cheapest first. Connected, Busy and the robot gate read the
// cache, and every field they read is in the poll's status hash, so a robot
// that is busy or faulted at the cancel is asked again, free, on the status
// change that clears it. The order read comes after them.
func (e *Engine) maybeReturnCarriedBin(bin *bins.Bin, robotID string, robot fleet.RobotStatus, soleOnNode bool) {
	// BUSY STANDS DOWN, and here it is the right test. A terminate that did not
	// take leaves the vendor order running and Busy true (CancelOrder logs a
	// refused vendor cancel and proceeds); a return pinned behind it would be
	// queued on a robot that is still doing the cancelled job.
	if !robot.Connected || robot.Busy {
		return
	}
	if robotCanTakeARecoveryOrder(robotID, robot, true) != nil {
		return
	}
	ords, err := e.db.ListOrdersByBin(bin.ID, 10)
	if err != nil {
		e.dbg("engine: cancel return: bin %d — orders unreadable (%v); asking again next status change", bin.ID, err)
		return
	}
	live, carrier := onDeckSplit(ords)
	if live != nil || carrier == nil {
		return
	}
	if e.returnTried(bin.ID, carrier.ID) {
		return
	}
	var hist *orders.History
	if carrier.Status == protocol.StatusCancelled {
		if hist, err = e.db.LatestOrderHistoryForStatus(carrier.ID, carrier.Status); err != nil {
			e.dbg("engine: cancel return: bin %d — terminal row of order %d unreadable (%v)", bin.ID, carrier.ID, err)
			return
		}
	}
	if ok, why := returnEligible(carrier, hist, e.strandedSweepWindow(), clock.Now().UTC()); !ok {
		e.markReturnTried(bin.ID, carrier.ID)
		e.dbg("engine: cancel return: bin %d not returned — %s", bin.ID, why)
		return
	}
	// THE RESTART GUARD COUNTS TERMINAL ROWS. The map dies with the process; the
	// order table does not. A return already attempted for this carrier — live,
	// finished, or failed — is the episode's one attempt.
	for _, o := range ords {
		if o.SourceIntent == dispatch.SourceIntentOnDeck && o.RecoversOrderID != nil && *o.RecoversOrderID == carrier.ID {
			e.markReturnTried(bin.ID, carrier.ID)
			return
		}
	}
	if !e.markReturnTried(bin.ID, carrier.ID) {
		return // the other host got here first
	}
	// ONLY THE SOLE BIN ON ITS CARRIER NODE. The watch's decline arms leave a
	// bin on the node after a drop nobody watched, and the robot's next lift
	// shares the node; one loaded deck is not a fact about two bins, and
	// ordering both down is two phantoms. Held, once, with the reason — the
	// person who sorts out which bin is real presses Recover.
	if !soleOnNode {
		reason := "another bin is recorded on " + bin.NodeName + "; one loaded deck says nothing about which of them is on it"
		e.recordReturnHeld(bin, robotID, carrier, reason)
		e.notifyBinReturn(carrier, bin, protocol.BinReturnHeld, "", reason)
		return
	}
	e.tryReturnCarriedBin(bin, robotID, robot, carrier)
}

// returnEligible: is this carrier's bin one the cancel-return policy returns.
//
// CANCELLED IS CANCELLED. Every producer of `cancelled` returns its bin:
//
//	person / station terminate     operator_cancelled             → returned
//	fleet stop                     uncoded, "fleet order stopped"  → returned
//	reconciliation abandon         uncoded, "abandoned: ..."       → returned
//	reshuffle dissolve             uncoded, "reshuffle dissolved"  → returned
//	reshuffle leg failed           uncoded, "reshuffle dissolved"  → returned
//	swap-peer cascade              peer_terminal                   → returned
//
// There is no reason classifier, and that is the ruling, not an omission. An
// abandon is Core cancelling an order it has stopped trusting the progress of;
// whether the bin it leaves on a deck is safe to order down is a question about
// the ROBOT, and the trigger's physical gates answer it the same way for every
// producer — a jack that reads certain and loaded, a robot that is not Busy, a
// robot the fleet will dispatch. The telemetry buckets "abandoned: ..." as a
// failure because that is how a dashboard wants to count it; it says nothing
// about what is on the deck. A FAILED order is a different status and is not
// returned.
//
// THE EPISODE MUST BE FRESH: the terminal row inside the same window the
// _TRANSIT sweep uses (terminalWithin). A carrier that ended hours ago is not
// the job this deck is holding the bin for. The row is read for its timestamp.
func returnEligible(carrier *orders.Order, hist *orders.History, window time.Duration, now time.Time) (bool, string) {
	if carrier == nil {
		return false, "no order on record carried it"
	}
	if carrier.Status != protocol.StatusCancelled {
		return false, fmt.Sprintf("order %d is %s; only a cancelled order's bin is returned", carrier.ID, carrier.Status)
	}
	if hist == nil {
		return false, fmt.Sprintf("order %d has no cancelled row in its history", carrier.ID)
	}
	if age := now.Sub(hist.CreatedAt); age > window {
		return false, fmt.Sprintf("order %d ended %s ago, outside the %s window", carrier.ID, age.Round(time.Second), window)
	}
	return true, ""
}

// tryReturnCarriedBin is the door with the cancel-return policy, and the hold
// row when the door says no.
func (e *Engine) tryReturnCarriedBin(bin *bins.Bin, robotID string, robot fleet.RobotStatus, carrier *orders.Order) {
	recovers := carrier.ID
	order, detail, err := e.orderCarriedBinDown(bin.ID, robotID, onDeckRequest{
		actor:    cancelReturnActor,
		action:   actionReturnOrdered,
		recovers: &recovers,
		choose:   e.chooseReturn(carrier),
	})
	if err != nil {
		// EVERY REFUSAL IS A HOLD ROW: the policy found nowhere, the door's own
		// gates said no, or the fleet refused the dispatch (the order is already
		// failed with its code). The bin stays on the deck, as it would have
		// without this file, and the row says why.
		reason := err.Error()
		if nr, ok := err.(*CarriedBinNotRecoverable); ok {
			reason = nr.Reason
		}
		e.recordReturnHeld(bin, robotID, carrier, reason)
		e.notifyBinReturn(carrier, bin, protocol.BinReturnHeld, "", reason)
		return
	}
	e.logFn("engine: cancel return: bin %d returns for cancelled order %d as order %d — %s",
		bin.ID, carrier.ID, order.ID, detail)
	e.notifyBinReturn(carrier, bin, protocol.BinReturnReturning, order.DeliveryNode, "")
}

// notifyBinReturn tells the cancelled order's station what became of its bin
// (P2). Notice-only: the Edge records it against the cancelled order and shows
// it; no order status and no lineside count moves. A station-less carrier has
// nobody to tell. Best-effort: a send that fails is logged, and the bin's
// fate is still on the order page and in recovery_actions.
func (e *Engine) notifyBinReturn(cancelled *orders.Order, bin *bins.Bin, state, destination, reason string) {
	cancelled = e.orderTheEdgeHolds(cancelled)
	if cancelled == nil || cancelled.StationID == "" || cancelled.EdgeUUID == "" {
		return
	}
	if err := e.SendDataToEdge(protocol.SubjectBinReturn, cancelled.StationID, &protocol.BinReturn{
		OrderUUID:   cancelled.EdgeUUID,
		BinLabel:    bin.Label,
		PayloadCode: bin.PayloadCode,
		State:       state,
		Destination: destination,
		Reason:      reason,
		At:          clock.Now().UTC(),
	}); err != nil {
		e.logFn("engine: cancel return: notify %s of bin %d (%s): %v", cancelled.StationID, bin.ID, state, err)
	}
}

// orderTheEdgeHolds is the order a notice about o's bin is keyed on: o itself,
// or, for a compound child (a dig leg), its nearest ancestor with no parent.
// Core mints a child's uuid and never sends it to the Edge, so a notice keyed
// on it would be stored at the station and shown on no order; the root is the
// order the station placed. One read per hop, only when a notice is sent. A
// read that fails keeps what it has.
func (e *Engine) orderTheEdgeHolds(o *orders.Order) *orders.Order {
	for hops := 0; o != nil && o.ParentOrderID != nil && hops < 8; hops++ {
		parent, err := e.db.GetOrder(*o.ParentOrderID)
		if err != nil || parent == nil {
			return o
		}
		o = parent
	}
	return o
}

// returnOrderEnded is the second half of the notice: a return order reaching a
// terminal status tells the cancelled order's station how it ended. Delivered
// is "returned"; failed or cancelled is "held", because the bin did not reach
// the slot and is wherever the robot left it — on its deck, for the watch.
//
// EVERY OTHER ORDER COSTS NOTHING. The terminal events carry the emitting
// order's recovers_order_id (set in dispatch.transition's fire actions, where
// the order is in hand), so an order that returns no bin — every order but a
// cancel-return — is turned away here on the payload, with no read.
func (e *Engine) returnOrderEnded(orderID int64, recovers *int64, state, reason string) {
	if recovers == nil {
		return
	}
	ret, err := e.db.GetOrder(orderID)
	if err != nil || ret == nil || ret.BinID == nil {
		return
	}
	// EventOrderCompleted fires on (*, Delivered) AND on (Delivered,
	// Confirmed). "Returned" is the bin set down, which is the first.
	if state == protocol.BinReturnReturned && ret.Status != protocol.StatusDelivered {
		return
	}
	cancelled, err := e.db.GetOrder(*recovers)
	if err != nil || cancelled == nil {
		return
	}
	bin, err := e.BinService().GetBin(*ret.BinID)
	if err != nil || bin == nil {
		return
	}
	if state == protocol.BinReturnHeld {
		reason = fmt.Sprintf("return order %d ended %s: %s", ret.ID, ret.Status, reason)
	}
	e.notifyBinReturn(cancelled, bin, state, ret.DeliveryNode, reason)
}

// wireCancelReturnNotices subscribes returnOrderEnded to the three terminal
// events a return order can reach.
func (e *Engine) wireCancelReturnNotices() {
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderCompletedEvent]) {
		e.returnOrderEnded(evt.Payload.OrderID, evt.Payload.RecoversOrderID, protocol.BinReturnReturned, "")
	}, EventOrderCompleted)
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderFailedEvent]) {
		e.returnOrderEnded(evt.Payload.OrderID, evt.Payload.RecoversOrderID, protocol.BinReturnHeld, evt.Payload.Detail)
	}, EventOrderFailed)
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderCancelledEvent]) {
		e.returnOrderEnded(evt.Payload.OrderID, evt.Payload.RecoversOrderID, protocol.BinReturnHeld, evt.Payload.Reason)
	}, EventOrderCancelled)
}

// recordReturnHeld writes the episode's hold: the bin stays on the deck, as it
// would have without this file, and the row says why.
func (e *Engine) recordReturnHeld(bin *bins.Bin, robotID string, carrier *orders.Order, reason string) {
	held := fmt.Sprintf("order %d cancelled with the bin on %s: held — %s", carrier.ID, robotID, reason)
	if aerr := e.db.RecordRecoveryAction(actionReturnHeld, "bin", bin.ID, held, cancelReturnActor); aerr != nil {
		e.logFn("engine: cancel return: audit hold for bin %d: %v", bin.ID, aerr)
	}
	e.logFn("engine: cancel return: bin %d %s", bin.ID, held)
}

// returnSource is one place the plant sources this bin from, by the name the
// claim gave it, and the node that name resolves to (nil if it resolves to
// nothing — kept so the hold sentence can say so).
type returnSource struct {
	name string
	node *nodes.Node
}

// returnSources: the nodes a line will source this bin from, best first.
//
//	payload bin  the inbound sources the claims name for the payload — the
//	             cancelled order's own process first, then styles running now,
//	             then by name (store.ReturnSourcesForPayload).
//	empty bin    the places the claims DECLARE empties of its bin type go to
//	             and come from — a consume claim's outbound destination, a
//	             produce claim's inbound source, and the loader equivalents —
//	             the cancelled order's own process first, then styles running
//	             now, then by name (store.EmptiesPlacesForBinType). "Another
//	             area where they are used" is the walk past the own process.
//
// NOT node_payloads. That table is a store RESTRICTION that defaults to allow,
// and no finder reads it to source: a bin put where it allows but no claim
// names is a bin no need will look for.
func (e *Engine) returnSources(bin *bins.Bin, carrier *orders.Order) ([]returnSource, error) {
	own := carrierProcessNodes(carrier)
	if bin.PayloadCode != "" {
		names, err := e.db.ReturnSourcesForPayload(bin.PayloadCode, own)
		if err != nil {
			return nil, err
		}
		out := make([]returnSource, 0, len(names))
		for _, name := range names {
			node, nerr := e.db.GetNodeByDotName(name)
			if nerr != nil {
				node = nil
			}
			out = append(out, returnSource{name: name, node: node})
		}
		return out, nil
	}
	names, err := e.db.EmptiesPlacesForBinType(bin.BinTypeID, own)
	if err != nil {
		return nil, err
	}
	out := make([]returnSource, 0, len(names))
	for _, name := range names {
		node, nerr := e.db.GetNodeByDotName(name)
		if nerr != nil {
			node = nil
		}
		out = append(out, returnSource{name: name, node: node})
	}
	return out, nil
}

// fencedFor reports whether a declared empties place is a strict maintained
// group that fences the cancelled order's process — the same admission the
// finder's tier 3 applies to a need naming that group (nodes.GroupFencesAsker).
// DECLARED AND NOT FENCED: a place some claim declares, but that this process
// may not draw from, is a place the next empty need from this process would be
// turned away at. The asker is the order's process node, else the node it
// delivered to or lifted from (carrierProcessNodes' first).
func (e *Engine) fencedFor(src *nodes.Node, carrier *orders.Order) (bool, string) {
	if src == nil || src.NodeTypeCode != protocol.NodeClassNGRP {
		return false, ""
	}
	asker := ""
	if own := carrierProcessNodes(carrier); len(own) > 0 {
		asker = own[0]
	}
	fenced, err := e.db.GroupFencesAsker(src.ID, asker)
	if err != nil {
		return true, "its fence could not be read"
	}
	if fenced {
		return true, "it is a strict group that does not support " + asker
	}
	return false, ""
}

// storeInto: can this bin be stored at that source now, and where exactly. The
// answer is a CONCRETE node chosen before any order exists, so a hold is "no
// order" rather than an order parked on a group. With a node, the string is a
// note for the audit row ("" or the overflow hop); without one, the refusal. Branches on the SOURCE's
// class; a bare position that is none of these is refused, because a claim
// naming one names a line-side place, not storage.
//
// A LANE IS NOT A SOURCE. Lines name node groups or loader positions, never
// lanes (owner, 2026-10-03), and the config checks refuse a lane as a claim's
// source. A lane that reaches here anyway is refused rather than resolved
// through its group: a need that names a lane searches that lane only
// (SourceFinder tier 1), so a bin set down in a sibling lane would be one its
// line never finds.
func (e *Engine) storeInto(src *nodes.Node, bin *bins.Bin) (*nodes.Node, string) {
	if src == nil {
		return nil, "it names no node Core knows"
	}
	if home, err := e.db.GetLoaderHomeByPositionNode(src.ID); err == nil && home != nil {
		return e.storeIntoLoader(home, bin)
	}
	group := src
	switch src.NodeTypeCode {
	case protocol.NodeClassNGRP:
	case protocol.NodeClassLANE:
		return nil, "a lane is not a source; name its group"
	default:
		return nil, "it is a single position, not a store group or a loader position"
	}
	gr, stated := e.maintainerResolver(), binresolver.KnownBinType(bin.BinTypeID)
	res, err := gr.ResolveStore(group, bin.PayloadCode, stated, reservations.Anyone)
	if err == nil && res != nil && res.Node != nil {
		return res.Node, ""
	}
	// A MAINTAINED GROUP AT ITS LEVEL OVERFLOWS, ONE HOP, as any store into it
	// does at intake (LifecycleService.tryOverflow): the keeper topping a bank
	// back up after this bin was lifted out of it is ordinary, and the plant has
	// already said where the next carrier goes. Only that refusal — a group
	// that is full, or a fence that refuses this bin, holds as before — and
	// only one hop: an overflow at its own level holds rather than consulting
	// its overflow.
	//
	// THE HOP FOLLOWS A DECLARATION BY THE BANK, NOT BY A CONSUMER. No retrieve
	// tier reads overflow_destination, so a bin that took the hop is found by
	// blank-source needs (the keeper's top-ups, the plant-wide tier) only.
	// Empties-only in practice: a payload is refused by an empties bank before
	// its level is asked.
	if !errors.Is(err, binresolver.ErrAtDeclaredLevel) {
		return nil, refusalText(err, "no slot in "+group.Name)
	}
	over, why := dispatch.OverflowGroupOf(e.db, group)
	if over == nil {
		if why == "" {
			why = "it names no overflow"
		}
		return nil, group.Name + " is at its declared level and " + why
	}
	res, err = gr.ResolveStore(over, bin.PayloadCode, stated, reservations.Anyone)
	if err != nil || res == nil || res.Node == nil {
		return nil, fmt.Sprintf("%s is at its declared level; its overflow %s refused too: %s",
			group.Name, over.Name, refusalText(err, "no slot"))
	}
	return res.Node, group.Name + " at its declared level, overflowed to " + over.Name
}

// storeIntoLoader is placeForLoader's reads for a source that is a loader's
// position: the loader's home for this payload when it is free and takes this
// carrier, else the first free buffer of the same loader.
//
// A loader position reserves nothing (isStorageDropoff is false for it), so
// "free" here is CheckDropoffCapacity's answer at this instant, which is the
// same race placeForLoader lives with.
func (e *Engine) storeIntoLoader(home *loaders.Home, bin *bins.Bin) (*nodes.Node, string) {
	l, err := e.db.GetLoader(home.LoaderID)
	if err != nil || l == nil || l.ArchivedAt != nil {
		return nil, "its loader is archived or unreadable"
	}
	members, err := e.db.ListLoaderHomes(l.ID)
	if err != nil {
		return nil, "its loader's positions could not be read"
	}
	capable, err := e.db.ListLoaderHomeBinTypes(l.ID)
	if err != nil {
		return nil, "its loader's window types could not be read"
	}
	takes := func(m loaders.Home) bool {
		types, restricted := capable[m.PositionNodeID]
		if !restricted {
			return true
		}
		for _, code := range types {
			if code == bin.BinTypeCode {
				return true
			}
		}
		return false
	}
	free := func(m loaders.Home) *nodes.Node {
		n, nerr := e.db.GetNode(m.PositionNodeID)
		if nerr != nil || n == nil || !n.Enabled || !takes(m) {
			return nil
		}
		if blocked, _ := dispatch.CheckDropoffCapacityForType(e.db, n.Name, 0, &bin.BinTypeID); blocked {
			return nil
		}
		return n
	}
	for _, m := range members {
		if m.Kind != loaders.HomeKindBuffer && (m.PayloadCode == "" || m.PayloadCode == bin.PayloadCode) {
			if n := free(m); n != nil {
				return n, ""
			}
		}
	}
	for _, m := range members {
		if m.Kind == loaders.HomeKindBuffer {
			if n := free(m); n != nil {
				return n, ""
			}
		}
	}
	return nil, fmt.Sprintf("loader %s has no free home or buffer for it", l.Name)
}

// chooseReturn binds the policy into the door's choose. It walks returnSources
// and takes the first storeInto answer; with none, it refuses with a sentence
// naming every source and why it said no, so the hold row is the whole story.
//
// The door hands choose the carrier from its own read. If that is no longer
// the carrier the watch decided on, the bin's job changed under the attempt and
// the policy declines rather than return a bin for an order it did not judge.
func (e *Engine) chooseReturn(judged *orders.Order) func(*bins.Bin, fleet.RobotStatus, *orders.Order) (*nodes.Node, string, error) {
	return func(bin *bins.Bin, robot fleet.RobotStatus, carrier *orders.Order) (*nodes.Node, string, error) {
		if carrier == nil || carrier.ID != judged.ID {
			return nil, "", fmt.Errorf("the bin's last order is no longer cancelled order %d", judged.ID)
		}
		return e.chooseDeclared(bin, robot, carrier)
	}
}

// chooseDeclared is the one chooser both triggers share — the watch through
// chooseReturn, the bins page's Return button directly. It walks returnSources
// and takes the first storeInto answer; with none, it refuses with a sentence
// naming every place tried and why it said no. carrier may be nil (a bin with
// no order on record): the walk then has no "own process" to put first.
func (e *Engine) chooseDeclared(bin *bins.Bin, _ fleet.RobotStatus, carrier *orders.Order) (*nodes.Node, string, error) {
	srcs, err := e.returnSources(bin, carrier)
	if err != nil {
		return nil, "", fmt.Errorf("could not read where %s is sourced from: %v", describeBinLoad(bin), err)
	}
	if len(srcs) == 0 {
		if bin.PayloadCode == "" {
			return nil, "", fmt.Errorf("no claim declares a place for an empty %s", bin.BinTypeCode)
		}
		return nil, "", fmt.Errorf("no claim reports where %s is sourced from", bin.PayloadCode)
	}
	var refusals []string
	for _, s := range srcs {
		if bin.PayloadCode == "" {
			if fenced, why := e.fencedFor(s.node, carrier); fenced {
				refusals = append(refusals, s.name+": "+why)
				continue
			}
		}
		dest, why := e.storeInto(s.node, bin)
		if dest != nil {
			tier := "returned to " + s.name + ", where " + describeBinLoad(bin) + " is sourced from"
			if bin.PayloadCode == "" {
				tier = "returned to " + s.name + ", a declared place for " + describeBinLoad(bin)
			}
			if why != "" {
				tier += "; " + why
			}
			return dest, tier, nil
		}
		refusals = append(refusals, s.name+": "+why)
	}
	return nil, "", fmt.Errorf("every place %s is sourced from refused it (%s)",
		describeBinLoad(bin), strings.Join(refusals, "; "))
}

// carrierProcessNodes: the nodes that make a claim "the cancelled order's own".
// Its process node when it carries one, and its delivery and source nodes —
// a supply order delivers to the line that claims the payload, an evacuation
// lifts off it — deduplicated, blanks dropped.
func carrierProcessNodes(o *orders.Order) []string {
	if o == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, n := range []string{o.ProcessNode, o.DeliveryNode, o.SourceNode} {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

func describeBinLoad(bin *bins.Bin) string {
	if bin.PayloadCode == "" {
		return "an empty " + bin.BinTypeCode
	}
	return bin.PayloadCode
}

func refusalText(err error, fallback string) string {
	if err != nil {
		return err.Error()
	}
	return fallback
}

// returnTried reports whether this (bin, carrier) episode has had its attempt.
func (e *Engine) returnTried(binID, carrierID int64) bool {
	e.dropObsMu.Lock()
	defer e.dropObsMu.Unlock()
	return e.returnAttempted[binID] == carrierID
}

// markReturnTried is the test-and-set: true if this call recorded the episode,
// false if another host already had. Under dropObsMu beside the drop maps, and
// RELEASED before the caller reaches the door, whose forgetDrop takes the same
// mutex.
//
// Keyed on the CARRIER, not the bin: a bin whose failed carrier stamped it can
// stay on the deck and be claimed again through a named-bin door, and that
// later cancel is a new episode with its own attempt.
func (e *Engine) markReturnTried(binID, carrierID int64) bool {
	e.dropObsMu.Lock()
	defer e.dropObsMu.Unlock()
	if e.returnAttempted == nil {
		e.returnAttempted = map[int64]int64{}
	}
	if e.returnAttempted[binID] == carrierID {
		return false
	}
	e.returnAttempted[binID] = carrierID
	return true
}
