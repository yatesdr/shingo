package scenesim

import (
	"fmt"
	"sort"
	"strings"
)

// Violation is one invariant-checker failure observed after a tick. Any
// violation fails the seed; the Detail is the evidence for the trace.
type Violation struct {
	Checker string
	Tick    int
	Detail  string
}

// check runs the live S0 checkers: mode purity and no-deadlock. Later stages add
// reachability, packing, contract sweep, starvation, digger purity.
func (s *Sim) check() []Violation {
	var v []Violation
	v = append(v, s.checkModePurity()...)
	v = append(v, s.checkNoDeadlock()...)
	v = append(v, s.checkReachability()...)
	v = append(v, s.checkDigOccupancy()...)
	return v
}

// checkReachability (checker 3): an in-flight order still heading to a lane slot
// must be able to physically reach it — no bin may sit in a SHALLOWER slot of the
// same lane, which single-file would wall the target off. This is the entry-order
// air bubble (§13.4): a shallow store drops first and walls a deeper bind.
func (s *Sim) checkReachability() []Violation {
	var v []Violation
	for _, id := range s.order {
		r := s.robots[id]
		if r.order == nil || r.idle || r.block >= len(r.order.Blocks) {
			continue
		}
		target := r.order.Blocks[r.block].Location
		lane := s.scene.slotLane[target]
		if lane == "" {
			continue // current target isn't a lane slot
		}
		depth, ok := s.scene.SlotDepth(target)
		if !ok {
			continue
		}
		// If the robot is already at or past its target depth, it's in — skip.
		if r.pos.inLane() && r.pos.Lane == lane && r.pos.Index >= depth {
			continue
		}
		// Only slots AHEAD of the robot can wall it. A bin BEHIND a robot already
		// in the lane (shallower than its current cell) was passed and cannot block
		// it — checking from the mouth would falsely flag a deep robot whose shallow
		// neighbor dropped behind it. A robot not yet in the lane must clear from
		// the mouth (start 0).
		start := 0
		if r.pos.inLane() && r.pos.Lane == lane {
			start = r.pos.Index + 1
		}
		for i := start; i < depth; i++ {
			slot := s.scene.lanes[lane].Slots[i]
			if s.bins[slot] {
				// A wall a SIBLING LEG OF THE SAME DIG is on its way to remove is
				// not a wall — it is the dig working. This checker guards the
				// entry-order air bubble: a bin DROPPED shallow walls a deeper
				// bind permanently, because nothing is scheduled to move it. A
				// blocker that another leg of my own group is bound to pick up is
				// scheduled to move, by construction, and the mouth gate keeps me
				// out of the lane until it has.
				//
				// The exemption is deliberately narrow: the walling slot must be
				// the CURRENT PICKUP TARGET of a live sibling leg. Not "a dig is
				// running" and not "someone in my group is busy" — either of those
				// would excuse a genuine bubble sitting in a lane a dig happens to
				// be working. Cross-order bubbles, which is what the checker exists
				// for, are untouched.
				//
				// Found by the group matrix: the single-order dig never tripped
				// this because its blocks are SEQUENCED, so its own target is
				// always clear by the time it becomes current. Concurrent legs
				// break that assumption, and they are what production is about to
				// start doing.
				if s.siblingLegIsClearing(r, slot) {
					continue
				}
				v = append(v, Violation{
					Checker: "reachability",
					Tick:    s.tick,
					Detail: fmt.Sprintf("order %s bound to %s (depth %d) is walled off by a bin at %s (depth %d)",
						r.order.ID, target, depth, slot, i),
				})
				break
			}
		}
	}
	return v
}

// checkModePurity (checker 1): every robot committed into one lane shares one
// work kind, and a dig never shares. "Committed" = physically in the lane or on
// a path heading into it. Mode is derived from what the robot's order DOES in the
// lane (dropoff → inbound, pickup → outbound, dig order → dig).
func (s *Sim) checkModePurity() []Violation {
	var v []Violation
	for _, lane := range s.scene.LaneNames() {
		robots := s.committedTo(lane)
		if len(robots) < 2 {
			continue
		}
		modes := map[string]string{}
		distinct := map[string]bool{}
		digGroups := map[string]bool{}
		for _, r := range robots {
			m, ok := s.orderLaneMode(r, lane)
			if !ok {
				continue
			}
			modes[r.ID] = m
			distinct[m] = true
			if m == "dig" && r.order != nil {
				digGroups[r.order.DigGroup] = true
			}
		}
		// The second clause used to read `digs > 0 && len(modes) > 1`, and its
		// spelling hid what it meant. Any NON-dig robot committed alongside a dig
		// already trips len(distinct) > 1, because "dig" differs from "inbound"
		// and "outbound" — so the only property that clause carried on its own was
		// NO TWO DIGS SHARE A LANE. Keyed on groups it carries the same property
		// one level looser: two legs of ONE dig are legal (that is the whole point
		// of groups), two DIFFERENT digs are not. It is also strictly stronger in
		// the axis that matters, since the old form caught two different digs only
		// incidentally, via the robot count.
		if len(distinct) > 1 || len(digGroups) > 1 {
			v = append(v, Violation{
				Checker: "mode-purity",
				Tick:    s.tick,
				Detail:  fmt.Sprintf("lane %s has mixed work among committed robots: %s", lane, fmtModes(modes)),
			})
		}
	}
	return v
}

// checkNoDeadlock (checker 2): no cycle of mutually-blocked robots, and a global
// no-progress watchdog (no state change for Watchdog ticks with work outstanding).
func (s *Sim) checkNoDeadlock() []Violation {
	var v []Violation

	// Cycle in the blocked-by graph (each robot points at the one blocking it).
	color := map[string]int{} // 0 unvisited, 1 on-stack, 2 done
	var stack []string
	var found []string
	var dfs func(id string) bool
	dfs = func(id string) bool {
		color[id] = 1
		stack = append(stack, id)
		if b := s.robots[id].blockedBy; b != "" {
			if color[b] == 1 {
				found = append([]string(nil), stack...)
				found = append(found, b)
				return true
			}
			if color[b] == 0 && dfs(b) {
				return true
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = 2
		return false
	}
	for _, id := range s.order {
		if color[id] == 0 && dfs(id) {
			v = append(v, Violation{
				Checker: "no-deadlock",
				Tick:    s.tick,
				Detail:  fmt.Sprintf("mutually-blocked cycle: %v", found),
			})
			break
		}
	}

	// No-progress watchdog: outstanding work but nothing has changed. Suppressed
	// while any robot is legitimately parked on a Wait block (an external
	// ReleaseWait is expected — that is pending, not deadlocked).
	if !s.AllIdle() && !s.anyWaiting() && s.tick-s.lastProgress >= s.opts.Watchdog {
		v = append(v, Violation{
			Checker: "no-deadlock",
			Tick:    s.tick,
			Detail:  fmt.Sprintf("no progress for %d ticks with work outstanding", s.tick-s.lastProgress),
		})
	}
	return v
}

func (s *Sim) anyWaiting() bool {
	for _, r := range s.robots {
		if r.waiting {
			return true
		}
	}
	return false
}

// committedTo returns robots PHYSICALLY inside the lane. A robot merely heading
// toward the lane (path planned) but held at the boundary by the mouth gate has
// NOT committed — it can still be turned away — so it must not count toward mode
// purity, or the gate holding a different-mode robot out would read as a mixed-
// mode violation. Once a robot steps onto a lane cell it is committed.
func (s *Sim) committedTo(lane string) []*Robot {
	var out []*Robot
	for _, id := range s.order {
		r := s.robots[id]
		if r.pos.inLane() && r.pos.Lane == lane {
			out = append(out, r)
		}
	}
	return out
}

// orderLaneMode derives a robot's work kind in a lane from its order's blocks
// there. Returns false if the order doesn't touch the lane.
func (s *Sim) orderLaneMode(r *Robot, lane string) (string, bool) {
	if r.order == nil {
		return "", false
	}
	touches := false
	if r.order.Dig {
		for _, b := range r.order.Blocks {
			if s.scene.LaneForNode(b.Location) == lane {
				return "dig", true
			}
		}
		return "", false
	}
	mode := "outbound"
	for _, b := range r.order.Blocks {
		if s.scene.LaneForNode(b.Location) == lane {
			touches = true
			if b.Action == ActionDropoff {
				mode = "inbound"
			}
		}
	}
	return mode, touches
}

func fmtModes(m map[string]string) string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%s=%s", id, m[id]))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// checkDigOccupancy (checker 4) is the acceptance gate for relaxing compound
// serialization: ON A LANE A DIG GROUP HOLDS, EXACTLY ONE ROBOT IS INSIDE, AND IT
// BELONGS TO THAT GROUP.
//
// It is deliberately NOT "at most one robot inside any lane". That is not an
// invariant of this system and a checker asserting it would fail most of the
// suite: same-KIND co-occupancy is the mouth gate's designed behaviour (see
// admitToLane), and SetLaneCapacity1 exists precisely as the comparison arm that
// turns co-occupancy off. One-robot-per-lane is a baseline being measured
// against, not a rule. Scoped to dig-held lanes, the statement is true every
// tick and does not contradict mode share.
//
// The two halves are the two holds. "A group holds the lane" is Hold A, which
// spans the whole reshuffle including the gaps between legs. "Exactly one robot
// inside" is Hold B, which belongs to one leg and ends when that leg places its
// bin. Several legs of one dig may be in flight; only one may be in the lane.
//
// The detail string names both robots AND their groups, because the interesting
// failure is SAME GROUP, BOTH INSIDE — that is Hold B failing, and to anyone
// reading only mode purity it looks fine: one group, one mode, no impurity.
func (s *Sim) checkDigOccupancy() []Violation {
	var v []Violation
	for _, lane := range s.scene.LaneNames() {
		committed := s.committedTo(lane)
		if len(committed) == 0 {
			continue
		}
		holder := s.digClaimHolder(lane)
		digInside := false
		for _, r := range committed {
			if m, ok := s.orderLaneMode(r, lane); ok && m == "dig" {
				digInside = true
				break
			}
		}
		if holder == "" && !digInside {
			continue // no dig involved in this lane — this checker has no opinion
		}
		if len(committed) > 1 {
			v = append(v, Violation{
				Checker: "dig-occupancy",
				Tick:    s.tick,
				Detail: fmt.Sprintf("lane %s is held by dig group %q but %d robots are inside: %s",
					lane, holder, len(committed), fmtRobotGroups(committed)),
			})
			continue
		}
		r := committed[0]
		inGroup := r.order != nil && r.order.DigGroup == holder
		if holder != "" && !inGroup {
			v = append(v, Violation{
				Checker: "dig-occupancy",
				Tick:    s.tick,
				Detail: fmt.Sprintf("lane %s is held by dig group %q but the robot inside is %s",
					lane, holder, fmtRobotGroups(committed)),
			})
		}
	}
	return v
}

// fmtRobotGroups renders robots as id=group (or id=<no-group> for non-dig work),
// deterministically ordered.
func fmtRobotGroups(rs []*Robot) string {
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		g := "<no-group>"
		if r.order != nil && r.order.DigGroup != "" {
			g = r.order.DigGroup
		}
		parts = append(parts, fmt.Sprintf("%s=%s", r.ID, g))
	}
	sort.Strings(parts)
	return "[" + strings.Join(parts, ", ") + "]"
}

// siblingLegIsClearing reports whether another leg of r's dig group is currently
// bound to pick the bin up out of slot — i.e. the wall in front of r is being
// actively removed by r's own reshuffle.
func (s *Sim) siblingLegIsClearing(r *Robot, slot string) bool {
	if r.order == nil || r.order.DigGroup == "" {
		return false
	}
	for _, id := range s.order {
		o := s.robots[id]
		if o.ID == r.ID || o.order == nil || o.idle || o.order.DigGroup != r.order.DigGroup {
			continue
		}
		if o.block >= len(o.order.Blocks) {
			continue
		}
		b := o.order.Blocks[o.block]
		if b.Location == slot && b.Action == ActionPickup {
			return true
		}
	}
	return false
}
