package plan

import (
	"fmt"

	"shingocore/dispatch/binresolver"
	"shingocore/domain"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// choose_drop.go — C2c (Lane C): the ONE spelling of "choose a drop in this
// group" (C2c ruling 1; DESIGN-C2c §7.1). The hard rules first, then the
// group's configured store algorithm (binresolver: LKND, DPTH), no scoring.
// Called at plan time (intake, asker Anyone), at acquiring (the replay and
// asIntent's re-made choices) and at release (the group gate pass, the valve,
// the own-dig resume), so the three moments cannot drift.
//
// The hard rules and where each is asked:
//
//	enabled, payload and carrier allowed, empties-only, the declared level,
//	reachable, dig-free, the burial guard   the resolver's own fences
//	                                        (binresolver.ResolveStore), and the
//	                                        same ones of a slot offered as paper
//	                                        (StoreFitsPaper, PaperSlotsInGroup)
//	never bury the order's own later pickup  the lanes of EVERY later pickup set
//	                                        aside before the algorithm runs
//	                                        (DESIGN-C2c §0 finding 6: the old
//	                                        post-pass re-checked only one lane)
//	a refused mouth is the next candidate   Refused, set aside the same way
//	                                        (ruling 3)
//
// It reads and never writes (planner purity): a slot offered as a stranger's
// paper comes back with the take named, and the caller's store door takes it
// under lock (store.DB.ConfirmReleaseDrop, TakeGroupRoomByRank).

// ChooseDropDB is what the chooser reads beyond the resolver's store: the paper
// on a group's slots, and the demand an asker presents at a rank comparison.
type ChooseDropDB interface {
	PaperSlotsInGroup(groupID, owner int64) ([]nodes.PaperSlot, error)
	LoadDemandRank(orderID int64) (orders.DemandRank, error)
}

// DropNeed is what a drop asks the chooser with (DESIGN-C2c §7.1's `need`).
type DropNeed struct {
	// Payload is the drop's own payload (DropPayload): none for a carried empty,
	// which an empty may be stored wherever an empty may stand (C2b Q4).
	Payload string
	// Stated is the carrier the drop puts down: the carried bin's type at release,
	// BinTypeBeforeStep at acquiring.
	Stated binresolver.BinTypeStatement
	Asker  domain.DigAsker
	// LaterPickups are the nodes of the plan's pickups after this drop. The lane
	// each stands in is set aside before the algorithm runs: a drop never stands
	// in front of a pickup its own plan makes later (hard rule S, C2b Q1).
	LaterPickups []string
	// Refused are the lanes whose admission refused this release (ruling 3: a
	// refused mouth is the next candidate, not a wait, while the group has one).
	Refused map[int64]bool
	// Lane narrows the ask to one lane: a drop with no group stamp (D13 Q-NAMED:
	// a person-named slot stays lane-scoped) or a relay (ruling 6: never
	// re-aimed). The same chooser, asked a narrower question; nil asks the group.
	Lane *nodes.Node
	// Committed says the asker is a robot standing at the group's waiting spot:
	// dispatched and gate-staged (D13 F5). It may take an unsent plan's paper
	// (ruling 2, "yields to a committed robot").
	Committed bool
	// Ranked asks for the group's whole order, not only its first answer: the
	// release walks it when a lane refuses (ruling 3) without reading the group
	// again (DESIGN-C2c §7.3: the group's slot state read once per release).
	Ranked bool
	// direct asks the group resolver's store arm itself rather than the general
	// resolver, which lists the group's children before delegating: the keeper's
	// and the cancel-return's ask (groupStore), whose group is an NGRP by its
	// config save and which never paid that read. Unexported: only this package's
	// group store sets it.
	direct bool
}

// DropChoice is the chooser's answer: the slot, the lane it stands in (0 for a
// flat child of the group), and — when the slot is a stranger's paper — the take.
type DropChoice struct {
	Slot   *nodes.Node
	LaneID int64
	Take   *PaperTake
	// Alternatives are the group's next answers, best first, for a Ranked ask:
	// one slot per other lane (its deepest usable) and each usable flat child.
	Alternatives []DropChoice
}

// PaperTake says the chosen slot is a stranger's groupRoom paper, and the right
// the asker takes it under: by commitment (a committed robot and an unsent
// holder) or, when that is false, by rank (the asker outranks the holder).
type PaperTake struct {
	ByCommitment bool
}

// Over is the planner reading through r instead of its own reader: the same
// parts over another view of the same store (a dispatch gate pass's memo of the
// group reads no release inside the pass can change). The default resolver is
// rebuilt over r, so the chooser's reads go through it too; any other resolver
// is kept as it is.
func (p *Planner) Over(r Reader) *Planner {
	q := *p
	q.db = r
	if _, ok := p.resolver.(*binresolver.DefaultResolver); ok || p.resolver == nil {
		q.resolver = &binresolver.DefaultResolver{DB: r, DebugLog: p.dbg}
	}
	return &q
}

// ChooseDrop chooses the slot a drop into group takes. The error is the
// resolver's when nothing is offered, so a caller's capacity classification
// (CapacityShape) reads it unchanged.
func (p *Planner) ChooseDrop(group *nodes.Node, need DropNeed) (DropChoice, error) {
	defer observe(p.db, "choose-drop")()
	resolver := p.resolver
	if resolver == nil {
		// A planner built without a group resolver (a test configuration; every
		// production planner has one) still chooses through the resolver's own
		// rules, over the same reader. The gate's release never needed an
		// injected resolver for its own lane before C2c, and must not start to.
		resolver = &binresolver.DefaultResolver{DB: p.db, DebugLog: p.dbg}
	}
	if need.Lane != nil {
		res, err := resolver.Resolve(need.Lane, binresolver.ResolveModeStore, need.Payload, need.Stated, need.Asker, nil)
		if err != nil {
			return DropChoice{}, err
		}
		// HARD RULE S ON A LANE-SCOPED ASK. The selector exempts the asker and
		// cannot see plan order, so its slot may stand in front of a pickup this
		// plan makes later in the same lane (swap 270). Asked of one lane, the
		// lane is then nowhere (OwnPickupLane is depth-aware, so a relay's own
		// node — a pickup at the drop's depth — does not refuse it).
		if p.OwnPickupLane(res.Node.Name, need.LaterPickups) == need.Lane.ID {
			return DropChoice{}, fmt.Errorf("no available slot in lane %s: every slot it offers order %d "+
				"walls the order's own later pickup there", need.Lane.Name, need.Asker.OrderID)
		}
		return DropChoice{Slot: res.Node, LaneID: need.Lane.ID}, nil
	}
	exclude := p.dropExclusions(need)
	if need.Ranked {
		if r, ok := resolver.(storeRanker); ok {
			ranked, rErr := r.RankStoreExcludingLanes(group, need.Payload, need.Stated, need.Asker, exclude)
			if rErr == nil && len(ranked) > 0 {
				choices := make([]DropChoice, 0, len(ranked))
				for _, n := range ranked {
					choices = append(choices, DropChoice{Slot: n, LaneID: laneOfChild(group, n)})
				}
				first := choices[0]
				first.Alternatives = choices[1:]
				return first, nil
			}
			if CapacityShape(rErr) != CapacitySlot {
				return DropChoice{}, rErr
			}
			if choice, ok := p.paperChoice(group, need, exclude); ok {
				return choice, nil
			}
			return DropChoice{}, rErr
		}
	}
	var (
		res *binresolver.ResolveResult
		err error
	)
	switch {
	case need.direct && len(exclude) == 0:
		gr := &binresolver.GroupResolver{DB: p.db, DebugLog: p.dbg}
		res, err = gr.ResolveStore(group, need.Payload, need.Stated, need.Asker)
	case len(exclude) == 0:
		// THE SAME CALL THE PLAN HAS ALWAYS MADE, so a drop with nothing to set
		// aside is answered byte-for-byte as before (the planner golden's
		// group-resolver and doors cases are this path).
		res, err = resolver.Resolve(group, binresolver.ResolveModeStore, need.Payload, need.Stated, need.Asker, nil)
	default:
		r, ok := resolver.(laneExcludingResolver)
		if !ok {
			return DropChoice{}, fmt.Errorf("cannot resolve group %s without lanes %v: the resolver cannot set a lane aside",
				group.Name, exclude)
		}
		res, err = r.ResolveStoreExcludingLanes(group, need.Payload, need.Stated, need.Asker, exclude)
	}
	if err == nil {
		return DropChoice{Slot: res.Node, LaneID: laneOfChild(group, res.Node)}, nil
	}
	if CapacityShape(err) != CapacitySlot {
		return DropChoice{}, err
	}
	if choice, ok := p.paperChoice(group, need, exclude); ok {
		return choice, nil
	}
	return DropChoice{}, err
}

// storeRanker is the resolver's ranked store arm with lanes set aside
// (binresolver.DefaultResolver.RankStoreExcludingLanes).
type storeRanker interface {
	RankStoreExcludingLanes(group *nodes.Node, payloadCode string, stated binresolver.BinTypeStatement,
		asker domain.DigAsker, exclude map[int64]bool) ([]*nodes.Node, error)
}

// laneOfChild is the lane a chosen slot stands in, read off the node itself: a
// slot whose parent is the group is a flat child (0).
func laneOfChild(group, slot *nodes.Node) int64 {
	if slot == nil || slot.ParentID == nil || *slot.ParentID == group.ID {
		return 0
	}
	return *slot.ParentID
}

// dropExclusions is the set of lanes the algorithm must not offer: the lanes of
// the plan's later pickups, and the lanes that refused this release. A later
// pickup's lane is its node's parent; a parent that is not a lane of this group
// sets nothing aside, since no child of the group carries its id.
func (p *Planner) dropExclusions(need DropNeed) map[int64]bool {
	var out map[int64]bool
	add := func(id int64) {
		if out == nil {
			out = map[int64]bool{}
		}
		out[id] = true
	}
	for id, refused := range need.Refused {
		if refused {
			add(id)
		}
	}
	for _, name := range need.LaterPickups {
		n, err := p.db.GetNodeByDotName(name)
		if err != nil || n == nil || n.ParentID == nil || n.Depth == nil {
			continue
		}
		add(*n.ParentID)
	}
	return out
}

// paperChoice is the chooser's last resort: a lane slot free but for a
// stranger's groupRoom paper, offered only to an asker that may take it
// (ruling 2: "taken by rank or yields to a committed robot").
//
//   - BY COMMITMENT: the asker is a robot at the group's spot and the holder
//     was never sent. No rank is asked: a plan never blocks a committed robot.
//   - BY RANK: the asker outranks the holder (DemandRank.Outranks; a tie keeps
//     the incumbent).
//   - NEVER a sent holder's paper (D13 F2: commitment wins; a staged robot's
//     paper is not offered to an unsent plan). F2 also allows a committed party
//     to take a committed holder's paper by rank; that arm is not built, so a
//     sent holder's paper is offered to nobody.
//
// A free slot is always preferred: a take costs the holder a re-plan. Among
// takeable paper, the read's order — lanes in id order, deepest first — and
// the first slot the resolver's fences admit.
func (p *Planner) paperChoice(group *nodes.Node, need DropNeed, exclude map[int64]bool) (DropChoice, bool) {
	if need.Asker.OrderID == 0 {
		return DropChoice{}, false // intake: no row yet, so no rank and no commitment
	}
	paper, err := p.db.PaperSlotsInGroup(group.ID, need.Asker.OrderID)
	if err != nil {
		p.debug("plan: paper on %s unreadable (%v) — offering none", group.Name, err)
		return DropChoice{}, false
	}
	if len(paper) == 0 {
		return DropChoice{}, false
	}
	// DIG-FREE AND ENABLED, as the resolver asks it: a lane another order's dig
	// holds is not offered, nor a disabled one.
	children, err := p.db.ListChildNodesUnlocked(group.ID, need.Asker)
	if err != nil {
		return DropChoice{}, false
	}
	open := map[int64]*nodes.Node{}
	for _, c := range children {
		if c.Enabled {
			open[c.ID] = c
		}
	}
	var (
		rank     orders.DemandRank
		rankRead bool
	)
	gr := &binresolver.GroupResolver{DB: p.db, DebugLog: p.dbg}
	for _, ps := range paper {
		lane := open[ps.LaneID]
		if lane == nil || exclude[ps.LaneID] || ps.HolderSent {
			continue
		}
		take := &PaperTake{ByCommitment: need.Committed}
		if !need.Committed {
			if !rankRead {
				if rank, err = p.db.LoadDemandRank(need.Asker.OrderID); err != nil {
					return DropChoice{}, false
				}
				rankRead = true
			}
			holder := orders.DemandRank{Priority: ps.HolderPriority, CreatedAt: ps.HolderCreatedAt, ID: ps.HolderDemand}
			if !rank.Outranks(holder) {
				continue
			}
		}
		if !gr.StoreFitsPaper(group, lane, ps.Slot, need.Payload, need.Stated, need.Asker) {
			continue
		}
		p.debug("plan: %s has no free slot; order %d may take order %d's paper on %s (%s)", group.Name,
			need.Asker.OrderID, ps.Holder, ps.Slot.Name, map[bool]string{true: "a committed robot", false: "by rank"}[need.Committed])
		return DropChoice{Slot: ps.Slot, LaneID: ps.LaneID, Take: take}, true
	}
	return DropChoice{}, false
}
