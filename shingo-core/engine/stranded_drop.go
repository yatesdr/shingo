// stranded_drop.go — the one reading that describes where the bin was set down.
//
// The deck is sampled, not evented (there is no jack-unload notification), so
// "where did it go" is answered by whatever the robot reported on the tick the
// deck first read empty. That reading has a shelf life measured in seconds: on
// 2026-08-24 bin 5's deck emptied at AP102 — Core's own SMN_007 — and the
// station field held that value for 19 two-second ticks before decaying through
// LM100, LM7, LM8, LM9 and finally PP95, a park point 12.3 m away, where it sat
// for the remaining 50. Every one of those 69 ticks wrote the bin's anomaly
// note, so the note an operator eventually read pointed at the wrong end of the
// aisle.
//
// So the sample is FROZEN on the first at-rest empty tick and re-resolved
// afterwards, rather than re-sampled. Freezing the raw reading and not the
// resolution is deliberate twice over: a resolution that failed would cement a
// failure a later scene sync could have fixed, and a resolved node can be
// renamed or deleted by the next fleet-node sync while the reading cannot.
// The observation cannot be re-taken; the resolution can be re-run for free.

package engine

import (
	"time"

	"shingo/protocol/clock"
	"shingocore/fleet"
	"shingocore/store/bins"
)

// dropObservation is the RAW sample: what the robot reported, not what Core
// made of it.
//
// At is CORE'S poll time, never the robot's. Robot clocks at Springfield are
// off by days (FINDING-seer-jackunload-vs-block-completion-2026-08-12.md), and
// this instant is shown to an operator as when the bin was set down.
//
// LiftHeight rides along with JackState because the anomaly note renders both,
// and a note that printed a frozen station beside a live height would be two
// different moments in one sentence.
type dropObservation struct {
	RobotID        string
	CurrentStation string
	LastStation    string
	X, Y, Angle    float64
	JackState      int
	LiftHeight     float64
	At             time.Time
}

// observeDrop takes the sample. Nothing is interpreted here.
func observeDrop(robotID string, r fleet.RobotStatus, at time.Time) dropObservation {
	return dropObservation{
		RobotID:        robotID,
		CurrentStation: r.CurrentStation,
		LastStation:    r.LastStation,
		X:              r.X,
		Y:              r.Y,
		Angle:          r.Angle,
		JackState:      r.JackState,
		LiftHeight:     r.LiftHeight,
		At:             at,
	}
}

// status rebuilds the fields strandedNote renders, so the note and the audit
// speak from the frozen sample without every renderer growing a second
// signature.
func (d dropObservation) status() fleet.RobotStatus {
	return fleet.RobotStatus{
		VehicleID:      d.RobotID,
		CurrentStation: d.CurrentStation,
		LastStation:    d.LastStation,
		X:              d.X,
		Y:              d.Y,
		Angle:          d.Angle,
		JackState:      d.JackState,
		LiftHeight:     d.LiftHeight,
	}
}

// markDeckLoaded records that this process has seen this bin's deck loaded.
//
// THIS IS THE WITNESS, and it is why no `Witnessed` bool is needed on the
// observation itself: the freeze only ever forms on a loaded→empty transition
// this process watched, so a carried bin whose deck reads empty with no mark is
// the unwitnessed case by construction.
func (e *Engine) markDeckLoaded(binID int64) {
	e.dropObsMu.Lock()
	defer e.dropObsMu.Unlock()
	if e.deckSeenLoaded == nil {
		e.deckSeenLoaded = map[int64]bool{}
	}
	e.deckSeenLoaded[binID] = true
}

// freezeDrop returns the frozen sample for a bin, taking it now if this is the
// first at-rest empty tick. ok is false when the transition was never
// witnessed.
//
// UNWITNESSED MEANS ANOMALY — there is no agreement-with-intent fallback, and
// that is a decision rather than an omission. The freeze is in-memory and dies
// with Core, so a bin found already on a carrier node with an already-empty
// deck may have been unloaded at any point while Core was down. In that window
// an operator may have physically taken the bin off the deck, so the
// deck-plus-position reading says nothing about where the bin is. Placing it
// only when the reading AGREES with the cancelled order's destination sounds
// safe and is not: the robot parks at the destination station often enough that
// both sources agree while a human carried the bin somewhere else, and the
// agreement then reads as corroboration. The operator button (the bins page's
// "Ask AMR-09 to set it down", and RecoverTransitAnomaly for a bin already off
// the deck) is the designed exit.
func (e *Engine) freezeDrop(binID int64, obs dropObservation) (dropObservation, bool) {
	e.dropObsMu.Lock()
	defer e.dropObsMu.Unlock()
	if frozen, ok := e.dropObs[binID]; ok {
		return frozen, true
	}
	if !e.deckSeenLoaded[binID] {
		return dropObservation{}, false
	}
	if e.dropObs == nil {
		e.dropObs = map[int64]dropObservation{}
	}
	e.dropObs[binID] = obs
	return obs, true
}

// forgetDrop discards a bin's frozen sample.
//
// Called when the bin is placed, and when a recovery order takes over the
// question — that order names a destination somebody chose and the ordinary
// arrival path records where the bin actually landed, so an older guess must
// not survive to compete with it.
//
// The seen-loaded mark is deliberately NOT dropped here: a recovery order that
// fails leaves the bin riding the same deck, and clearing the witness would
// make the next empty-deck tick look like a restart and decline with a reason
// that is not true.
func (e *Engine) forgetDrop(binID int64) {
	e.dropObsMu.Lock()
	delete(e.dropObs, binID)
	e.dropObsMu.Unlock()
}

// pruneDropObservations drops what no bin on a carrier node is entitled to any
// more.
//
// Against the CARRIED LIST rather than by expiry alone, because that list is
// the population both maps describe: a bin that left a deck by any route — this
// watch, a recovery order, an operator — is gone from it, and self-healing
// against the real population needs no other bookkeeping. Expiry still applies
// on top for a bin that rides a deck longer than the inference window.
func (e *Engine) pruneDropObservations(carried []*bins.Bin, window time.Duration) {
	live := make(map[int64]bool, len(carried))
	for _, bin := range carried {
		live[bin.ID] = true
	}
	now := clock.Now().UTC()
	e.dropObsMu.Lock()
	defer e.dropObsMu.Unlock()
	for id, obs := range e.dropObs {
		if !live[id] || now.Sub(obs.At) > window {
			delete(e.dropObs, id)
		}
	}
	for id := range e.deckSeenLoaded {
		if !live[id] {
			delete(e.deckSeenLoaded, id)
		}
	}
}
