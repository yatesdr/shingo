package dispatch

import (
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"

	"shingocore/store/reservations"
)

// ── THE MUTUAL DIG HOLD HAS TO APPEAR SOMEWHERE AS ITSELF (§R.75/§R.76) ────
//
// A dig-vs-dig standoff does not show as one. Every wait in it is recorded
// correctly and individually — dig A parked naming lane Y, dig B parked naming
// lane X — and nowhere does anything say that A and B are holding each other. The shape only
// becomes visible when a human reads three rows side by side and notices the
// names close a loop, which on the rig took a slot-by-slot walk of a frozen
// database to establish.
//
// So the loop gets found by the machine and reported as one finding.
//
// ── ALARM ONLY. NOTHING RUNS IN THE TROUBLE PATH ──────────────────────────
//
// This detects and records; a human rules each incident. That is a ruling, not a
// limitation to be designed around later: the automatic response that was drafted
// for this — retreating one dig by putting its blocker back at its own lane's
// mouth — is rejected outright, and bindChosenDestination asserts against it. If
// a future reader is tempted to "just have the tripwire fix it", the fix it would
// reach for is the one that is forbidden, and the assertion will stop it.
//
// ── QUIET WHEN ZERO (law 9) ───────────────────────────────────────────────
//
// The normal state of this sweep is finding nothing and saying nothing. Every
// firing is a defect: admission is supposed to make this unreachable by refusing
// digs the group cannot afford, so a closed walk means the capacity claim
// mis-counted, or the room was eaten by something admission does not see. That is
// the whole diagnostic value — this instrument only ever speaks when the thing
// above it has already failed.

// standoffAction is the recovery-action name a mutual dig hold is filed under.
const standoffAction = "dig_standoff_detected"

// SweepMutualDigHolds walks holder-of-holder across the running digs and records
// every closed walk it finds. Returns the number of distinct standoffs recorded,
// which is zero on a healthy plant and is the number a soak reads.
func (d *Dispatcher) SweepMutualDigHolds() int {
	if d.laneLock == nil {
		return 0
	}
	edges, lanes := d.digBlockingEdges()
	if len(edges) == 0 {
		return 0
	}
	recorded := 0
	for _, cycle := range closedWalks(edges) {
		d.recordStandoff(cycle, lanes)
		recorded++
	}
	return recorded
}

// digBlockingEdges builds the holder-of-holder graph: an edge from each dig that
// still owes a blocker a slot to the dig whose lane holds the parking it wanted.
//
// ── THE EDGE COMES FROM THE POOL WALK, NOT FROM THE CAUSE TEXT ────────────
//
// The cause rows do name the holding dig, and reading them back would mean
// parsing queue_reason — a human-readable sentence — to make a structural
// decision. This codebase has a scar exactly there: planUsedExposeMode recovered
// a plan's mode by string-matching PayloadDesc, and a lock decision rode a
// description field until it was deleted. So the edge is re-derived from state
// through the same predicate that produced the wait in the first place:
// findShuffleSlots with the dig's own asker, whose DigParkingHeldError already
// carries the holder's id because right of way needed it to name the releaser.
//
// A dig with no outstanding claim has no edge — it is not waiting for parking, so
// it cannot be a link in a standoff, whatever else it is doing.
func (d *Dispatcher) digBlockingEdges() (map[int64]int64, map[int64]string) {
	holds, err := reservations.ListDigHolds(d.db.DB)
	if err != nil {
		log.Printf("dig standoff: could not read the dig holds: %v (skipping this pass)", err)
		return nil, nil
	}
	edges := map[int64]int64{}
	lanes := map[int64]string{}
	for _, h := range holds {
		if _, done := edges[h.OrderID]; done {
			continue // one edge per dig; the first lane that answers is enough
		}
		lane, lErr := d.db.GetNode(h.LaneID)
		if lErr != nil || lane == nil || lane.ParentID == nil {
			continue
		}
		// Does this dig still owe a blocker a slot? Asked through the same
		// predicate admission counts with, so the tripwire and the claim cannot
		// disagree about what "owing" means.
		owing, oErr := d.db.ListOutstandingDigClaims(*lane.ParentID, reservations.Anyone)
		if oErr != nil {
			continue
		}
		if !slices.Contains(owing, h.OrderID) {
			continue
		}
		_, sErr := findShuffleSlots(d.db, h.LaneID, *lane.ParentID, 1, d.digAsker(h.OrderID), nil)
		var held *DigParkingHeldError
		if !errors.As(sErr, &held) || held.HolderID == 0 {
			continue // not blocked by another dig: no edge
		}
		edges[h.OrderID] = held.HolderID
		lanes[h.OrderID] = lane.Name
	}
	return edges, lanes
}

// closedWalks returns every distinct cycle in a graph where each node has at most
// one outgoing edge.
//
// THE SHAPE MAKES THIS SIMPLE AND IT IS WORTH SAYING WHY. Each dig is waiting on
// exactly one holder, so the graph is a functional graph: following edges from any
// node walks a path that either runs out or enters a cycle, and it can never
// branch. So there is no search here — just a walk per node, and a revisit inside
// the current walk is the cycle.
//
// Cycles are canonicalised by their smallest order id and de-duplicated, so a
// three-way standoff is reported once rather than once per participant.
func closedWalks(edges map[int64]int64) [][]int64 {
	var out [][]int64
	seen := map[string]bool{}
	for start := range edges {
		pos := map[int64]int{}
		var path []int64
		for at, ok := start, true; ok; at, ok = edges[at] {
			if i, dup := pos[at]; dup {
				cycle := canonicalCycle(path[i:])
				key := fmt.Sprint(cycle)
				if !seen[key] {
					seen[key] = true
					out = append(out, cycle)
				}
				break
			}
			pos[at] = len(path)
			path = append(path, at)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// canonicalCycle rotates a cycle to start at its smallest id so the same standoff
// found from three different entry points produces one key.
func canonicalCycle(cycle []int64) []int64 {
	if len(cycle) == 0 {
		return cycle
	}
	min := 0
	for i, v := range cycle {
		if v < cycle[min] {
			min = i
		}
	}
	out := make([]int64, 0, len(cycle))
	out = append(out, cycle[min:]...)
	out = append(out, cycle[:min]...)
	return out
}

// recordStandoff files one closed walk as its own defect row and says it loudly.
//
// LOUD IS THE POINT. A standoff is a set of loaded robots that will not move
// again without intervention, and the individual waits under it all look lawful —
// each one names a real lane held by a real dig with a real releaser. Reporting it
// at the volume of an ordinary wait would bury the one line that says the waits
// are circular.
func (d *Dispatcher) recordStandoff(cycle []int64, lanes map[int64]string) {
	var parts []string
	for _, id := range cycle {
		if lane := lanes[id]; lane != "" {
			parts = append(parts, fmt.Sprintf("dig %d (digging %s)", id, lane))
		} else {
			parts = append(parts, fmt.Sprintf("dig %d", id))
		}
	}
	loop := strings.Join(parts, " waits on ")
	detail := fmt.Sprintf("MUTUAL DIG HOLD, %d digs: %s waits on dig %d — the walk is closed. "+
		"Every one of these waits is individually lawful and none of them can clear: each dig is "+
		"holding the lane the next one needs for parking, and each is waiting for a slot only "+
		"another member of the cycle can release. It will not self-clear. "+
		"THIS SHOULD BE UNREACHABLE: dig admission counts the group's usable room against the "+
		"claims already outstanding precisely so a group cannot start digs it cannot feed, so a "+
		"firing here means the claim mis-counted or the room was eaten by a writer admission does "+
		"not see. A human rules the incident; nothing automatic runs.",
		len(cycle), loop, cycle[0])

	if err := d.db.RecordRecoveryAction(standoffAction, "order", cycle[0], detail, "system"); err != nil {
		log.Printf("dig standoff: could not record the standoff for dig %d: %v", cycle[0], err)
	}
	log.Printf("DIG STANDOFF: %s", detail)
}
