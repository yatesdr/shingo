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
// A leg LEAVES THE CELL'S NODES when the fleet has confirmed the last step in
// its steps_json whose node is in the claim's cell set: CoreNodeName, both
// paired index positions, and BOTH staging nodes (a leg still holding a staging
// slot has not left, however far its robot has driven). It is still a live
// order; what it stops being is the cell's, which is what the admission guards,
// the station card and the level sweep ask about.
//
// DEPARTURE IS A CONJUNCTION of two facts, not one:
//
//	(a) the robot has LEFT THE CELL'S NODES — the fleet has confirmed the last
//	    step of steps_json whose node is in the cell set (`cell_left_at`); and
//	(b) the leg's own PLACEMENT at the line position is RECORDED — the cell's
//	    runtime shows a bin.
//
// For most legs the two land together and (b) is already true when (a) fires.
// They separate for single_robot: step 7 places the fresh carrier on the line,
// step 8 lifts the spent one off OutboundStaging, and step 8 is the last cell
// step because cellSetFor admits both staging nodes. So the leg leaves the
// cell's nodes ONE STEP AFTER it has placed and BEFORE the placement is on the
// books — and a leg stamped departed in that window is filtered out of every
// admission reader while the position it just filled still reads empty. That is
// the double-supply race, re-opened from the other side (0cc734c3).
//
// The proof events are BinPickedUp and IsTerminal for (a), and the Edge's own
// bind for (b) — HandleUOPAdjustment's Bound arm, fed by Core's intermediate
// dropoff, which calls settleCellPlacement so arrival order cannot matter. A
// leg's last cell step is either a pickup with steps after it (stamped) or its
// own final step (terminal covers it). There is deliberately no
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
	// The claim's own geometry, plus its two staging nodes — which are part of
	// the cell for departure purposes but are not positions the cell occupies,
	// so they are added here rather than folded into Positions.
	for _, n := range claim.Positions() {
		cell[n] = true
	}
	for _, n := range []string{claim.InboundStaging, claim.OutboundStaging} {
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

// stampDepartureIfLeftCell records that this leg's robot has left the cell's
// nodes, when the pickup Core just reported is the last cell step in the leg's
// own plan — and completes the departure if the leg's placement is already on
// the books. When it is not, settleCellPlacement finishes it later.
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
	// requestedClaimAtNode, not runtime.ActiveClaimID: the runtime pointer is the
	// swap machinery's and is nil for long stretches of a cell's life, while the
	// claim resolved from the process's active style is the one that describes
	// the cell's GEOMETRY — the only thing wanted here.
	claim := requestedClaimAtNode(e.db, node)
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
	// Fact (a): the robot has left the cell's nodes. Stamped unconditionally —
	// it is a physical event and it is true whatever the placement record says.
	leftChanged, lerr := e.db.MarkOrderLeftCell(order.ID, time.Now().UTC())
	if lerr != nil {
		e.logFn("bin_picked_up: order %d — stamp cell-left at %s: %v", order.ID, departNode, lerr)
		return
	}

	// Fact (b). A leg that still owes this cell a placement nobody has recorded
	// is not departed — it has put a carrier on the line and cannot prove it.
	// settleCellPlacement finishes the departure when the bind lands.
	if e.cellPlacementOutstanding(node, claim, steps) {
		if leftChanged {
			e.logFn("bin_picked_up: order %d (%s) left the cell at %s but its own placement at %s is not "+
				"recorded — the cell stays shut until it lands", order.ID, order.UUID, departNode, claim.CoreNodeName)
		}
		return
	}
	e.markDeparted(order, departNode)
}

// cellPlacementOutstanding answers "does this leg owe the cell a placement that
// nobody has recorded yet?"
//
// Two halves, and neither invents a spelling:
//
//   - legPlacesBinAt(steps, claim.CoreNodeName) is the repo's ONE predicate for
//     "this leg leaves a bin on the machine". It already drives confirmPolicy,
//     the delivered gate and the standard's receipt rule.
//   - "recorded" is the cell's runtime showing a bin. That pointer is cleared by
//     the leg's own press pickup (handler_bin_picked_up.go, gated on bin
//     identity) and re-set by the step-7 intermediate dropoff's
//     UOPAdjustment{Bound}. It is nil across exactly this window.
//
// A node-scoped witness is unambiguous by construction: exactly one leg per
// cycle places at claim.CoreNodeName, which
// TestEverySwapLegDepartsProvablyAndConfirmsOnPlacement fails the build over. An
// order-scoped one would need OrderUUID on protocol.UOPAdjustment — a wire
// change, and not one to make before a call site needs it.
//
// FAIL-CLOSED: an unreadable runtime answers "outstanding". A wrong "departed"
// admits a second delivery into a position a robot is about to fill; a wrong
// "still working the cell" costs a reopen the leg's own terminal gives back.
func (e *Engine) cellPlacementOutstanding(node *processes.Node, claim *processes.NodeClaim, steps []protocol.ComplexOrderStep) bool {
	if node == nil || claim == nil {
		return false
	}
	if !legPlacesBinAt(steps, claim.CoreNodeName) {
		return false
	}
	runtime, err := e.db.GetProcessNodeRuntime(node.ID)
	if err != nil || runtime == nil {
		return true
	}
	return runtime.ActiveBinID == nil
}

// markDeparted writes the stamp and says so once. Split out of
// stampDepartureIfLeftCell because settleCellPlacement completes the same
// departure from the other trigger, and one sentence has to cover both.
func (e *Engine) markDeparted(order *domain.Order, departNode string) {
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

// settleCellPlacement is the departure's SECOND trigger: the placement record
// landing after the robot has already left.
//
// Called from the Bound arm of HandleUOPAdjustment, the one Edge site that
// records a bin onto a cell. It walks the node's live orders, and for any leg
// that has left the cell's nodes but was held back because its placement was
// unrecorded, it completes the departure. Whichever of the two facts arrives
// second finishes it, so ARRIVAL ORDER STOPS MATTERING.
//
// ONE call site, not two. handleNodeOrderDelivered is the other bind, and every
// leg that reaches it departs by departureKindFinal — it never has cell_left_at
// set, so a second call there would guard a state no builder produces.
//
// Best-effort and silent on a read failure, like every other path in this file:
// a leg that stays undeparted holds its cell until terminal, which is the
// fail-closed direction.
func (e *Engine) settleCellPlacement(nodeID int64) {
	node, err := e.db.GetProcessNode(nodeID)
	if err != nil || node == nil {
		return
	}
	claim := requestedClaimAtNode(e.db, node)
	if claim == nil {
		return
	}
	rows, err := e.db.ListActiveOrdersByProcessNode(nodeID)
	if err != nil {
		return
	}
	cell := cellSetFor(claim)
	for i := range rows {
		order := &rows[i]
		if order.CellLeftAt == nil || order.Departed {
			continue
		}
		stepsJSON, serr := e.db.GetOrderStepsJSON(order.ID)
		if serr != nil {
			continue
		}
		steps, derr := decodeSteps(stepsJSON)
		if derr != nil {
			continue
		}
		if e.cellPlacementOutstanding(node, claim, steps) {
			continue
		}
		_, departNode, ok := legDepartsAt(steps, cell)
		if !ok {
			continue
		}
		e.markDeparted(order, departNode)
	}
}
