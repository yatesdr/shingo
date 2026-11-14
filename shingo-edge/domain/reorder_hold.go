package domain

import (
	"fmt"
	"time"
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
	// waiting robots were sent back. Until the line counts a part or someone
	// gives a go.
	ReorderHoldOff ReorderHold = "off"
	// ReorderHoldPersonOff: a person switched auto-reorder off here. Only a
	// person clears it.
	ReorderHoldPersonOff ReorderHold = "person_off"
)

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
			Sentence: fmt.Sprintf("Auto-reorder is paused until %s: %s. REQUEST asks now.", clockTime(until), note)}
	case ReorderHoldOff:
		return ReorderHeld{Hold: ReorderHoldOff,
			Sentence: fmt.Sprintf("Auto-reorder is off: %s. REQUEST asks now.", note)}
	case ReorderHoldPersonOff:
		return ReorderHeld{Hold: ReorderHoldPersonOff,
			Sentence: fmt.Sprintf("Auto-reorder was switched off here by %s at %s.", note, clockTime(rt.ReorderHoldAt))}
	default:
		return ReorderHeld{Hold: rt.ReorderHold,
			Sentence: fmt.Sprintf("Auto-reorder is held (%q, which this Edge does not know). REQUEST asks now.", string(rt.ReorderHold))}
	}
}

// clockTime is a time of day as the board shows one, in the Edge's zone.
func clockTime(t time.Time) string {
	return t.Local().Format("15:04")
}
