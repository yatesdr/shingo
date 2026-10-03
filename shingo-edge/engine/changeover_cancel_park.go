package engine

import (
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

// parkedBin is one aborted leg's park: the bin it lifted off the line and set
// down on park, bound for dest later in the same leg.
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

// parkOf is parkedTrip for an aborted changeover order: one steps read, and
// one node read when the steps park something.
func (e *Engine) parkOf(order *domain.Order, processNodeID int64) (parkedBin, bool) {
	if order.OrderType != ordermgr.TypeComplex {
		return parkedBin{}, false
	}
	raw, err := e.db.GetOrderStepsJSON(order.ID)
	if err != nil {
		return parkedBin{}, false
	}
	steps, err := decodeSteps(raw)
	if err != nil {
		return parkedBin{}, false
	}
	node, err := e.db.GetProcessNode(processNodeID)
	if err != nil || node == nil {
		return parkedBin{}, false
	}
	park, dest, ok := parkedTrip(steps, node.CoreNodeName)
	if !ok {
		return parkedBin{}, false
	}
	return parkedBin{nodeID: node.ID, line: node.CoreNodeName, park: park, dest: dest}, true
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
// carrying that bin's own payload to where the aborted leg was taking it.
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
