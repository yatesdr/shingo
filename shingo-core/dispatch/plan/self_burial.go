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

// groupDrop is a group drop a resolution walk chose, held for the Q1 check until
// every step of the plan has its slot (unwallOwnPickups). i indexes the resolved
// plan; payload and stated are what the drop was resolved with, for the re-ask.
type groupDrop struct {
	i       int
	group   string
	payload string
	stated  binresolver.BinTypeStatement
}

// unwallOwnPickups is Q1 on the FULLY RESOLVED plan: a group drop that stands in
// front of a pickup this plan makes later in the same lane is re-asked with that
// lane set aside (storeAvoidingOwnPickup), in place.
//
// ── WHY AFTER THE WALK AND NOT IN IT ──────────────────────────────────────
//
// The walk resolves steps left to right, so when a swap's drop of the press's full
// is resolved, the empty it fetches next may still name its GROUP, and a group is
// not a slot OwnPickupLane can place. The empty then resolved behind the drop, in
// the same lane, and nothing asked again: order 266 (packed, b6880b5d) left with
// its drop at LSD_006 in front of its own empty at LSD_009, the gate refused it
// at the mark, and it stood there for the rest of the run. Asked here, every
// later pickup is a slot. The reads are the walk's own, moved: one per drop and
// one per later pickup, plus the lane's when a wall is found.
//
// The re-asked drop is checked against the one lane it walled. Reports whether
// any step moved; a group with nowhere else is the resolver's capacity error.
func (p *Planner) unwallOwnPickups(steps []Step, drops []groupDrop, asker domain.DigAsker) (bool, error) {
	moved := false
	for _, gd := range drops {
		lane := p.OwnPickupLane(steps[gd.i].Node, LaterPickups(steps, gd.i))
		if lane == 0 {
			continue
		}
		name, err := p.storeAvoidingOwnPickup(gd.group, lane, gd.payload, gd.stated, asker)
		if err != nil {
			return moved, fmt.Errorf("step %d: %w", gd.i, err)
		}
		if name != steps[gd.i].Node {
			steps[gd.i].Node = name
			moved = true
		}
	}
	return moved, nil
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
