//go:build sim

package simulator_test

import (
	"testing"

	"shingocore/engine"
	"shingocore/fleet/simulator"
)

// The sim deck classifier is a copy of the engine's block classifier (an
// import cycle forbids sharing it — see deckLoadsOn). This holds the copy to
// the original over the vocabulary the original is written for, plus the
// substring fallbacks, so a change to one that is not made to the other fails
// here instead of making the sim's deck disagree with Core's bin ledger.
func TestDeckClassifierMatchesEngine(t *testing.T) {
	tasks := []string{
		"", "JackLoad", "JackUnload", "Wait", "load", "unload", "pickup", "pick",
		"dropoff", "drop", "release", "jack_load", "jack_unload", "fork_load",
		"fork_unload", "RollerLoad", "RollerUnload", "ForkLoadHigh", "PickFromRack",
		"DropToFloor", "ReleaseHold", "Move", "Charge",
	}
	for _, task := range tasks {
		if got, want := simulator.DeckLoadsOn(task), engine.IsPickupBlock(task); got != want {
			t.Errorf("load classification of %q: sim %v, engine %v", task, got, want)
		}
		if got, want := simulator.DeckEmptiesOn(task), engine.IsDropoffBlock(task); got != want {
			t.Errorf("unload classification of %q: sim %v, engine %v", task, got, want)
		}
	}
}
