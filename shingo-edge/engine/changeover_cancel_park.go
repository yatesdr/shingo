package engine

import (
	"log"
	"slices"

	"shingo/protocol"
	"shingoedge/domain"
	ordermgr "shingoedge/orders"
	"shingoedge/store/processes"
)

// changeover_cancel_park.go — a cancelled changeover leg's parked bin.
//
// The single-robot changeover leg lifts the line's bin, sets it down on the
// outgoing claim's outbound staging, collects the incoming bin, delivers it,
// and only then takes the parked bin on to the outbound destination. A cancel
// in between aborts the leg with the line's bin standing on outbound staging,
// and every later single-robot swap at the line drops there: each one waits
// behind it for good. So the cancel finishes that bin's trip, with one plain
// move to where the aborted leg was taking it.
//
// On a claim that keeps no spare, the changeover stages the incoming bin on
// inbound staging first, by an order of its own, for the leg to collect. A
// cancel after the stage landed leaves that bin where every later swap sets its
// own incoming bin down, so it goes back to where the stage fetched it from, by
// the same plain move and in the same read.

// parkedBin is one bin a changeover order left standing: the line's bin a leg
// parked, bound for dest later in the same leg, or the incoming bin a stage set
// down, with dest the place it was fetched from.
type parkedBin struct {
	nodeID     int64
	line       string
	park, dest string
}

// parkedTrip reads a leg's steps for the park of the bin it lifts off line: a
// staging dropoff straight after the pickup at the line, a later pickup at the
// same node, and the dropoff after that. Only the line's own bin counts: a
// bin parked on inbound staging on its way TO the line is the incoming style's,
// and a cancel must not carry it on. ok is false when the leg parks nothing or
// the dropoff after the park names no node.
func parkedTrip(steps []protocol.ComplexOrderStep, line string) (park, dest string, ok bool) {
	lifted := false
	for i, s := range steps {
		switch s.Action {
		case protocol.ActionPickup:
			lifted = s.Node == line
		case protocol.ActionDropoff:
			if !lifted || !s.ExclusiveSlot || s.Node == "" {
				lifted = false
				continue
			}
			return parkDestination(steps[i+1:], s.Node)
		}
	}
	return "", "", false
}

// parkDestination finds, in the steps after a park on node, the pickup there
// and the dropoff that follows it.
func parkDestination(rest []protocol.ComplexOrderStep, node string) (park, dest string, ok bool) {
	for j, s := range rest {
		if s.Action != protocol.ActionPickup || s.Node != node {
			continue
		}
		for _, d := range rest[j+1:] {
			if d.Action == protocol.ActionDropoff {
				return node, d.Node, d.Node != ""
			}
		}
		return "", "", false
	}
	return "", "", false
}

// stagedTrip reads a stage order's steps for the bin it set down to wait: a
// pickup somewhere other than the line, an exclusive dropoff on a staging node
// straight after it, and no later pickup there in the same order. The bin waits
// for another order to collect it, and dest is where it was fetched from. A
// two-robot supply collects its own staged bin, so it is not a stage.
func stagedTrip(steps []protocol.ComplexOrderStep, line string) (stage, dest string, ok bool) {
	for i := 1; i < len(steps); i++ {
		s, prev := steps[i], steps[i-1]
		if s.Action != protocol.ActionDropoff || !s.ExclusiveSlot || s.Node == "" ||
			prev.Action != protocol.ActionPickup || prev.Node == "" || prev.Node == line {
			continue
		}
		for _, later := range steps[i+1:] {
			if later.Action == protocol.ActionPickup && later.Node == s.Node {
				return "", "", false
			}
		}
		return s.Node, prev.Node, true
	}
	return "", "", false
}

// leftOnStaging is parkedTrip and stagedTrip for one changeover order: one
// steps read, and one node read when the steps leave a bin anywhere. A park
// counts only for a leg aborted now (aborted); a stage only once its order has
// reached a robot (flown), since before that it has set nothing down.
func (e *Engine) leftOnStaging(order *domain.Order, processNodeID int64, aborted, flown bool) (park, stage parkedBin, parked, staged bool) {
	if order.OrderType != ordermgr.TypeComplex || (!aborted && !flown) {
		return
	}
	raw, err := e.db.GetOrderStepsJSON(order.ID)
	if err != nil {
		return
	}
	steps, err := decodeSteps(raw)
	if err != nil {
		return
	}
	node, err := e.db.GetProcessNode(processNodeID)
	if err != nil || node == nil {
		return
	}
	if p, dest, ok := parkedTrip(steps, node.CoreNodeName); ok && aborted {
		park, parked = parkedBin{nodeID: node.ID, line: node.CoreNodeName, park: p, dest: dest}, true
	}
	if s, dest, ok := stagedTrip(steps, node.CoreNodeName); ok && flown {
		stage, staged = parkedBin{nodeID: node.ID, line: node.CoreNodeName, park: s, dest: dest}, true
	}
	return
}

// taskLeftOnStaging collects what one changeover task's orders left standing,
// aborting the live ones as it goes. A stage counts only beside a park: the
// leg that would have collected the staged bin was aborted now, so the bin is
// still waiting, and the read that answers for the park answers for it too.
// Without a live leg the stage was collected, or its leg ended earlier.
func (e *Engine) taskLeftOnStaging(task processes.NodeTask) []parkedBin {
	var parks, stages []parkedBin
	for _, orderID := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
		if orderID == nil {
			continue
		}
		order, err := e.db.GetOrder(*orderID)
		if err != nil {
			continue
		}
		flown := protocol.ChangeoverStartActionFor(order.Status) != protocol.ChangeoverStartCancel &&
			order.Status != protocol.StatusCancelled && order.Status != protocol.StatusFailed
		aborted := !ordermgr.IsTerminal(order.Status)
		if aborted {
			if err := e.orderMgr.AbortOrder(order.ID); err != nil {
				log.Printf("changeover cancel: abort order %s: %v", order.UUID, err)
			}
		}
		park, stage, parked, staged := e.leftOnStaging(order, task.ProcessNodeID, aborted, flown)
		if parked {
			parks = append(parks, park)
		}
		if staged {
			stages = append(stages, stage)
		}
	}
	if len(parks) == 0 {
		return nil
	}
	return append(parks, stages...)
}

// parkNames is every park node, once each.
func parkNames(parks []parkedBin) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range parks {
		if !seen[p.park] {
			seen[p.park] = true
			out = append(out, p.park)
		}
	}
	return out
}

// finishParkedTrips orders, for each park Core reports a bin on, one plain move
// carrying that bin's own payload to its dest.
// Attributed to the line, never in its runtime slots, and created under the
// line's prime lock, as every other order the line's cell decides. One move per
// park node: one node holds one bin.
func (e *Engine) finishParkedTrips(parks []parkedBin, rows map[string]NodeBinInfo, origin ordermgr.Origin) {
	done := map[string]bool{}
	for _, p := range parks {
		row, read := rows[p.park]
		if done[p.park] || !read || !row.Occupied {
			continue
		}
		done[p.park] = true
		mu := e.primeNodeLock(&processes.NodeClaim{CoreNodeName: p.line})
		mu.Lock()
		nodeID := p.nodeID
		_, err := e.orderMgr.CreateMoveOrderCarrying(&nodeID, p.park, p.dest, row.PayloadCode, origin)
		mu.Unlock()
		if err != nil {
			e.logFn("changeover cancel: %s: send the bin left on %s on to %s: %v", p.line, p.park, p.dest, err)
			continue
		}
		e.logFn("changeover cancel: %s: the bin left on %s goes on to %s", p.line, p.park, p.dest)
	}
}

// withoutSpots drops what stands on a keep-staged spot: the spots' own
// decision at the cancel covers whatever stands there.
func withoutSpots(parks []parkedBin, spots []spotChange) []parkedBin {
	if len(spots) == 0 {
		return parks
	}
	out := parks[:0]
	for _, p := range parks {
		if !slices.ContainsFunc(spots, func(ch spotChange) bool { return ch.spot == p.park }) {
			out = append(out, p)
		}
	}
	return out
}

// parkAsked reports whether a request on this claim asks Core about its
// outbound staging: single-robot only, the one swap that parks the line's bin
// there on its way out.
func parkAsked(claim *processes.NodeClaim) bool {
	return claim != nil && claim.SwapMode == protocol.SwapModeSingleRobot && claim.OutboundStaging != ""
}

// clearStrandedPark is the request's half of the cancel's park: a bin standing on
// a single-robot claim's outbound staging, read in the request's own node-bins
// call, goes on to the outbound destination by the same plain move.
//
// A PARK CAN BE SET DOWN AFTER THE CANCEL'S READ. The cancel queues its abort to
// Core and reads node-bins at once; a robot still carrying the line's bin when
// that read answers sets it down on outbound staging afterwards, and the cancel
// has already decided there was nothing to move. Every later single-robot swap
// at the line parks there, so it would wait behind that bin for good.
//
// Called after the request's guards, so no live swap holds the line's slots. It
// still leaves the bin when any live complex order of the line remains (a leg
// that has left the line but will still collect its parked bin) or a live move
// already takes it off (the cancel's own, or an earlier request's). The line's
// rows are read only when Core reports a bin there. The move names no demand:
// it is the cell's own clean-up, not what the request asked for.
func (e *Engine) clearStrandedPark(node *processes.Node, claim *processes.NodeClaim, park NodeBinInfo) {
	if !park.Occupied || !parkAsked(claim) || claim.OutboundDestination == "" {
		return
	}
	rows, err := e.db.ListActiveOrdersByProcessNode(node.ID)
	if err != nil {
		e.logFn("request: %s: cannot read the line's orders (%v) — the bin on %s is left for the next request",
			node.Name, err, claim.OutboundStaging)
		return
	}
	for _, o := range rows {
		if o.OrderType == ordermgr.TypeComplex || (o.OrderType == ordermgr.TypeMove && o.SourceNode == claim.OutboundStaging) {
			return
		}
	}
	nodeID := node.ID
	if _, err := e.orderMgr.CreateMoveOrderCarrying(&nodeID, claim.OutboundStaging, claim.OutboundDestination,
		park.PayloadCode, ordermgr.NoDemand()); err != nil {
		e.logFn("request: %s: send the bin left on %s on to %s: %v", node.Name, claim.OutboundStaging,
			claim.OutboundDestination, err)
		return
	}
	e.logFn("request: %s: the bin left on %s goes on to %s", node.Name, claim.OutboundStaging, claim.OutboundDestination)
}
