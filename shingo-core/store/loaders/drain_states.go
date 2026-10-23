package loaders

import (
	"errors"
	"fmt"

	"shingo/protocol"
)

// drain_states.go — what an unloader drains from its inbound source beside its
// fulls (v165; owner, 2026-10-22: "just check boxes ... allow the unloader to
// click if it wants that option to auto drain for the different states").
//
// An unloader pulls FULLS of the parts it serves, always (U1), and partials only
// with Accept partials. It never pulled a carrier with no part, so a press
// whose outbound is a plain group, not the unloader, stranded SEND BACK's
// leftover-tote cart there. The four boxes are the part-less states a plant may
// have its unloader drain as well.

// DrainStates is the part-less carrier states an unloader drains from its
// inbound source: a set over protocol.CarrierState, one box each on the loader
// page. ONE VALUE, read from the four columns in one place (scanLoader, through
// drainStatesOf) and written back through columns; the pull asks
// helpers.CarrierStateSQL = ANY of it (Ticked), so no reader threads four
// booleans. Never on LoaderInfo: the Edge does nothing with it, and Core's pull
// (dispatch.DrainLoader) is its one reader.
//
// The empty set (nil) is every loader until a plant ticks a box, and the Go
// zero value, so a Loader built without it drains nothing.
type DrainStates []protocol.CarrierState

// Drainable is every state a box may tick, in the order the columns are
// declared and the page draws the boxes. A loaded cart is a full (the
// unloader's job already, and not a box); a bare marker is stage 2's, never
// drained (its own pull, ListBareInGroup).
var Drainable = [...]protocol.CarrierState{
	protocol.CarrierEmpty, protocol.CarrierTopsAndBottoms, protocol.CarrierLeftover, protocol.CarrierUnverified,
}

// ErrNotDrainable refuses a state no box drains.
var ErrNotDrainable = errors.New("an unloader drains empty, tops & bottoms, leftover or unverified carts")

// NewDrainStates is the set naming states, in Drainable's order and each once.
// A state no box drains (loaded, bare, or a value that is no state) is refused
// with its name (ErrNotDrainable), so a request cannot store a box the page
// does not have.
func NewDrainStates(states []protocol.CarrierState) (DrainStates, error) {
	var cols [len(Drainable)]bool
	for _, s := range states {
		i := drainableIndex(s)
		if i < 0 {
			return nil, fmt.Errorf("%w, not %q", ErrNotDrainable, s)
		}
		cols[i] = true
	}
	return drainStatesOf(cols), nil
}

// Has reports whether the set drains s.
func (d DrainStates) Has(s protocol.CarrierState) bool {
	for _, x := range d {
		if x == s {
			return true
		}
	}
	return false
}

// Ticked is the set as the text array the pull binds for
// helpers.CarrierStateSQL = ANY($n).
func (d DrainStates) Ticked() []string {
	out := make([]string, len(d))
	for i, s := range d {
		out[i] = string(s)
	}
	return out
}

// drainStatesOf is the set the four columns hold, in Drainable's order: the
// one place a row's booleans become the value.
func drainStatesOf(cols [len(Drainable)]bool) DrainStates {
	var out DrainStates
	for i, on := range cols {
		if on {
			out = append(out, Drainable[i])
		}
	}
	return out
}

// columns is the set as the four columns a write stores, in Drainable's order:
// drains_empty, drains_tops_bottoms, drains_leftover, drains_unverified.
func (d DrainStates) columns() [len(Drainable)]bool {
	var cols [len(Drainable)]bool
	for _, s := range d {
		if i := drainableIndex(s); i >= 0 {
			cols[i] = true
		}
	}
	return cols
}

func drainableIndex(s protocol.CarrierState) int {
	for i, x := range Drainable {
		if x == s {
			return i
		}
	}
	return -1
}
