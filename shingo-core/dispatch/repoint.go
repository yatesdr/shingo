package dispatch

import (
	"encoding/json"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store/orders"
)

// repoint.go — THE ONE WRITER OF A STEP'S NODE once an order has a plan.
//
// Core moves a step for a handful of reasons: a carrier goes by what it holds
// (place_by_contents.go), a contained part goes to containment, a loader's
// return parks at its home or a buffer (loader_place.go), the lane gate picks
// the slot of its group the robot can reach (lane_gate_choose.go), a dug bin's
// pickup follows it. They decide; repoint writes. One writer is what lets the
// plan and everything that hangs off it move together:
//
//	the plan itself     steps_json, and before the splice the very slice the
//	                    pass goes on to send, so the create carries the choice
//	                    rather than waiting for a release to overlay it
//	the waits           after the splice, Core's waits on the rest of the route
//	                    are planned again by the splice (spliceCoreWaitsFrom):
//	                    a lane wait for the lane the step is now in, none for a
//	                    slot in no lane, a turn wait for a turn node
//	the columns         delivery_node when the step is the trip's last drop,
//	                    source_node when it named the pickup moved
//	the carts           order_bins' step positions, moved with the plan
//
// A drop moved after the splice without its waits is the shape the lane gate
// refuses on every pass ("mis-spliced plan"): a wait for the market's lane in
// front of a drop that is now in a kanban. Moved into a lane it was never
// spliced for, it is worse: no wait at all, and the robot enters a marked
// lane ungated. The splice's own checks run over what it plans here, so a
// plan this writes is one the splice would have written.
//
// If the splice refuses the new route, nothing is written and the step stays
// where it was: the old route is the one its waits were planned for.

// stepMove is one step's new place: step at goes to node and carries group
// as its group stamp ("" when Core chose the place for the carrier, not from
// a group the gate may choose across again). action is what the caller holds
// step at to be; a step that is something else is a plan and a caller that
// disagree, and nothing is moved.
type stepMove struct {
	at     int
	action string
	node   string
	group  string
}

// repoint moves one step of the order's plan and stores the plan.
//
// after nil: the plan has not been spliced (the order is acquiring). steps is
// the plan the pass holds and is edited in place, so the create sends the
// moved step, and the stored plan's step at the same index is moved with it
// (the pass's plan and the stored one share their indexes before the splice;
// the stored one may hold the pass's other choices in another form, a group
// re-made as its intent, so only the one step is written); the create splices
// it as it splices every plan.
//
// after set: the plan is spliced and the fleet has every step before after.at.
// Core's waits from after.at on are stripped (withoutInsertedWaits), the step
// moved, and spliceCoreWaitsFrom plans the waits of the rest of the route, the
// robot in lane after.inside when it reaches after.at. steps is not changed;
// the stored plan is returned.
func (d *Dispatcher) repoint(order *orders.Order, steps []resolvedStep, mv stepMove, after *spliceFrom) ([]resolvedStep, error) {
	if mv.at < 0 || mv.at >= len(steps) || steps[mv.at].Action != mv.action {
		return nil, fmt.Errorf("repoint order %d: step %d is not the %s the caller names (%d steps)",
			order.ID, mv.at, mv.action, len(steps))
	}
	was := steps[mv.at].Node
	if after == nil {
		if order.StepsJSON == "" {
			// No stored plan (a row written before its plan): the column alone.
			moveStep(&steps[mv.at], mv)
			return steps, d.storePlan(order, steps, mv, was, nil, false)
		}
		stored, ok := decodeSteps(order.StepsJSON)
		if !ok || mv.at >= len(stored) || stored[mv.at].Action != mv.action {
			return nil, fmt.Errorf("repoint order %d: its stored plan has no %s at step %d", order.ID, mv.action, mv.at)
		}
		moveStep(&stored[mv.at], mv)
		if err := d.storePlan(order, stored, mv, was, nil, true); err != nil {
			return nil, err
		}
		moveStep(&steps[mv.at], mv)
		return steps, nil
	}
	if mv.at < after.at {
		return nil, fmt.Errorf("repoint order %d: step %d is behind step %d, which the fleet already has", order.ID, mv.at, after.at)
	}
	plan := append(append(make([]resolvedStep, 0, len(steps)), steps[:after.at]...), withoutInsertedWaits(steps[after.at:])...)
	k := after.at
	for i := after.at; i < mv.at; i++ {
		if !isInsertedWait(steps[i]) {
			k++
		}
	}
	moveStep(&plan[k], mv)
	out, _, _, err := d.spliceCoreWaitsFrom(plan, order.ID, *after)
	if err == nil {
		// The wait the robot stands at gates the first step of the new route:
		// it is checked with the waits the splice planned, which the splice
		// skips when the rest of the route enters no lane.
		err = d.assertEachWaitGatesItsEntryFrom(out, max(after.at-1, 0))
	}
	if err != nil {
		return nil, fmt.Errorf("repoint order %d: step %d to %s: the rest of the route cannot be planned: %w",
			order.ID, mv.at, mv.node, err)
	}
	if err := d.storePlan(order, out, mv, was, planShift(steps, out), true); err != nil {
		return nil, err
	}
	if waitsDiffer(steps[after.at:], out[after.at:]) {
		log.Printf("dispatch: order %d: %s moved from %s to %s; the rest of the route is planned again: %s → %s",
			order.ID, mv.action, was, mv.node, describeWaits(steps[after.at:]), describeWaits(out[after.at:]))
	}
	return out, nil
}

// moveStep is the edit itself. A moved drop is not the drop a vacate stamp
// was granted for.
func moveStep(s *resolvedStep, mv stepMove) {
	s.Node, s.Group, s.Vacate = mv.node, mv.group, nil
}

// storePlan writes the plan (writePlan), then the columns that follow the
// moved step, then the carts (shift: their positions old to new; nil when none
// moved). The plan first: every append is cut from steps_json, so a crash after
// it leaves a column a later pass can read the plan to correct, never a column
// the plan does not mention.
func (d *Dispatcher) storePlan(order *orders.Order, steps []resolvedStep, mv stepMove, was string, shift map[int]int, writePlan bool) error {
	if writePlan {
		j, err := json.Marshal(steps)
		if err != nil {
			return fmt.Errorf("repoint order %d: marshal its plan: %w", order.ID, err)
		}
		if err := d.db.UpdateOrderStepsJSON(order.ID, string(j)); err != nil {
			return fmt.Errorf("repoint order %d: store its plan: %w", order.ID, err)
		}
		order.StepsJSON = string(j)
	}
	switch {
	case mv.action == protocol.ActionDropoff && lastDropIndex(steps) == indexOfMoved(steps, mv, shift):
		// delivery_node is the trip's end (C2a A.5): an interior drop moves in
		// the plan alone.
		if err := d.db.UpdateOrderDeliveryNode(order.ID, mv.node); err != nil {
			return fmt.Errorf("repoint order %d: delivery node %s: %w", order.ID, mv.node, err)
		}
		order.DeliveryNode = mv.node
	case mv.action == protocol.ActionPickup && order.SourceNode == was:
		if err := d.db.UpdateOrderSourceNode(order.ID, mv.node); err != nil {
			return fmt.Errorf("repoint order %d: source node %s: %w", order.ID, mv.node, err)
		}
		order.SourceNode = mv.node
	}
	if len(shift) == 0 {
		return nil
	}
	if _, err := d.moveCarts(order, shift); err != nil {
		return fmt.Errorf("repoint order %d: move its carts with the plan: %w", order.ID, err)
	}
	if vErr := cartsPlanMismatch(steps, order.Carts); vErr != nil {
		log.Printf("INVARIANT: order %d's carts do not describe the plan it will run after a re-plan: %v", order.ID, vErr)
	}
	return nil
}

// indexOfMoved is the moved step's index in the stored plan.
func indexOfMoved(steps []resolvedStep, mv stepMove, shift map[int]int) int {
	if to, ok := shift[mv.at]; ok {
		return to
	}
	return mv.at
}

// isInsertedWait reports whether s is a wait the splice inserts: a lane's or a
// turn's. Every other step is the plan's own.
func isInsertedWait(s resolvedStep) bool {
	return s.Action == protocol.ActionWait && (s.WaitKind == WaitKindLane || s.WaitKind == WaitKindTurn)
}

// planShift maps each step of before that the splice did not insert to its
// index in after, for the steps that moved: the same steps in the same order,
// with Core's waits between them changed.
func planShift(before, after []resolvedStep) map[int]int {
	shift := map[int]int{}
	j := 0
	for i, s := range before {
		if isInsertedWait(s) {
			continue
		}
		for j < len(after) && isInsertedWait(after[j]) {
			j++
		}
		if j < len(after) && i != j {
			shift[i] = j
		}
		j++
	}
	return shift
}

// waitsDiffer reports whether two routes carry different inserted waits.
func waitsDiffer(a, b []resolvedStep) bool {
	return describeWaits(a) != describeWaits(b)
}

// describeWaits names a route's inserted waits for the log: kind and node.
func describeWaits(steps []resolvedStep) string {
	out := ""
	for _, s := range steps {
		if !isInsertedWait(s) {
			continue
		}
		if out != "" {
			out += ", "
		}
		out += s.WaitKind + " wait at " + s.Node
	}
	if out == "" {
		return "no wait"
	}
	return out
}
