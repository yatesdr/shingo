package plan

import (
	"fmt"

	"shingo/protocol"

	"shingocore/dispatch/binresolver"
	"shingocore/domain"
	"shingocore/store/nodes"
)

// OwnPickupLane is the lane in which a drop at dropNode would stand in front of
// a pickup this same plan makes LATER in that lane, or 0 (C2b Q1, the plan's
// fact: P0 ruling Q1 (b)). laterPickups are the nodes of the plan's pickups
// after the drop, as they are named now; a group or an unknown node is not a
// slot and is skipped.
//
// THE SELECTOR STAYS PLAN-BLIND, ON PURPOSE: its burial clause exempts the
// asker so a re-bind may aim behind a bin the order itself is coming for
// (store/nodes/lanes.go). It cannot see plan order, so nothing there stops a
// drop at step i walling the order's own pickup at step j > i (swap 270's
// shape, 2-3 a run on base). The plan can, and this is where it asks: intake
// and replay step resolution (ComplexSteps, ReComplexSteps) and the gate's
// re-bind (dispatch rebindGatedDropoff). The remedy is the existing widen: the
// drop re-asks the group with that lane set aside, and a group with nowhere else
// waits under the existing no-slot cause.
//
// Same lane means the same LANE node; the geometry is helpers.ShallowerInSameLane's
// (same parent, strictly shallower depth). Tiers directly under a group are not
// a lane the store resolve can set aside, and are not asked.
func (p *Planner) OwnPickupLane(dropNode string, laterPickups []string) int64 {
	drop, err := p.db.GetNodeByDotName(dropNode)
	if err != nil || drop == nil || drop.ParentID == nil || drop.Depth == nil {
		return 0
	}
	for _, name := range laterPickups {
		pk, err := p.db.GetNodeByDotName(name)
		if err != nil || pk == nil || pk.ParentID == nil || pk.Depth == nil || *pk.ParentID != *drop.ParentID {
			continue
		}
		if *pk.Depth <= *drop.Depth {
			continue
		}
		lane, err := p.db.GetNode(*drop.ParentID)
		if err == nil && lane != nil && lane.NodeTypeCode == protocol.NodeClassLANE {
			return lane.ID
		}
		return 0
	}
	return 0
}

// LaterPickups is the nodes of the pickups after step i of a resolved plan.
func LaterPickups(steps []Step, i int) []string {
	var out []string
	for j := i + 1; j < len(steps); j++ {
		if steps[j].Action == protocol.ActionPickup && steps[j].Node != "" {
			out = append(out, steps[j].Node)
		}
	}
	return out
}

// laterRequestPickups is LaterPickups over the request's steps, at intake.
func laterRequestPickups(steps []protocol.ComplexOrderStep, i int) []string {
	var out []string
	for j := i + 1; j < len(steps); j++ {
		if steps[j].Action == protocol.ActionPickup && steps[j].Node != "" {
			out = append(out, steps[j].Node)
		}
	}
	return out
}

// laneExcludingResolver is the resolver's store arm with lanes set aside
// (binresolver.DefaultResolver.ResolveStoreExcludingLanes).
type laneExcludingResolver interface {
	ResolveStoreExcludingLanes(group *nodes.Node, payloadCode string, stated binresolver.BinTypeStatement,
		asker domain.DigAsker, exclude map[int64]bool) (*binresolver.ResolveResult, error)
}

// storeAvoidingOwnPickup is a group drop's resolution re-asked with the lane it
// would wall its own later pickup in set aside. It returns the slot, or the
// resolver's capacity error: the group has nowhere else, and the drop waits
// under the cause a full group already has.
func (p *Planner) storeAvoidingOwnPickup(group string, lane int64, payloadCode string, stated binresolver.BinTypeStatement,
	asker domain.DigAsker) (string, error) {
	r, ok := p.resolver.(laneExcludingResolver)
	if !ok {
		return "", fmt.Errorf("cannot resolve group %s without lane %d: the resolver cannot set a lane aside", group, lane)
	}
	g, err := p.db.GetNodeByDotName(group)
	if err != nil || g == nil {
		return "", fmt.Errorf("cannot resolve group %s: %v", group, err)
	}
	res, err := r.ResolveStoreExcludingLanes(g, payloadCode, stated, asker, map[int64]bool{lane: true})
	if err != nil {
		return "", fmt.Errorf("cannot resolve group %s: %w", group, err)
	}
	p.debug("plan: a drop into %s would wall this plan's own later pickup in lane %d — re-asked without it: %s",
		group, lane, res.Node.Name)
	return res.Node.Name, nil
}
