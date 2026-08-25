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

// deckWitnessRecency bounds the gap between the reading that last showed this
// bin's deck LOADED and the reading that shows it empty. Inside that bound the
// two are a transition this process watched; outside it they are two snapshots
// with a hole between them, and a bin can leave a deck through a hole.
//
// THE RESTART RULE'S PREMISE, MADE TRUE. That rule refuses a drop Core did not
// see because it was down — but Core staying UP is not the same as Core
// watching. The fleet can go unreachable, or one AMR can roam off the WiFi, and
// robotsCache is never pruned, so a deck last read loaded sits there being
// re-read while the robot is somewhere Core cannot see. An unbounded mark says
// "at some point", and "at some point" is not a witness. (The mark only
// advances from a reading of a CONNECTED robot — see markDeckLoaded — which is
// what lets it age at all.)
//
// TWO TIMES THE CADENCE THAT ACTUALLY REFRESHES IT, and that cadence is
// `staging.sweep_interval`. sweepCarriedBins has two callers: the 2-second
// robot poll (engine_background.go:81) and the reconciliation loop
// (stranded_transit.go:664, driven from engine_lifecycle.go:165 by
// cfg.Staging.SweepInterval, 5m shipped). The poll is the fast one but it is
// not guaranteed — it sits behind a fleet-hash short-circuit
// (engine_background.go:65), so on a plant where nothing is moving the
// reconciliation loop is the only thing that refreshes the mark. The bound has
// to clear the SLOW one.
//
// The margin rule, both ways round. A bound at or below its own feeding cadence
// declines on ordinary scheduling jitter — the mark would routinely be older
// than the bound with nothing wrong — and every such decline costs an operator a
// button press on a bin the system actually knew about. A bound far above it
// widens the window in which an unwitnessed drop still reads as witnessed.
// Twice the cadence is the balance: one whole missed tick of headroom, and
// nothing legitimate sits between two ticks and the minutes-to-hours outages
// this exists to catch.
//
// DERIVED, NOT PINNED, because the cadence is configuration and the shipped
// values differ by an order of magnitude: 5m in production, 2s in
// shingocore.dev.yaml. A constant correct for one is wrong for the other.
func (e *Engine) deckWitnessRecency() time.Duration {
	if e.cfg == nil || e.cfg.Staging.SweepInterval <= 0 {
		return defaultDeckWitnessRecency
	}
	return 2 * e.cfg.Staging.SweepInterval
}

// defaultDeckWitnessRecency is twice config's shipped staging.sweep_interval,
// for the nil-config path (tests construct an Engine without one).
const defaultDeckWitnessRecency = 10 * time.Minute

// markDeckLoaded records WHEN this process last saw this bin's deck loaded.
//
// THIS IS THE WITNESS, and it is why no `Witnessed` bool is needed on the
// observation itself: the freeze only ever forms on a loaded→empty transition
// this process watched, so a carried bin whose deck reads empty with no recent
// mark is the unwitnessed case by construction.
//
// AN INSTANT AND NOT A BOOL. A bool answers "did this process ever see this
// deck loaded", and every wrong placement the rule exists to prevent can answer
// yes — Core saw the deck loaded an hour ago and has heard nothing since. The
// question worth asking is whether it saw it loaded JUST NOW, and only a
// timestamp can be asked that.
func (e *Engine) markDeckLoaded(binID int64) {
	e.dropObsMu.Lock()
	defer e.dropObsMu.Unlock()
	if e.deckSeenLoaded == nil {
		e.deckSeenLoaded = map[int64]time.Time{}
	}
	e.deckSeenLoaded[binID] = clock.Now().UTC()
}

// dropVerdict is what the watch is entitled to do with a bin whose deck reads
// empty.
//
// Four answers and not two, because the three refusals are three different
// things to tell an operator: Core was not running, Core was running but deaf,
// and Core watched the drop but never got to act on it in time. Collapsing them
// would hand the floor one sentence for three situations whose next actions
// differ.
type dropVerdict int

const (
	// dropUsable: a sample this process took, still inside the window.
	dropUsable dropVerdict = iota
	// dropUnwitnessed: no mark at all — Core restarted after the unload.
	dropUnwitnessed
	// dropGapped: a mark, but older than deckWitnessRecency — Core was up and
	// heard nothing about this robot in between.
	dropGapped
	// dropExpired: a witnessed sample that outlived the inference window
	// without a placement ever becoming possible.
	dropExpired
)

// freezeDrop returns what the watch may act on for this bin, taking the sample
// now if this is the first at-rest empty tick after a recent loaded reading.
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
//
// AN EXPIRED SAMPLE IS RETURNED, NOT DROPPED. The alternative — delete it and
// let the next tick decide afresh — is how a live reading gets promoted into an
// answer: the robot has gone back to work by then, so a re-taken sample
// resolves to whatever station it happens to be standing at and places the bin
// somewhere it was never set down. Keeping it lets the sentence say how old the
// observation is, and means the recency-bounded witness above is never asked to
// authorise a second freeze for a bin that already had one.
func (e *Engine) freezeDrop(binID int64, obs dropObservation, window time.Duration) (dropObservation, dropVerdict) {
	e.dropObsMu.Lock()
	defer e.dropObsMu.Unlock()
	if frozen, ok := e.dropObs[binID]; ok {
		if clock.Now().UTC().Sub(frozen.At) > window {
			return frozen, dropExpired
		}
		return frozen, dropUsable
	}
	seen, ok := e.deckSeenLoaded[binID]
	switch {
	case !ok:
		return dropObservation{}, dropUnwitnessed
	case obs.At.Sub(seen) > e.deckWitnessRecency():
		return dropObservation{}, dropGapped
	}
	if e.dropObs == nil {
		e.dropObs = map[int64]dropObservation{}
	}
	e.dropObs[binID] = obs
	return obs, dropUsable
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
// that is not true. It ages out on its own if nothing refreshes it.
func (e *Engine) forgetDrop(binID int64) {
	e.dropObsMu.Lock()
	delete(e.dropObs, binID)
	e.dropObsMu.Unlock()
}

// pruneDropObservations drops what no bin on a carrier node is entitled to any
// more.
//
// AGAINST THE CARRIED LIST, AND ONLY THAT. That list is the population both
// maps describe: a bin that left a deck by any route — this watch, a recovery
// order, an operator — is gone from it, so self-healing against the real
// population needs no other bookkeeping, and both maps stay bounded by the
// handful of bins riding decks.
//
// AGE IS DELIBERATELY NOT A REASON TO DELETE, and it used to be. Dropping a
// still-carried bin's sample because it was old left the WITNESS behind to
// authorise a fresh one, so the expiry did not stop the inference — it re-armed
// it with a reading taken hours after the drop, at a station the robot had
// since driven to. Age is now decided where the answer is used (freezeDrop),
// where it can be SAID instead of silently acted on.
func (e *Engine) pruneDropObservations(carried []*bins.Bin) {
	live := make(map[int64]bool, len(carried))
	for _, bin := range carried {
		live[bin.ID] = true
	}
	e.dropObsMu.Lock()
	defer e.dropObsMu.Unlock()
	for id := range e.dropObs {
		if !live[id] {
			delete(e.dropObs, id)
		}
	}
	for id := range e.deckSeenLoaded {
		if !live[id] {
			delete(e.deckSeenLoaded, id)
		}
	}
}
