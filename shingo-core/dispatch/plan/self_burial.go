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
// and replay step resolution (ComplexSteps, ReComplexSteps), as the trigger for
// unwallOwnPickups's re-ask. The remedy is the one chooser (ChooseDrop, C2c
// ruling 1) asked with the lanes of every later pickup set aside; a group with
// nowhere else waits under the existing no-slot cause. The gate's release asks
// ChooseDrop with the same set from the start, so it needs no trigger.
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
// front of a pickup this plan makes later in the same lane is re-asked of the
// one chooser with the lanes of EVERY later pickup set aside (ChooseDrop's hard
// rule S), in place.
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
// ── EVERY LATER PICKUP, NOT THE ONE IT WALLED (C2c ruling 1) ─────────────
//
// The re-ask used to set aside only the lane it walled, so the answer could
// stand in front of a SECOND later pickup in another lane of the group
// (DESIGN-C2c §0 finding 6, latent: no fixture made a plan with two). The rule
// is now asked before the algorithm, of all of them at once.
//
// Reports whether any step moved; a group with nowhere else is the resolver's
// capacity error.
func (p *Planner) unwallOwnPickups(steps []Step, drops []groupDrop, asker domain.DigAsker) (bool, error) {
	moved := false
	for _, gd := range drops {
		later := LaterPickups(steps, gd.i)
		lane := p.OwnPickupLane(steps[gd.i].Node, later)
		if lane == 0 {
			continue
		}
		g, err := p.db.GetNodeByDotName(gd.group)
		if err != nil || g == nil {
			return moved, fmt.Errorf("step %d: cannot resolve group %s: %v", gd.i, gd.group, err)
		}
		choice, err := p.ChooseDrop(g, DropNeed{Payload: gd.payload, Stated: gd.stated, Asker: asker, LaterPickups: later})
		if err != nil {
			return moved, fmt.Errorf("step %d: cannot resolve group %s: %w", gd.i, gd.group, err)
		}
		p.debug("plan: a drop into %s would wall this plan's own later pickup in lane %d — re-asked without the "+
			"lanes of its later pickups: %s", gd.group, lane, choice.Slot.Name)
		if choice.Slot.Name != steps[gd.i].Node {
			steps[gd.i].Node = choice.Slot.Name
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
