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
		for i := range depth {
			slot := s.scene.lanes[lane].Slots[i]
			if s.bins[slot] {
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
		digs := 0
		for _, r := range robots {
			m, ok := s.orderLaneMode(r, lane)
			if !ok {
				continue
			}
			modes[r.ID] = m
			distinct[m] = true
			if m == "dig" {
				digs++
			}
		}
		if len(distinct) > 1 || (digs > 0 && len(modes) > 1) {
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

// committedTo returns robots physically in the lane or on a path heading into it.
func (s *Sim) committedTo(lane string) []*Robot {
	var out []*Robot
	for _, id := range s.order {
		r := s.robots[id]
		in := r.pos.inLane() && r.pos.Lane == lane
		heading := false
		for _, c := range r.path {
			if c.inLane() && c.Lane == lane {
				heading = true
				break
			}
		}
		if in || heading {
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
