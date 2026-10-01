package dispatch

import (
	"encoding/json"

	"shingo/protocol"
	"shingocore/store/orders"
)

// release_points.go — the dynamic half of a release point (SHAPE §3.7): what a
// release of an order would let its robot do next, read fresh from Core for
// each act. The Edge cannot see these facts (seven writers re-point a drop
// after staging; the lift that empties a drop node is another leg's), so it
// asks rather than infers.

// ReleasePoints answers protocol.ReleasePointsRequest for one station. An
// order Core does not hold for that station comes back Found=false.
func (d *Dispatcher) ReleasePoints(stationID string, orderUUIDs []string) []protocol.ReleasePoint {
	out := make([]protocol.ReleasePoint, 0, len(orderUUIDs))
	for _, uuid := range orderUUIDs {
		pt := protocol.ReleasePoint{OrderUUID: uuid, Enters: []string{}, AwaitsLift: []protocol.LiftDependency{}}
		o, err := d.db.GetOrderByUUID(uuid)
		if err != nil || o == nil || o.StationID != stationID {
			out = append(out, pt)
			continue
		}
		pt.Found = true
		seg, ok := d.pendingSegment(o)
		if !ok {
			out = append(out, pt)
			continue
		}
		for _, s := range seg {
			if (s.Action == protocol.ActionPickup || s.Action == protocol.ActionDropoff) && s.Node != "" {
				pt.Enters = append(pt.Enters, s.Node)
			}
		}
		for _, dep := range d.liftDependencies(o, seg) {
			pt.AwaitsLift = append(pt.AwaitsLift, protocol.LiftDependency{
				LifterUUID: dep.lifter.EdgeUUID, Node: dep.node, CoRelease: dep.coRelease,
			})
		}
		pt.LinesideBin = d.departingLinesideBin(o, seg)
		out = append(out, pt)
	}
	return out
}

// liftDep is one dependency (SHAPE §3.4): this order's next segment sets a
// bin down on node, which holds a bin now, and lifter — its Core sibling —
// lifts from node before any drop of its own there.
type liftDep struct {
	lifter    *orders.Order
	node      string
	coRelease bool
}

// liftDependencies is SHAPE §3.4's rule over seg, the segment a release of p
// would append. A dependency already satisfied by (a) — the node no longer
// holds a bin — is simply absent. coRelease is (b): the lifter is staged at
// the wait whose next segment contains the lift at the node, and p's segment
// picks something up before it sets a bin down there (p is not carrying the
// bin it will place). A leg never depends on itself.
//
// U5 (vacateReleaseRefusal) stays the stamp-scoped backstop it is by ruling;
// both read the segment as it will be sent (pendingSegment) and the same
// sibling link.
func (d *Dispatcher) liftDependencies(p *orders.Order, seg []resolvedStep) []liftDep {
	if p.SiblingOrderUUID == "" {
		return nil
	}
	lifter, err := d.db.GetOrderByUUID(p.SiblingOrderUUID)
	if err != nil || lifter == nil || lifter.ID == p.ID {
		return nil
	}
	var lifterSteps []resolvedStep
	if json.Unmarshal([]byte(lifter.StepsJSON), &lifterSteps) != nil {
		return nil
	}
	remaining := stepsAfterWait(lifterSteps, lifter.WaitIndex-1)
	var lifterSeg []resolvedStep
	if lifter.Status == StatusStaged {
		lifterSeg, _ = d.pendingSegment(lifter)
	}
	var deps []liftDep
	seen := map[string]bool{}
	for i, s := range seg {
		if s.Action != protocol.ActionDropoff || s.Node == "" || seen[s.Node] {
			continue
		}
		seen[s.Node] = true
		if !liftsBeforeDropping(remaining, s.Node) || !d.nodeHoldsABin(s.Node) {
			continue
		}
		deps = append(deps, liftDep{
			lifter: lifter,
			node:   s.Node,
			coRelease: lifter.Status == StatusStaged && segmentLifts(lifterSeg, s.Node) &&
				picksUpBefore(seg, i),
		})
	}
	return deps
}

// stepsAfterWait is the steps after the waitIdx-th wait (0-based), or all of
// them for a negative index: an order's remaining work once wait waitIdx is
// behind it.
func stepsAfterWait(steps []resolvedStep, waitIdx int) []resolvedStep {
	if waitIdx < 0 {
		return steps
	}
	seen := 0
	for i, s := range steps {
		if s.Action == protocol.ActionWait {
			if seen == waitIdx {
				return steps[i+1:]
			}
			seen++
		}
	}
	return nil
}

// liftsBeforeDropping reports whether the first bin action at node in steps is
// a pickup.
func liftsBeforeDropping(steps []resolvedStep, node string) bool {
	for _, s := range steps {
		if s.Node != node {
			continue
		}
		switch s.Action {
		case protocol.ActionPickup:
			return true
		case protocol.ActionDropoff:
			return false
		}
	}
	return false
}

// segmentLifts reports whether seg picks a bin up at node.
func segmentLifts(seg []resolvedStep, node string) bool {
	for _, s := range seg {
		if s.Action == protocol.ActionPickup && s.Node == node {
			return true
		}
	}
	return false
}

// picksUpBefore reports whether seg picks something up before step i.
func picksUpBefore(seg []resolvedStep, i int) bool {
	for _, s := range seg[:i] {
		if s.Action == protocol.ActionPickup {
			return true
		}
	}
	return false
}

// nodeHoldsABin reports whether a bin stands on the named node. An unreadable
// answer counts as holding one: a dependency reported that turns out absent
// costs a held release, and one missed costs two bins on a node.
func (d *Dispatcher) nodeHoldsABin(name string) bool {
	n, err := d.db.GetNodeByDotName(name)
	if err != nil || n == nil {
		return true
	}
	onNode, err := d.db.ListBinsByNode(n.ID)
	if err != nil {
		return true
	}
	return len(onNode) > 0
}

// departingLinesideBin is the bin a release would lift off the order's line
// node, when its segment picks up there: the departing leg's lineside bin,
// which the Edge used to fetch separately (BinAtLineside).
func (d *Dispatcher) departingLinesideBin(o *orders.Order, seg []resolvedStep) *protocol.NodeBinInfo {
	if o.ProcessNode == "" || !segmentLifts(seg, o.ProcessNode) {
		return nil
	}
	n, err := d.db.GetNodeByDotName(o.ProcessNode)
	if err != nil || n == nil {
		return nil
	}
	onNode, err := d.db.ListBinsByNode(n.ID)
	if err != nil || len(onNode) == 0 {
		return &protocol.NodeBinInfo{NodeName: o.ProcessNode}
	}
	b := onNode[0]
	return &protocol.NodeBinInfo{
		NodeName: o.ProcessNode, BinID: b.ID, BinLabel: b.Label, BinTypeCode: b.BinTypeCode, Bare: b.BinTypeBare,
		PayloadCode: b.PayloadCode, UOPRemaining: b.UOPRemaining, DeltaEpoch: b.DeltaEpoch,
		Manifest: b.Manifest, ManifestConfirmed: b.ManifestConfirmed, Occupied: true,
	}
}
