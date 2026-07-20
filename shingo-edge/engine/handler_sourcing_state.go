package engine

import (
	"log"

	"shingo/protocol"
)

// HandleSourcingState persists Core's sourceability feed (SubjectSourcingState)
// into the Edge cache, so the changeover picker and other HMI screens read the
// last-known verdict from SQLite with no Core round-trip at click time.
//
// A full snapshot (report.Snapshot) replaces the whole cache — styles Core no
// longer reports drop out — even when it carries zero states (a plant with no
// styles). A change delta upserts only the styles whose verdict moved and leaves
// the rest untouched.
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
	}
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
