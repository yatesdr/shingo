package engine

import (
	"strings"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// Leg departure — "this leg is no longer the cell's business".
//
// A leg is DEPARTED when the fleet has confirmed the last step in its steps_json
// whose node is in the claim's cell set: CoreNodeName, both paired index
// positions, and BOTH staging nodes (a leg still holding a staging slot has not
// left, however far its robot has driven). It is still a live order; what it
// stops being is the cell's, which is what the admission guards, the station
// card and the level sweep ask about.
//
// The two proof events are BinPickedUp and IsTerminal, nothing else: a leg's
// last cell step is either a pickup with steps after it (stamped) or its own
// final step (terminal covers it). There is deliberately no
// `switch claim.SwapMode` here — the positional rule this replaced is exactly
// what broke. A new swap mode is a new step builder; it inherits departure and
// confirm for free, or it fails
// TestEverySwapLegDepartsProvablyAndConfirmsOnPlacement.
//
// See docs/order-lifecycle.md § Departed legs and cell-done.

const (
	// departureKindPickup: the last cell step is a pickup that is not the leg's
	// final step. BinPickedUp at that location is the proof.
	departureKindPickup = "pickup"
	// departureKindFinal: the last cell step IS the final step, so the leg
	// cannot leave the cell before it is done. Terminal is the proof.
	departureKindFinal = "final"
)

// cellSetFor returns the node names that belong to this claim's cell.
//
// The empty string is never a member: an unconfigured OutboundStaging would
// otherwise match every step whose Node is blank (buildStep emits exactly that
// for a deferred dedicated-loader dropoff) and read the whole market as cell.
func cellSetFor(claim *processes.NodeClaim) map[string]bool {
	cell := map[string]bool{}
	if claim == nil {
		return cell
	}
	for _, n := range []string{
		claim.CoreNodeName,
		claim.PairedCoreNode,
		claim.SecondPairedCoreNode,
		claim.InboundStaging,
		claim.OutboundStaging,
	} {
		if n != "" {
			cell[n] = true
		}
	}
	return cell
}

// lastCellStep returns the index of the last step whose node is in the cell set.
// ok=false means the leg never touches the cell — it departed trivially.
func lastCellStep(steps []protocol.ComplexOrderStep, cell map[string]bool) (int, bool) {
	idx, found := -1, false
	for i, s := range steps {
		if s.Node != "" && cell[s.Node] {
			idx, found = i, true
		}
	}
	return idx, found
}

// legDepartsAt reports HOW this leg leaves the cell, and at which node.
//
// ok=false — the leg never touches the cell, or its last cell step is a dropoff
// (or a wait) followed by off-cell steps, which neither proof event can speak
// about. That is FAIL-CLOSED and correct, not a gap: the leg stays undeparted
// and blocks the cell until terminal, which is the behaviour that predates this.
func legDepartsAt(steps []protocol.ComplexOrderStep, cell map[string]bool) (kind string, node string, ok bool) {
	idx, found := lastCellStep(steps, cell)
	if !found {
		return "", "", false
	}
	last := steps[idx]
	if idx == len(steps)-1 {
		return departureKindFinal, last.Node, true
	}
	if last.Action == protocol.ActionPickup {
		return departureKindPickup, last.Node, true
	}
	return "", "", false
}

// orderWorksTheCell is the ONE admission predicate, and it is one function
// because its five readers must never disagree: CanAcceptOrders and
// hasActiveSwap ask it about the runtime SLOTS, guardPositionSpokenFor's second
// arm and sweepNodeLevel ask it about the durable ROWS at the node, and the
// station card asks it (as `!o.departed`) about the orders it lists.
//
// NOT departed is the fail-closed default: every pre-v39 row, every leg whose
// last cell step is its own final step, and every leg whose shape could not be
// proved all read as still working the cell.
func orderWorksTheCell(o *domain.Order) bool {
	if o == nil {
		return false
	}
	return !orders.IsTerminal(o.Status) && !o.Departed
}

// stampDepartureIfLeftCell records that this leg is done with its cell, when the
// pickup Core just reported is the last cell step in the leg's own plan.
//
// Called from HandleBinPickedUp ABOVE the location gate: a departure is not slot
// work — single_robot departs at OutboundStaging, and a 3-position press's cell
// pickups can be at either index position.
//
// Every early exit is silent and correct — no claim, a leg that never touches
// the cell, a leg whose last cell step is its final step (nothing to stamp), a
// pickup at some other node. Best-effort throughout: a failed read leaves the
// leg undeparted, and fail-closed is the only safe direction, because a wrong
// "departed" admits a second swap into a cell a robot is standing in.
func (e *Engine) stampDepartureIfLeftCell(order *domain.Order, location string) {
	if order == nil || order.ProcessNodeID == nil {
		return
	}
	node, err := e.db.GetProcessNode(*order.ProcessNodeID)
	if err != nil || node == nil {
		return
	}
	// findActiveClaim, not runtime.ActiveClaimID: the runtime pointer is the
	// swap machinery's and is nil for long stretches of a cell's life, while the
	// claim resolved from the process's active style is the one that describes
	// the cell's GEOMETRY — the only thing wanted here.
	claim := findActiveClaim(e.db, node)
	if claim == nil {
		return
	}
	cell := cellSetFor(claim)
	stepsJSON, serr := e.db.GetOrderStepsJSON(order.ID)
	if serr != nil {
		return
	}
	steps, derr := decodeSteps(stepsJSON)
	if derr != nil {
		// A simple order stores no steps, and no swap leg is simple.
		return
	}
	if _, touches := lastCellStep(steps, cell); !touches {
		return
	}
	kind, departNode, ok := legDepartsAt(steps, cell)
	if !ok {
		// THE TRIPWIRE. No current builder emits a shape the two proof events
		// cannot speak about, and the standard test is what keeps it that way.
		// This line is so a cell sitting on a long swap has a sentence naming why.
		e.logFn("bin_picked_up: order %d (%s) — departure unprovable from its steps; the cell stays "+
			"shut until it goes terminal", order.ID, order.UUID)
		return
	}
	if kind != departureKindPickup {
		return
	}
	if strings.TrimSpace(departNode) != strings.TrimSpace(location) {
		return
	}
	changed, merr := e.db.MarkOrderDeparted(order.ID, time.Now().UTC())
	if merr != nil {
		e.logFn("bin_picked_up: order %d — stamp departure at %s: %v", order.ID, departNode, merr)
		return
	}
	if !changed {
		// A replayed BinPickedUp: Core's poller holds block states in memory, so
		// a restart re-fires every already-FINISHED block once. One stamp, one log.
		return
	}
	e.logFn("bin_picked_up: order %d (%s) DEPARTED the cell at %s — it is carrying a bin away and no "+
		"longer blocks the next swap", order.ID, order.UUID, departNode)
}
