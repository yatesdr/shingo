package engine

import (
	"encoding/json"
	"fmt"
	"slices"

	"shingo/protocol"
	"shingoedge/release"
	"shingoedge/store/orders"
)

// legPlacesBinAt is the supply/evac discriminator: whether this leg LEAVES A
// BIN at node when it finishes (release.PlacesBinAt, its one definition).
func legPlacesBinAt(steps []protocol.ComplexOrderStep, node string) bool {
	return release.PlacesBinAt(steps, node)
}

// orderPlacesBinAtAny reports whether a live order will place a bin at any of
// the given nodes. It is the destination question both changeover gates ask, and
// it is TWO-ARMED because the store keeps the answer in a different column for
// each order shape (the delivery_node comment on the orders table,
// store/schema/sqlite_ddl.go):
//
//	steps_json == ""  simple order — delivery_node is "authoritative for SIMPLE
//	                  orders (one bin, one destination)"
//	steps_json != ""  complex order — delivery_node is "effectively a DISPLAY
//	                  value; nothing correctness-critical reads it any more",
//	                  so the steps are the only truth
//
// WHY THE EMPTY CASE IS NOT A BLOCK, which is the trap this function exists to
// avoid. orderGatesCutover fails closed on empty steps, and `steps_json TEXT NOT
// NULL DEFAULT ”` is the DDL default — so a simple order there is not
// destination-tested at all, it is an unconditional yes. Reading steps per order
// is fine; inheriting that default is not. Empty steps means "simple order, use
// the delivery-node arm", never "gate".
//
// Fail-closed remains correct for the two cases where the shape cannot be
// established: an unreadable row and undecodable steps. Those are "we cannot
// prove this leg is irrelevant", which is different from "this leg has no steps
// because simple orders never write any".
func (e *Engine) orderPlacesBinAtAny(orderID int64, deliveryNode string, nodes []string) bool {
	stepsJSON, err := e.db.GetOrderStepsJSON(orderID)
	if err != nil {
		return true // cannot read the row — cannot prove it is irrelevant
	}
	if stepsJSON == "" {
		for _, n := range nodes {
			if n != "" && n == deliveryNode {
				return true
			}
		}
		return false
	}
	steps, err := decodeSteps(stepsJSON)
	if err != nil {
		return true // undecodable steps fail closed, as they do at the cutover gate
	}
	for _, n := range nodes {
		if legPlacesBinAt(steps, n) {
			return true
		}
	}
	return false
}

// orderRefillsNodeItself reports whether this order's OWN plan already brings
// coreNode its replacement carrier — the question handleSequentialBackfill has
// to ask before minting a second one.
//
// It is a property of the order, not of the order's origin. The two sequential
// Order A shapes come out of two builders and differ in exactly this:
//
//	BuildSequentialRemovalSteps        wait(N) pickup(N) dropoff(OUT)
//	  removes only — the position gets nothing back, so a backfill is the
//	  only thing that will feed it. false.
//	buildSequentialPerPositionSwap     wait(N) pickup(N) dropoff(OUT)
//	                                     pickup(IN) dropoff(N)
//	  a round trip — it fetches the new style's carrier and sets it down on
//	  the same position. true, and a backfill would be a SECOND carrier for
//	  one slot. (buildSequentialPerPositionEvacuate has the same tail and is
//	  covered by the same read.)
//
// Asking the shape rather than "is a changeover running" is what keeps the two
// answers from drifting: a builder that starts or stops refilling its own node
// changes this predicate's answer on the same commit that changes the plan.
//
// Pinned by TestSequentialChangeover_OnePositionGetsOneCarrier and its mirror
// TestSequentialSteadyState_RemovalStillMintsTheBackfill.
//
// DeliveryNode is passed for the SIMPLE-order arm only, and a sequential Order A
// is always complex, so in practice the steps answer. See orderPlacesBinAtAny
// for why the empty-steps case is the delivery-node arm and never a block.
func (e *Engine) orderRefillsNodeItself(order *orders.Order, coreNode string) bool {
	if order == nil || coreNode == "" {
		return false
	}
	return e.orderPlacesBinAtAny(order.ID, order.DeliveryNode, []string{coreNode})
}

func decodeSteps(stepsJSON string) ([]protocol.ComplexOrderStep, error) {
	if stepsJSON == "" {
		return nil, fmt.Errorf("no steps stored")
	}
	var steps []protocol.ComplexOrderStep
	if err := json.Unmarshal([]byte(stepsJSON), &steps); err != nil {
		return nil, fmt.Errorf("decode steps: %w", err)
	}
	return steps, nil
}

// liftAhead is where a live order stands against the next lift its steps make
// at one of a set of nodes.
type liftAhead int

const (
	// liftNone: no lift at those nodes is still ahead of it.
	liftNone liftAhead = iota
	// liftHeld: a lift is ahead and the order is not yet released towards it;
	// the bin is still on the node and no robot is going for it.
	liftHeld
	// liftLeaving: the order is released towards the lift and the node's seat
	// still holds a bin; the bin is leaving.
	liftLeaving
)

// orderLiftAheadAt reports whether a live order still has a lift ahead of it
// at one of nodes, and at which. It is read from the order's steps and where it
// stands in them, never from the mode that built it:
//
//   - its steps: a complex order's own; a simple order's are the one pickup at
//     its source and the set-down at its delivery node;
//   - its release point: a lift behind k station waits is released once the
//     order has been released past the k-th. A lift with no station wait before
//     it has nothing to hold it once the order is with the fleet, and an order
//     not yet with the fleet has no robot going for anything;
//   - once released, the node's seat (seatAt): the planner's own question, is
//     a bin on the node. A seat that still holds one, or cannot say, is a lift
//     still ahead; a seat whose carrier has left is a lift behind the order.
//
// Before the release no lift can have happened, so the seat is not asked. An
// unreadable row answers liftNone. The placing question (orderPlacesBinAtAny)
// fails closed on the same row, so the start gate still sees it.
func (e *Engine) orderLiftAheadAt(o *orders.Order, nodes []string, seat func(coreNode string) Seat) (liftAhead, string) {
	steps, ok := e.orderSteps(o)
	if !ok {
		return liftNone, ""
	}
	waits := 0
	for _, s := range steps {
		if s.Action == protocol.ActionWait && protocol.IsStationWaitKind(s.WaitKind) {
			waits++
			continue
		}
		if s.Action != protocol.ActionPickup || !slices.Contains(nodes, s.Node) {
			continue
		}
		switch {
		case protocol.ChangeoverStartActionFor(o.Status) == protocol.ChangeoverStartCancel:
			return liftHeld, s.Node
		case waits > 0 && releasedWaits(o) < waits:
			return liftHeld, s.Node
		case seat(s.Node) != SeatEmpty:
			return liftLeaving, s.Node
		}
	}
	return liftNone, ""
}

// orderSteps is an order's steps; a simple order's are its one pickup at its
// source and set-down at its delivery node. ok=false for a row that cannot be
// read, and for a simple order with no source.
func (e *Engine) orderSteps(o *orders.Order) ([]protocol.ComplexOrderStep, bool) {
	stepsJSON, err := e.db.GetOrderStepsJSON(o.ID)
	if err != nil {
		return nil, false
	}
	if stepsJSON == "" {
		if o.SourceNode == "" || o.SourceNode == o.DeliveryNode {
			return nil, false
		}
		return []protocol.ComplexOrderStep{
			{Action: protocol.ActionPickup, Node: o.SourceNode},
			{Action: protocol.ActionDropoff, Node: o.DeliveryNode},
		}, true
	}
	steps, err := decodeSteps(stepsJSON)
	return steps, err == nil
}

// releasedWaits is how many of the order's station waits it has been released
// past: those before the one it is staged at or heading to (release.PointOf),
// and that one too once its release has gone out.
func releasedWaits(o *orders.Order) int {
	intent, err := release.DecodeIntent(o.ReleaseIntent)
	if err != nil {
		intent = nil
	}
	p := release.PointOf(o.Status, o.StationWait, o.WaitKind, intent, nil)
	if p.AtWait && intent != nil && intent.Sent() && intent.StationWait == p.Ordinal {
		return p.Ordinal + 1
	}
	return p.Ordinal
}

// Seat is what a process node's seat says stands on it.
type Seat int

const (
	// SeatUnknown: the seat cannot say. Its carrier was never bound (placed
	// by a path that sent no landing), or the row cannot be read.
	SeatUnknown Seat = iota
	// SeatBin: a bin is bound to the seat.
	SeatBin
	// SeatEmpty: no bin is bound and the last carrier is recorded as gone
	// (carrierLeft writes the departure with the pointer).
	SeatEmpty
)

// nodeStateAt reads what stands on each of a process's nodes, by core node
// name: the seat, the carrier's part and the counter's drain answer, all from
// the node's one runtime row. It is the one reader behind the changeover
// planner's passes (the reuse shortcut's drain, ApplyNodeState, the old
// carrier's aim) and the start gate's seat read (seatAt): the process and its
// nodes are read once, and each node's row at most once, however many passes
// ask. A node the process does not own answers the zero NodeState: seat
// unknown, part unknown, drain unknown.
func (e *Engine) nodeStateAt(processID int64) func(coreNode string) NodeState {
	process, _ := e.db.GetProcess(processID)
	wired := counterWired(process)
	nodes, err := e.db.ListProcessNodesByProcess(processID)
	if err != nil {
		return func(string) NodeState { return NodeState{} }
	}
	read := map[string]NodeState{}
	return func(coreNode string) NodeState {
		if st, ok := read[coreNode]; ok {
			return st
		}
		var st NodeState
		if node := findNodeByCoreName(nodes, coreNode); node != nil {
			if rt, err := e.db.GetProcessNodeRuntime(node.ID); err == nil && rt != nil {
				st = NodeState{Part: string(rt.LinesidePayloadCode), PartKnown: rt.LinesidePayloadKnown,
					Drain: drainOf(rt, wired), runtime: rt}
				switch {
				case rt.ActiveBinID != nil:
					st.Seat = SeatBin
				case carrierGone(rt):
					st.Seat = SeatEmpty
				}
			}
		}
		read[coreNode] = st
		return st
	}
}

// seatAt is nodeStateAt's seat alone, for the start gate (orderLiftAheadAt),
// which asks only whether a bin is on a node.
func (e *Engine) seatAt(processID int64) func(coreNode string) Seat {
	state := e.nodeStateAt(processID)
	return func(coreNode string) Seat { return state(coreNode).Seat }
}

// storedStepsOf is a stored order's steps.
func (e *Engine) storedStepsOf(orderID int64) ([]protocol.ComplexOrderStep, error) {
	raw, err := e.db.GetOrderStepsJSON(orderID)
	if err != nil {
		return nil, err
	}
	return decodeSteps(raw)
}
