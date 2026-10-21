package domain

import (
	"encoding/json"

	"shingo/protocol"
)

// RouteStop is one stop of an order's route as a person reads it: a node, or
// an end not chosen yet (Unassigned), and whether the robot is there now.
type RouteStop struct {
	Node       string `json:"node,omitempty"`
	Unassigned bool   `json:"unassigned,omitempty"`
	Current    bool   `json:"current,omitempty"`
}

// routeStep is the part of a stored plan step the route reads. Read here rather
// than through dispatch/plan, which domain does not import.
type routeStep struct {
	Action string `json:"action"`
	Node   string `json:"node,omitempty"`
	Group  string `json:"group,omitempty"`
}

// routeStopAt is a stop with the wait segments it spans: the number of station
// or lane waits before its first and its last step.
type routeStopAt struct {
	RouteStop
	firstSeg, lastSeg int
}

// RouteStops is THE route of an order, for every surface that draws one: the
// orders board and the Dashboard card (the order-route partial), the order
// pop-up (/api/orders/enriched) and the task board (BoardOrder). One function,
// so the three cannot spell a route three ways
// (TestOrderRouteRuleMatchesThePopup).
//
// AN ORDER WITH STEPS reads its steps in order: every pickup and dropoff names
// its node (the group, while the node is not resolved yet), waits are dropped,
// and a stop that repeats the one before it is one stop. A press-index R1
// (wait PLN_012, pickup PLN_012, dropoff ULN_002, pickup SMN_BUF_100, dropoff
// PLN_008) reads PLN_012 → ULN_002 → SMN_BUF_100 → PLN_008: the outbound and
// the supermarket its first and last stops alone hid.
//
// AN ORDER WITH NO STEPS, or steps that do not parse or name no node (an old
// row), reads From → Line → To: source_node, then process_node unless it
// repeats an end, then delivery_node, an end not chosen yet Unassigned.
//
// THE CURRENT STOP. A staged order is parked at the wait it has reached
// (WaitIndex counts the waits released, so that wait is the WaitIndex-th): the
// stop at that wait's node. A dispatched or moving order is somewhere in the
// segment after its last released wait: the first stop of that segment at the
// robot's station (currentStation, the fleet's last report; "" when unknown).
// Anything else, queued, delivered or ended, marks no stop. A route with no
// steps marks none either: it has no segments to place the robot in.
func RouteStops(o *Order, currentStation string) []RouteStop {
	if o == nil {
		return nil
	}
	stops := stepStops(o.StepsJSON)
	if len(stops) == 0 {
		return endpointStops(o)
	}
	markCurrent(stops, o, currentStation)
	out := make([]RouteStop, len(stops))
	for i, s := range stops {
		out[i] = s.RouteStop
	}
	return out
}

// stepStops reads the stored steps into stops; nil when there are none to read.
func stepStops(stepsJSON string) []routeStopAt {
	if stepsJSON == "" {
		return nil
	}
	var steps []routeStep
	if json.Unmarshal([]byte(stepsJSON), &steps) != nil {
		return nil
	}
	var stops []routeStopAt
	seg := 0
	for _, s := range steps {
		if s.Action == protocol.ActionWait {
			seg++
			continue
		}
		node := s.Node
		if node == "" {
			node = s.Group
		}
		if node == "" {
			continue
		}
		if n := len(stops); n > 0 && stops[n-1].Node == node {
			stops[n-1].lastSeg = seg
			continue
		}
		stops = append(stops, routeStopAt{RouteStop: RouteStop{Node: node}, firstSeg: seg, lastSeg: seg})
	}
	return stops
}

// markCurrent marks the stop the robot is at, as RouteStops describes.
func markCurrent(stops []routeStopAt, o *Order, currentStation string) {
	switch o.Status {
	case protocol.StatusStaged:
		node, ok := waitNode(o.StepsJSON, o.WaitIndex)
		if !ok || node == "" {
			return
		}
		// The wait sits between segment WaitIndex and the next: the stop at its
		// node that spans that boundary (the lift after it, or the drop before).
		for i := range stops {
			if stops[i].Node == node && stops[i].firstSeg <= o.WaitIndex+1 && stops[i].lastSeg >= o.WaitIndex {
				stops[i].Current = true
				return
			}
		}
	case protocol.StatusDispatched, protocol.StatusAcknowledged, protocol.StatusInTransit:
		if currentStation == "" {
			return
		}
		for i := range stops {
			if stops[i].Node == currentStation && stops[i].firstSeg <= o.WaitIndex && stops[i].lastSeg >= o.WaitIndex {
				stops[i].Current = true
				return
			}
		}
	}
}

// waitNode is the node of the waitIndex-th wait step, as dispatch's waitAt
// counts them.
func waitNode(stepsJSON string, waitIndex int) (string, bool) {
	var steps []routeStep
	if json.Unmarshal([]byte(stepsJSON), &steps) != nil {
		return "", false
	}
	seen := 0
	for _, s := range steps {
		if s.Action != protocol.ActionWait {
			continue
		}
		if seen == waitIndex {
			return s.Node, true
		}
		seen++
	}
	return "", false
}

// endpointStops is the route of an order with no steps to read: From → Line →
// To, the line omitted when it repeats an end.
func endpointStops(o *Order) []RouteStop {
	end := func(node string) RouteStop {
		if node == "" {
			return RouteStop{Unassigned: true}
		}
		return RouteStop{Node: node}
	}
	stops := []RouteStop{end(o.SourceNode)}
	if o.ProcessNode != "" && o.ProcessNode != o.SourceNode && o.ProcessNode != o.DeliveryNode {
		stops = append(stops, RouteStop{Node: o.ProcessNode})
	}
	return append(stops, end(o.DeliveryNode))
}
