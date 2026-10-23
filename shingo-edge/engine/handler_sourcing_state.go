package engine

import (
	"errors"
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
// pickers and the start, and whether the copy they come from is current:
// CurrentAt over the feed's arrival and Core's last confirmation, the rule
// every screen applies to a held copy. A verdict from a copy that is not
// current says nothing about stock now ("stock not checked"). A local SQLite
// read (no Core round-trip). Degrades to nil on error so the picker still
// renders — a missing verdict just shows no annotation.
func (e *Engine) SourcingStateForProcess(process string) ([]protocol.SourcingState, bool) {
	received, confirmed := e.FeedTimes(protocol.FeedSourcing)
	_, old := CurrentAt(received, confirmed, e.feeds.clock())
	states, err := e.db.ListSourcingStateForProcess(process)
	if err != nil {
		log.Printf("sourcing_state: read for process %q: %v", process, err)
		return nil, !old
	}
	return states, !old
}

// changeoverStockAdvisory is the changeover start's stock gate, and it never
// refuses (since 2026-06-04): a changeover started without stock queues its
// supply legs as Awaiting Stock and self-heals once the bins are loaded, where
// a refusal dead-ended the operator with idle robots (Springfield NF SPOT 3,
// 2026-06-03). Core's pair rule parks both legs of a pair together on the
// supply's material wait, so a stockless changeover queues its pairs rather
// than half-dispatching them. The steady-state door queues the same way: a
// request into a source Core reports dry arms its pair, which parks on
// waiting_for_material until stock lands (requestNodeFromClaim).
//
// It reads the sourcing feed's verdict for the to-style, held here, with no
// round trip; the start used to ask Core and refused when Core did not
// answer. Missing payloads are logged as the advisory. No verdict for the
// style, or a copy past CurrentAt, is "stock not checked", logged, and the
// start proceeds: the pickers draw the same words, and the feed's next verdict
// redraws them.
func (e *Engine) changeoverStockAdvisory(processID, toStyleID int64) {
	process, perr := e.db.GetProcess(processID)
	style, serr := e.db.GetStyle(toStyleID)
	if perr != nil || serr != nil {
		e.logFn("changeover: process %d → style %d starting with stock not checked: %v",
			processID, toStyleID, errors.Join(perr, serr))
		return
	}
	states, current := e.SourcingStateForProcess(process.Name)
	i := slices.IndexFunc(states, func(s protocol.SourcingState) bool { return s.StyleID == style.Name })
	switch {
	case i < 0 || !current:
		e.logFn("changeover: process %d → style %d starting with stock not checked (verdict held %v, copy current %v)",
			processID, toStyleID, i >= 0, current)
	case len(states[i].Missing) > 0:
		e.logFn("changeover: process %d → style %d starting with %d payload(s) not yet in stock; supply legs will queue as Awaiting Stock until loaded: %v",
			processID, toStyleID, len(states[i].Missing), states[i].Missing)
	}
}
