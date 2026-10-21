package engine

import (
	"log"
	"slices"

	"shingo/protocol"
	"shingo/protocol/clock"
)

// HandleSourcingState persists Core's sourceability feed (SubjectSourcingState)
// into the Edge cache, so the changeover picker and other HMI screens read the
// last-known verdict from SQLite with no Core round-trip at click time.
//
// A full snapshot (report.Snapshot) replaces the whole cache — styles Core no
// longer reports drop out — even when it carries zero states (a plant with no
// styles). A change delta upserts only the styles whose verdict moved and leaves
// the rest untouched. Either is a copy of FeedSourcing arriving: once it is
// stored, the cache's digest is held with the time it came (CurrentAt reads
// it), and the heartbeat quotes that digest, so a delta that was lost shows as
// a difference and Core's compare sends the snapshot.
func (e *Engine) HandleSourcingState(report protocol.SourcingStateReport) {
	var err error
	if report.Snapshot {
		err = e.db.ReplaceSourcingState(report.States)
	} else {
		if len(report.States) == 0 {
			return
		}
		err = e.db.UpsertSourcingState(report.States)
	}
	if err != nil {
		log.Printf("sourcing_state: persist (snapshot=%v, %d states): %v",
			report.Snapshot, len(report.States), err)
		return
	}
	if d, ok := e.sourcingDigest(); ok {
		e.holdFeed(protocol.FeedSourcing, d, "")
	}
	// Every stored report is announced, the periodic full one included: a
	// station rebuilds when it draws a named process (its own, or a served
	// cell's). Nothing is read to tell whether a verdict moved. Pinned by
	// TestSourcingStateChanged_EveryReport.
	var named []string
	for _, s := range report.States {
		if !slices.Contains(named, s.ProcessID) {
			named = append(named, s.ProcessID)
		}
	}
	e.Events.Emit(Event{
		Type:      EventSourcingStateChanged,
		Timestamp: clock.Now(),
		Payload:   SourcingStateChangedEvent{Processes: named},
	})
}

// sourcingDigest is the FeedSourcing digest of this Edge's sourcing cache: one
// SQLite read, with the function Core digests the station's snapshot with. Left
// out (ok false) when the cache cannot be read: an unread cache confirms
// nothing. Unlike the refusals digest it is quoted before the first ack too, so
// the boot burst's heartbeat already names it.
func (e *Engine) sourcingDigest() (string, bool) {
	states, err := e.db.ListSourcingState()
	if err != nil {
		e.logFn("feeds: read the sourcing cache for the heartbeat: %v — left out", err)
		return "", false
	}
	d, err := protocol.SourcingDigest(states)
	if err != nil {
		e.logFn("feeds: digest the sourcing cache: %v — left out", err)
		return "", false
	}
	e.noteHeld(protocol.FeedSourcing, d)
	return d, true
}

// SourcingStateForProcess returns the cached sourceability verdicts for a
// process (keyed by its Name — the feed's process key) for the changeover
// picker. A local SQLite read (no Core round-trip). Degrades to nil on error so
// the picker still renders — a missing verdict just shows no annotation.
func (e *Engine) SourcingStateForProcess(process string) []protocol.SourcingState {
	states, err := e.db.ListSourcingStateForProcess(process)
	if err != nil {
		log.Printf("sourcing_state: read for process %q: %v", process, err)
		return nil
	}
	return states
}
