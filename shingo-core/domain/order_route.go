package domain

import (
	"encoding/json"
	"strings"

	"shingo/protocol"
)

// RouteState is where a stop stands in the robot's progress along the route.
type RouteState string

const (
	// RoutePassed: the robot has worked the stop and gone on.
	RoutePassed RouteState = "passed"
	// RouteLeft: the stop the robot last worked, done with and left for the
	// next one: it is between the two.
	RouteLeft RouteState = "left"
	// RouteHere: the robot is at the stop: parked there, its next work there,
	// or its plan done there.
	RouteHere RouteState = "here"
	// RouteNext: where the robot goes next.
	RouteNext RouteState = "next"
	// RouteRemaining: still to come after the next.
	RouteRemaining RouteState = "remaining"
	// RouteFinal: where the cart finally lands, while it is neither here nor
	// next.
	RouteFinal RouteState = "final"
)

// RouteStop is one stop of an order's route as a person reads it: a node, or
// an end not chosen yet (Unassigned), and where it stands in the robot's
// progress.
type RouteStop struct {
	Node       string     `json:"node,omitempty"`
	Unassigned bool       `json:"unassigned,omitempty"`
	State      RouteState `json:"state"`
}

// routeStep is the part of a stored plan step the route reads. Read here rather
// than through dispatch/plan, which domain does not import.
type routeStep struct {
	Action string `json:"action"`
	Node   string `json:"node,omitempty"`
	Group  string `json:"group,omitempty"`
}

// routePlan is an order's route with the plan behind it: the stops, the steps
// the robot works them by, and for each step the stop it works at, -1 for a
// wait (or a step that names nothing).
type routePlan struct {
	stops  []RouteStop
	steps  []routeStep
	stopOf []int
}

// RouteStops is THE route of an order, for every surface that draws one: the
// orders board and the Dashboard card (the order-route partial), the order
// pop-up (/api/orders/enriched) and the task board (BoardOrder). One function,
// so the three cannot spell a route three ways
// (TestOrderRouteRuleMatchesThePopup). RouteViewOf lays it out as the strip.
//
// AN ORDER WITH STEPS reads its steps in order: every pickup and dropoff names
// its node (the group, while the node is not resolved yet), waits are dropped,
// and a stop that repeats the one before it is one stop. A press-index R1
// (wait PLN_012, pickup PLN_012, dropoff ULN_002, pickup SMN_BUF_100, dropoff
// PLN_008) reads PLN_012 → ULN_002 → SMN_BUF_100 → PLN_008.
//
// AN ORDER WITH NO STEPS, or steps that do not parse or name no node (an old
// row), reads From → Line → To: source_node, then process_node unless it
// repeats an end, then delivery_node, an end not chosen yet Unassigned. Its
// plan is the two steps it implies, the lift at From and the drop at To.
//
// WHERE THE ROBOT IS comes from the row: steps_done, the plan's steps the fleet
// has finished (the block subscriber's count), with the waits the wait_index
// has released behind it and, on a staged order, the wait it is parked at. It
// is by the stop of the last step done: here while it is parked there, its
// next work is there or its plan is done, and left once it has gone on to the
// next. At a wait at a node it is here at the stop of that node whose work is
// next, and else between its last stop (left) and the next. The next stop is
// the first with work still to do after it. A delivered order has done every
// step. An order that ended any other way has no robot here, left or next:
// what it worked is passed, the rest to come.
func RouteStops(o *Order) []RouteStop {
	if o == nil {
		return nil
	}
	p := planOf(o)
	by, how, next := p.place(o)
	p.mark(o, by, how, next)
	return p.stops
}

// StepsDoneAfter is what steps_done becomes when a block finishes at location
// doing action (a plan action; "" for a block of no step's kind, such as the
// turns of an advanced load sequence): the plan's next open step is done when
// it is that action at that node, or that action at a node not resolved yet.
// A bare wait sends no block, so the next open step is the first after
// steps_done that is not one. ok=false: the block finished no step, and
// steps_done stands — a turn, a repeat, a block out of its place.
func StepsDoneAfter(o *Order, location, action string) (int, bool) {
	if o == nil || action == "" {
		return 0, false
	}
	p := planOf(o)
	i := max(o.StepsDone, 0)
	for i < len(p.steps) && p.steps[i].Action == protocol.ActionWait && p.steps[i].Node == "" {
		i++
	}
	if i >= len(p.steps) {
		return o.StepsDone, false
	}
	s := p.steps[i]
	if s.Action != action || (s.Node != "" && strings.TrimSpace(s.Node) != strings.TrimSpace(location)) {
		return o.StepsDone, false
	}
	return i + 1, true
}

func planOf(o *Order) routePlan {
	if p, ok := stepPlan(o.StepsJSON); ok {
		return p
	}
	return endpointPlan(o)
}

// stepPlan reads the stored steps into a plan; ok=false when they name no
// stop to read.
func stepPlan(stepsJSON string) (routePlan, bool) {
	if stepsJSON == "" {
		return routePlan{}, false
	}
	var steps []routeStep
	if json.Unmarshal([]byte(stepsJSON), &steps) != nil {
		return routePlan{}, false
	}
	p := routePlan{steps: steps, stopOf: make([]int, len(steps))}
	for i, s := range steps {
		p.stopOf[i] = -1
		node := s.Node
		if node == "" {
			node = s.Group
		}
		if s.Action == protocol.ActionWait || node == "" {
			continue
		}
		if n := len(p.stops); n > 0 && p.stops[n-1].Node == node {
			p.stopOf[i] = n - 1
			continue
		}
		p.stops = append(p.stops, RouteStop{Node: node})
		p.stopOf[i] = len(p.stops) - 1
	}
	return p, len(p.stops) > 0
}

// endpointPlan is the route of an order with no steps to read: From → Line →
// To, the line omitted when it repeats an end, worked by the lift at From and
// the drop at To.
func endpointPlan(o *Order) routePlan {
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
	stops = append(stops, end(o.DeliveryNode))
	return routePlan{
		stops:  stops,
		steps:  []routeStep{{Action: protocol.ActionPickup, Node: o.SourceNode}, {Action: protocol.ActionDropoff, Node: o.DeliveryNode}},
		stopOf: []int{0, len(stops) - 1},
	}
}

// progress is how many of the plan's steps are behind the robot: every one for
// a delivered order, else steps_done and the waits after it that the
// wait_index has released (a bare wait sends no block, so its release is the
// only sign of it) or, on a staged order, the wait at a node it is parked at.
func (p routePlan) progress(o *Order) int {
	if o.Status == protocol.StatusDelivered || o.Status == protocol.StatusConfirmed {
		return len(p.steps)
	}
	k := min(max(o.StepsDone, 0), len(p.steps))
	waits := 0
	for _, s := range p.steps[:k] {
		if s.Action == protocol.ActionWait {
			waits++
		}
	}
	for ; k < len(p.steps) && p.steps[k].Action == protocol.ActionWait; k, waits = k+1, waits+1 {
		released := waits < o.WaitIndex
		parked := o.Status == protocol.StatusStaged && waits == o.WaitIndex && p.steps[k].Node != ""
		if !released && !parked {
			break
		}
	}
	return k
}

// place is the stop the robot is by, whether it is here or has left it, and
// the stop it goes to next, -1 for none, as RouteStops describes.
func (p routePlan) place(o *Order) (by int, how RouteState, next int) {
	k := p.progress(o)
	open := len(p.stops)
	for i := k; i < len(p.steps); i++ {
		if p.stopOf[i] >= 0 {
			open = p.stopOf[i]
			break
		}
	}
	by, standing := p.position(o, k, open)
	next = open
	if by >= 0 && by+1 > next {
		next = by + 1
	}
	if next >= len(p.stops) {
		next = -1
	}
	how = RouteHere
	if by >= 0 && !standing && next >= 0 {
		how = RouteLeft
	}
	return by, how, next
}

// position is the stop the robot was last by with k steps behind it, -1 for
// none, and whether it stands at it. open is the stop of its next work.
func (p routePlan) position(o *Order, k, open int) (int, bool) {
	if k == 0 {
		return -1, false
	}
	last := -1 // the stop of the last step done that works one
	for j := k - 1; j >= 0 && last < 0; j-- {
		last = p.stopOf[j]
	}
	if p.stopOf[k-1] >= 0 {
		// Parked here, its next work here, or its plan done here.
		return last, o.Status == protocol.StatusStaged || open == last || k == len(p.steps)
	}
	if node := p.steps[k-1].Node; node != "" {
		// At a wait at a node: at the stop there if its work is next, else
		// between the stop it last worked and the next.
		if open < len(p.stops) && p.stops[open].Node == node {
			return open, true
		}
		return last, false
	}
	// Released from a bare wait: still at its last stop only if its next
	// work is there too.
	return last, last >= 0 && open == last
}

// mark writes each stop's state from the stop the robot is by, how, and the
// stop it goes to next.
func (p routePlan) mark(o *Order, by int, how RouteState, next int) {
	// Ended any way but confirmed. A row with no status yet has not ended.
	over := o.Status != "" && protocol.IsTerminal(o.Status) && o.Status != protocol.StatusConfirmed
	front := len(p.stops) // the first stop not behind the robot
	if next >= 0 {
		front = next
	}
	if by >= 0 && !over {
		front = by
	}
	last := len(p.stops) - 1
	for i := range p.stops {
		switch {
		case !over && i == by:
			p.stops[i].State = how
		case !over && i == next:
			p.stops[i].State = RouteNext
		case i < front:
			p.stops[i].State = RoutePassed
		case i == last:
			p.stops[i].State = RouteFinal
		default:
			p.stops[i].State = RouteRemaining
		}
	}
}

// RouteView is the route as the strip on the boards draws it: four named beats
// at most (owner, 2026-10-26) — the origin at the left, the stop the robot is
// at (Here), the stop it goes to next (Next) and the final stop at the right —
// and one count, Remaining, for the stops between the next and the final.
// Passed stops are never drawn: the origin stands for them. Nor, in transit,
// is the stop just left: the accent arrow before the stop the robot heads to
// (Heading) says it is between. Here and Next are nil where that stop is the
// origin or the final, which carry the state themselves, or where there is
// none; Final is nil on a route of one stop. Every stop, passed and left
// included, stays in RouteStops for the strip's title and the pop-up timeline.
type RouteView struct {
	Origin    RouteStop  `json:"origin"`
	Here      *RouteStop `json:"here,omitempty"`
	Next      *RouteStop `json:"next,omitempty"`
	Remaining int        `json:"remaining"`
	Final     *RouteStop `json:"final,omitempty"`
	Heading   RouteBeat  `json:"heading,omitempty"`
}

// RouteBeat is a beat of the strip the robot can be on its way to.
type RouteBeat string

const (
	BeatOrigin RouteBeat = "origin"
	BeatNext   RouteBeat = "next"
	BeatFinal  RouteBeat = "final"
)

// RouteViewOf lays out stops, as RouteStops marks them for an order of
// status, as the strip. It reads their states and positions; the status says
// only whether a robot is on its way to the origin before it has worked a
// stop (routeHeading).
func RouteViewOf(stops []RouteStop, status protocol.Status) RouteView {
	var v RouteView
	if len(stops) == 0 {
		return v
	}
	v.Origin = stops[0]
	if len(stops) > 1 {
		final := stops[len(stops)-1]
		v.Final = &final
		for _, s := range stops[1 : len(stops)-1] {
			switch s.State {
			case RouteHere:
				v.Here = &s
			case RouteNext:
				v.Next = &s
			case RouteRemaining:
				v.Remaining++
			}
		}
	}
	v.Heading = routeHeading(stops, status)
	return v
}

// routeHeading is the beat the robot is on its way to, "" when none is: the
// next stop once it has left one, and the origin while a dispatched,
// acknowledged or in-transit order has worked none yet.
func routeHeading(stops []RouteStop, status protocol.Status) RouteBeat {
	left, next := -1, -1
	for i, s := range stops {
		switch s.State {
		case RouteLeft:
			left = i
		case RouteNext:
			next = i
		}
	}
	started := status == protocol.StatusDispatched || status == protocol.StatusAcknowledged ||
		status == protocol.StatusInTransit
	switch {
	case next < 0, left < 0 && !(next == 0 && started):
		return ""
	case next == 0:
		return BeatOrigin
	case next == len(stops)-1:
		return BeatFinal
	}
	return BeatNext
}
