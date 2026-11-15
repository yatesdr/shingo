package domain

import (
	"fmt"
	"strings"
	"time"

	"shingo/protocol"
)

// reorder_hold.go — why an armed position's automatic ask is held.
//
// ONE STATE, AT THE POSITION. A position whose claim is armed (auto_reorder)
// and whose level is breached is asked for by the demand sweep
// (engine/demand_reconciler.go), every pass, until something is coming. A hold
// stops that ask at one position without touching the claim. It is stored on
// the position's runtime row (process_node_runtime_states.reorder_hold, _at,
// _note), not on the claim: the claim's flag is per style, cannot be written
// while a changeover runs, re-arms the keep-staged keeper when saved, and keeps
// no record of who or why. The sweep reads the runtime row in the batched read
// it already makes, so the check costs no statement.
//
// KINDS ARE RANKED, AND A WRITE NEVER STEPS DOWN: paused < off < a person's
// off. Core abandons a two-robot swap with two cancels in one call stack, the
// dead leg's (its reason starts "abandoned:") and then its sibling's
// ("coordinated swap … cancelling …", shingo-core/dispatch/swap_peer.go), which
// does not. Applied in that order with no rank, the second would turn the
// first's off into a pause. The rank is the WHERE clause of the one write
// (store/processes.HoldReorder), so the refusal costs no read.

// ReorderHold is the kind of hold a position's runtime row carries; the zero
// value holds nothing.
type ReorderHold string

const (
	// ReorderHoldNone: the sweep asks as the claim says.
	ReorderHoldNone ReorderHold = ""
	// ReorderHoldPaused: a cancel or a failure at the position. The sweep
	// asks again ReorderPause after it was set, or sooner on a go.
	ReorderHoldPaused ReorderHold = "paused"
	// ReorderHoldOff: the line counted no part for the idle hour and its
	// waiting robots were sent back (engine/idle_cancel.go), or an older Core
	// gave up on an order that waited. Until the line counts a part after a
	// stop, or someone gives a go.
	ReorderHoldOff ReorderHold = "off"
	// ReorderHoldPersonOff: a person switched auto-reorder off here. Only a
	// person clears it.
	ReorderHoldPersonOff ReorderHold = "person_off"
	// ReorderHoldCount: the count at the position cannot be its claim's
	// (countHeld). Decided from the count each time and never stored: it is
	// not in ReorderHolds, so the one write refuses it.
	ReorderHoldCount ReorderHold = "count"
)

// IdleOffNotePrefix opens the note the idle rule writes with its off
// (IdleOffNote). An off with this note was written at a line that had counted
// nothing for its hour, so any part the line counts after it is a restart and
// ends it (store/processes.ClearIdleOffAtProcess); an older Core's off, whose
// note is its own, ends only after a stop and a restart, or a go.
const IdleOffNotePrefix = "the line counted no part for "

// IdleOffNote is the idle rule's note for its off: the line counted no part
// for idleWords (its hour, as the board says it) and the robots were sent back.
func IdleOffNote(idleWords string) string {
	return IdleOffNotePrefix + idleWords + " and the robots waiting here were sent back"
}

// IsIdleOff reports whether a held off is the idle rule's own (its note).
func IsIdleOff(hold ReorderHold, note string) bool {
	return hold == ReorderHoldOff && strings.HasPrefix(note, IdleOffNotePrefix)
}

// ReorderHolds is every kind that holds, lowest rank first. The rank the write
// guards on is the position in this list (Rank); store/processes builds its
// SQL from it, so the two cannot disagree.
var ReorderHolds = []ReorderHold{ReorderHoldPaused, ReorderHoldOff, ReorderHoldPersonOff}

// Rank is the kind's place in ReorderHolds, 1 for the lowest; 0 for none and
// for a word this Edge does not know.
func (h ReorderHold) Rank() int {
	for i, k := range ReorderHolds {
		if h == k {
			return i + 1
		}
	}
	return 0
}

// ReorderPause is how long a pause holds the ask: long enough that the sweep's
// next pass, a minute later, does not re-ask what someone has just cancelled,
// short enough that a position with little headroom is not starved by it. A
// REQUEST ends it at once.
const ReorderPause = 10 * time.Minute

// ReorderHeld is what holds an armed position's automatic ask now: the hold in
// force and the board's sentence for it. The zero value holds nothing.
type ReorderHeld struct {
	Hold ReorderHold
	// Until is when a pause ends; zero for every other kind.
	Until time.Time
	// Sentence is the station view's line for the tile, verbatim.
	Sentence string
}

// Holds reports whether the ask is held.
func (h ReorderHeld) Holds() bool { return h.Sentence != "" }

// AutoReorderHeld is the one decider of whether an armed position's automatic
// ask is held at now, read by the demand sweep (after its auto_reorder gate)
// and by the station view (for the tile's chip). A claim that is not armed
// holds nothing here: the sweep never asks for it, and a tile has nothing to
// say about an ask that is not made.
//
// A pause past its ReorderPause holds nothing and is left on the row: the next
// write over it is allowed by rank, and clearing it would cost a statement for
// no reader.
//
// A KIND THIS EDGE DOES NOT KNOW HOLDS. It was written by a newer Edge that has
// since been rolled back; asking for a position whose hold it cannot read is
// the one answer that cannot be wrong for long, and a go clears it.
func AutoReorderHeld(c *NodeClaim, rt *RuntimeState, now time.Time) ReorderHeld {
	// A loader's claim is never asked for by the sweep (it returns before the
	// level), so a hold there would be a sentence about nothing.
	if c == nil || rt == nil || !c.AutoReorder || c.IsLoaderNode() {
		return ReorderHeld{}
	}
	if h := heldByRow(rt, now); h.Holds() {
		return h
	}
	return countHeld(c, rt.RemainingUOPCached)
}

// heldByRow is the hold the runtime row carries, in force at now.
func heldByRow(rt *RuntimeState, now time.Time) ReorderHeld {
	note := rt.ReorderHoldNote
	switch rt.ReorderHold {
	case ReorderHoldNone:
		return ReorderHeld{}
	case ReorderHoldPaused:
		until := rt.ReorderHoldAt.Add(ReorderPause)
		if !now.Before(until) {
			return ReorderHeld{}
		}
		return ReorderHeld{Hold: ReorderHoldPaused, Until: until,
			Sentence: fmt.Sprintf("Auto-reorder is paused until %s: %s. REQUEST asks now and ends the pause.", clockTime(until), note)}
	case ReorderHoldOff:
		return ReorderHeld{Hold: ReorderHoldOff,
			Sentence: fmt.Sprintf("Auto-reorder is off: %s. It comes back on when the line counts a part after a stop, or on REQUEST.", note)}
	case ReorderHoldPersonOff:
		return ReorderHeld{Hold: ReorderHoldPersonOff,
			Sentence: fmt.Sprintf("Auto-reorder was switched off here by %s at %s.", note, clockTime(rt.ReorderHoldAt))}
	default:
		return ReorderHeld{Hold: rt.ReorderHold,
			Sentence: fmt.Sprintf("Auto-reorder is held (%q, which this Edge does not know). REQUEST clears it.", string(rt.ReorderHold))}
	}
}

// countHeld is the arming check: the sweep does not ask on a count that cannot
// be this claim's, and says so. It refuses nothing: the claim still saves,
// REQUEST still asks, and a corrected count lifts it.
//
//   - Produce: below 0, or past two packs. Two, not one: a strict-pack seat's
//     set-aside parts are charged to the next cart when it binds, so a count
//     can honestly run up to a pack past the pack.
//   - Consume: past one pack. Below 0 still asks: a consume count runs
//     negative honestly when the line draws more than was booked onto its
//     cart, and that line needs material most.
//
// A claim whose pack is not known (0) is not checked: there is nothing to
// measure the count against, and the sweep asks as it always has.
func countHeld(c *NodeClaim, count int) ReorderHeld {
	pack := c.UOPCapacity
	if pack <= 0 {
		return ReorderHeld{}
	}
	switch {
	case c.Role == protocol.ClaimRoleProduce && (count < 0 || count > 2*pack):
		return ReorderHeld{Hold: ReorderHoldCount, Sentence: fmt.Sprintf(
			"Auto-reorder is not asking: a count of %d cannot be this part's at a pack of %d. Correct the count; REQUEST still asks.", count, pack)}
	case c.Role == protocol.ClaimRoleConsume && count > pack:
		return ReorderHeld{Hold: ReorderHoldCount, Sentence: fmt.Sprintf(
			"Auto-reorder is not asking: a count of %d is more than a pack of %d. Correct the count; REQUEST still asks.", count, pack)}
	}
	return ReorderHeld{}
}

// clockTime is a time of day as the board shows one, in the Edge's zone.
func clockTime(t time.Time) string {
	return t.Local().Format("15:04")
}

// AutoReorderFlip is one flip of a line's auto-reorder switch, as
// auto_reorder_flips records it: which way, who, from where (Processes or the
// station), and when (the engine's clock).
type AutoReorderFlip struct {
	ID        int64     `json:"id"`
	ProcessID int64     `json:"process_id"`
	TurnedOn  bool      `json:"turned_on"`
	By        string    `json:"by"`
	Source    string    `json:"source"`
	At        time.Time `json:"at"`
}

// Where a flip of the switch came from (AutoReorderFlip.Source).
const (
	FlipFromProcesses = "processes"
	FlipFromStation   = "station"
)

// SwitchedOffOnTiles reports whether a person has switched auto-reorder off
// at any of a board's positions, from the runtime rows its tiles hold.
func SwitchedOffOnTiles(tiles []StationNodeView) bool {
	for _, t := range tiles {
		if t.Runtime != nil && t.Runtime.ReorderHold == ReorderHoldPersonOff {
			return true
		}
	}
	return false
}

// AutoReorderSwitch is a line's switch as Processes shows it: how many of the
// line's positions a person has switched off, of how many, and the last flip
// (nil when the line was never flipped). Settings › Auto-reorder reads exactly
// these; the station setting is the process row's (auto_reorder_switch_hmi),
// read with the row.
type AutoReorderSwitch struct {
	Positions   int              `json:"positions"`
	SwitchedOff int              `json:"switched_off"`
	LastFlip    *AutoReorderFlip `json:"last_flip,omitempty"`
}
