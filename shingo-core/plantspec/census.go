package plantspec

import (
	"fmt"
	"sort"
)

// ── THE CENSUS AT BIRTH (§R.78) ───────────────────────────────────────────
//
// A plant spec says where every bin starts. Until now nothing checked what that
// arrangement MEANS, and the lane-stress rig paid for it: its two worst air
// bubbles — empty slots buried behind bins, unreachable by anything, for the
// whole life of the plant — were not drilled by any dig. They came out of the
// seeder that way, and the group they were in started 73% full with a working
// margin of one slot in thirty-seven (§R.77). Every dig-vs-dig standoff the
// batch above exists to prevent grew in that soil.
//
// So the seed asserts two things about itself before a single order runs.
//
//	PACKED LANES.  A partially filled lane is filled from the BACK: its bins
//	occupy the deepest slots and its empties are at the mouth, contiguous. A
//	hole at birth is a slot no robot can reach and no dig can create room in,
//	and it is a seeder defect rather than a plant condition.
//
//	HEADROOM.  A group keeps at least one full lane's worth of slots free, so
//	there is somewhere to dig INTO. A group filled to the brim cannot conduct
//	an excavation at all — the blockers have nowhere to stand — and every dig
//	admitted against it is a dig that will park.
//
// ── WHY IT IS THE SPEC AND NOT THE DATABASE ───────────────────────────────
//
// Because this seeder places bins by explicit slot name: what the spec says is
// exactly what is born, one for one. A census that read the database back would
// be asking the same question one layer further from the author, and would name
// a node id where this names the line of YAML that has to change.
//
// It stops being true the moment a spec generates placements rather than listing
// them. At that point this moves to a read of the seeded rows, and the rules do
// not change — only where they are asked.

// LaneBubble is one air bubble found at birth: an empty slot with a bin behind it.
type LaneBubble struct {
	Zone  string
	Lane  string
	Slot  string
	Depth int
	// BlockedBy names the shallowest occupied slot IN FRONT of this hole — the
	// bin a robot would meet first, and the one that has to move.
	BlockedBy string
}

// GroupOverfill is one group with too little room to dig in.
type GroupOverfill struct {
	Zone      string
	Slots     int
	Bins      int
	Reachable int // free slots a dig could actually park in
	Reserve   int // slots that had to stay free: LanesFree × the deepest lane
	DeepLane  string
	DeepDepth int
}

// Census is what the spec says about itself: the bubbles and the overfilled
// groups. Empty means the seed is legal, which is the normal answer and the one
// that prints nothing.
type Census struct {
	Bubbles   []LaneBubble
	Overfills []GroupOverfill
}

// Clean reports whether the seed passes both rules.
func (c Census) Clean() bool { return len(c.Bubbles) == 0 && len(c.Overfills) == 0 }

// Findings renders the census as one line per defect, each naming what to change.
func (c Census) Findings() []string {
	var out []string
	for _, b := range c.Bubbles {
		out = append(out, fmt.Sprintf(
			"AIR BUBBLE AT BIRTH: %s/%s slot %s (depth %d) is empty and walled in by %s in front of "+
				"it. Nothing can reach it and nothing will open it — the bin in front is not in "+
				"anybody's way, so no dig will ever be raised against it. A lane seeds PACKED: bins "+
				"at the back, empties at the mouth, no holes (§R.78)",
			b.Zone, b.Lane, b.Slot, b.Depth, b.BlockedBy))
	}
	for _, o := range c.Overfills {
		out = append(out, fmt.Sprintf(
			"NO ROOM TO DIG: group %s seeds %d bins into %d slots and leaves only %d slot(s) a dig "+
				"can actually park in, where the rule needs %d (one lane's worth, and %s at depth %d "+
				"is the deepest). REACHABLE is the number that matters, not empty: a slot behind a "+
				"bin is not room, it is an air bubble, and counting it is how a group reads as having "+
				"headroom it cannot use. A group this short cannot conduct an excavation — a dig has "+
				"nowhere to stand its blockers — so every dig it admits is a dig that parks",
			o.Zone, o.Bins, o.Slots, o.Reachable, o.Reserve, o.DeepLane, o.DeepDepth))
	}
	return out
}

// CensusAtBirth walks the spec's initial bin placements and reports every air
// bubble and every group without room to dig in.
//
// It reports ALL of them rather than the first, for the same reason Validate
// does: a spec author fixes a seed in one pass, and a census that stops at the
// first hole hides how fragmented the plant actually is.
func (p *Plant) CensusAtBirth() Census {
	occupied := make(map[string]bool, len(p.Bins))
	for _, b := range p.Bins {
		occupied[b.Slot] = true
	}

	var c Census
	for _, z := range p.Zones {
		slots, bins, reachable, deepDepth, deepLane := 0, 0, 0, 0, ""

		for _, lane := range z.Lanes {
			ordered := append([]Slot(nil), lane.Slots...)
			sort.Slice(ordered, func(i, j int) bool { return ordered[i].Depth < ordered[j].Depth })

			slots += len(ordered)
			if len(ordered) > deepDepth {
				deepDepth, deepLane = len(ordered), lane.Name
			}

			// ── THE RULE IS THE RUNTIME'S OWN, NOT A SECOND ONE ──────────
			//
			// "Packed from the back" and "no holes" are one test, and it is
			// exactly nodes.IsSlotAccessible asked of the seed: an empty slot with
			// ANY occupied slot shallower than it cannot be reached. That covers
			// a hole in the middle and a lane filled from the mouth inward alike —
			// the second looks tidier and buries just as much.
			//
			// Walking shallow→deep, the first bin seen blocks every empty after
			// it, so one pass answers the whole lane and names the bin a robot
			// would meet first.
			blocker := ""
			for _, s := range ordered {
				if occupied[s.Name] {
					bins++
					if blocker == "" {
						blocker = s.Name
					}
					continue
				}
				if blocker == "" {
					// An empty at the mouth, in front of everything: legal, and the
					// only kind of free slot a dig can actually put a blocker in.
					reachable++
					continue
				}
				c.Bubbles = append(c.Bubbles, LaneBubble{
					Zone: z.Name, Lane: lane.Name, Slot: s.Name, Depth: s.Depth, BlockedBy: blocker,
				})
			}
		}

		// ── WHO THE HEADROOM RULE IS FOR ─────────────────────────────────
		//
		// Groups that can dig. A group with ONE lane cannot, at any fill level:
		// findShuffleSlots refuses to park a blocker back into the lane being dug
		// (the `c.ID == laneID` skip), so a lone lane has nowhere to send one and
		// no amount of reserved slack changes that. Demanding headroom there
		// would report a shortage that is not the problem — the problem is the
		// geometry, and it is a different finding.
		//
		// A zone with no lanes has no rule to break either, and neither has one
		// whose headroom is configured to zero: an author writing "this group
		// runs full" has said it on purpose and owns the consequence.
		reserve := deepDepth * p.Headroom.LanesFree()
		if len(z.Lanes) < 2 || deepDepth == 0 || reserve == 0 {
			continue
		}
		// REACHABLE, NOT EMPTY. Counting bare emptiness passed SYN_COMP on the
		// lane-stress seed: ten free slots, of which three were walled bubbles and
		// the rest scattered singles behind bins. Its real parking capacity was
		// four, two digs wanted six, and the closing run deadlocked in under a
		// minute on a group the rule had called healthy.
		if reachable < reserve {
			c.Overfills = append(c.Overfills, GroupOverfill{
				Zone: z.Name, Slots: slots, Bins: bins, Reachable: reachable,
				Reserve: reserve, DeepLane: deepLane, DeepDepth: deepDepth,
			})
		}
	}
	return c
}
