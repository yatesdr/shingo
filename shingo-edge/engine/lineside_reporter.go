package engine

import (
	"fmt"
	"log"
	"time"

	"shingo/protocol"
	"shingoedge/uop"
)

// Lineside reporter (Edge side): a per-carrier checksum.
//
// Every 60s the Edge sends Core, per consuming node, which carrier it has bound
// (bin id and generation), that carrier's count, the node's bucket for the
// carrier's part, and the last delta seq it allocated for the carrier. Core
// compares each row against its own replica on ingest and records a
// disagreement as a report_divergence episode. The report DECIDES NOTHING: every
// replenishment decision reads Core's count (seat-count round 1, 2026-09-23).
// It decided under lineside_decision_mode=edge_reports from 2026-07-24 until
// then; the knob is deleted, and rolling back is the previous build.
//
// EVERY SEAT, EVERY INTERVAL (owner ruling, 2026-09-24: "the edges should
// always report even if nothing is happening"). Each consume seat the running
// styles claim is a row; a seat with nothing bound is a row that says so (no
// carrier, 0, 0). A station with no consume seat sends a report with no rows,
// which tells Core it runs none right now. So Core closes an episode when its
// seat reports empty, and sees a carrier it places at a seat the Edge has
// nothing bound at, from the report alone.
//
// A report that does not arrive changes no decision; it only leaves Core's
// comparison for this station where the last report put it.

// linesideReportInterval is the reporter cadence.
const linesideReportInterval = 60 * time.Second

// startLinesideReporter spawns the lineside reporter goroutine.
func (e *Engine) startLinesideReporter() {
	go e.runLinesideReporter()
}

func (e *Engine) runLinesideReporter() {
	ticker := time.NewTicker(linesideReportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopChan:
			return
		case <-ticker.C:
			e.reportLinesideLevels()
		}
	}
}

func (e *Engine) reportLinesideLevels() {
	// AS OF FLUSHEDSEQ, AT NO COST. A tick moves the runtime row's count at
	// once and reaches the outbox up to one accumulator interval later, so a
	// raw count would show Core a gap its FlushedSeq says was sent: a
	// divergence on every running seat. Each row instead states its count
	// minus what the accumulator holds unflushed for that carrier (and its
	// bucket minus what is unflushed for that seat and part), which is the
	// number Core holds once it has applied every seq up to FlushedSeq.
	//
	// ONE INSTANT. The unflushed snapshot, the level SELECT (counts and
	// FlushedSeq) and the report's enqueue happen together: under the
	// accumulator's flush lock (WithPending), so no flush lands between them
	// and every delta the counts include is ahead of the report in the outbox;
	// and under countMu, which every path that writes a seat's count and
	// records the change holds across both, so no tick lands between them
	// either. No flush, no statement and no message is added: the same SELECT
	// and the same snapshot enqueue as before, a few milliseconds under lock.
	e.countMu.Lock()
	defer e.countMu.Unlock()
	if e.inventoryDelta == nil {
		if err := e.buildAndEnqueueLinesideReport(uop.Pending{}); err != nil {
			log.Printf("lineside-reporter: %v", err)
		}
		return
	}
	if err := e.inventoryDelta.WithPending(e.buildAndEnqueueLinesideReport); err != nil {
		log.Printf("lineside-reporter: %v", err)
	}
}

// buildAndEnqueueLinesideReport reads the levels, states each as of its
// FlushedSeq, and enqueues the report. Runs under countMu and, when there is an
// accumulator, its flush lock.
func (e *Engine) buildAndEnqueueLinesideReport(pending uop.Pending) error {
	levels, err := e.db.ListLinesideLevels()
	if err != nil {
		return fmt.Errorf("list levels: %w", err)
	}

	station := e.cfg.StationID()
	entries := make([]protocol.LinesideLevelEntry, 0, len(levels))
	var unknown []string
	for _, l := range levels {
		// A CARRIER NOBODY COULD IDENTIFY SHIPS NO ROW.
		//
		// There is no safe guess to make here. The old one — fall back to the
		// claim — is what put a part number of which zero existed plant-wide on
		// a carrier holding 7032 of something else, and because the payload is
		// the join key on Core it took the real part's correction down with it.
		//
		// Absence is the designed degradation. Core sees the seat with no bound
		// carrier, so a carrier Core has placed there opens an unbound_carrier
		// episode — which is true: nobody has identified what the Edge has bound.
		//
		// An EMPTY seat is not this case: nothing is there to identify, so it
		// ships as a row with no part. A carrier known to be empty ships too,
		// bound, with no part.
		if !l.PayloadKnown && (l.BinID != nil || l.BucketQty > 0) {
			unknown = append(unknown, l.CoreNodeName)
			continue
		}
		payload := l.PayloadCode
		if !l.PayloadKnown {
			payload = ""
		}
		binUOP := l.BinUOP
		if l.BinID != nil {
			binUOP -= pending.Bin(*l.BinID, l.BinEpoch)
		}
		entries = append(entries, protocol.LinesideLevelEntry{
			CoreNodeName: l.CoreNodeName,
			PayloadCode:  payload,
			BinCount:     l.BinCount,
			BinUOP:       binUOP,
			BucketQty:    l.BucketQty - pending.Bucket(l.NodeID, payload),
			BinID:        l.BinID,
			BinEpoch:     l.BinEpoch,
			FlushedSeq:   l.FlushedSeq,
		})
	}
	// Loud, because a node dropping out of the report is a seat Core can no
	// longer check, and the operator-visible symptom is nothing at all. A node
	// that stays here across many reports has a carrier no delivery envelope and
	// no person has ever identified.
	if len(unknown) > 0 {
		log.Printf("lineside-reporter: %d node(s) withheld — no established carrier identity: %v",
			len(unknown), unknown)
	}

	env, err := protocol.NewDataEnvelope(
		protocol.SubjectLinesideLevelReport,
		protocol.Address{Role: protocol.RoleEdge, Station: station},
		protocol.Address{Role: protocol.RoleCore},
		&protocol.LinesideLevelReport{
			Station:    station,
			ReportedAt: time.Now().UTC(),
			Entries:    entries,
		},
	)
	if err != nil {
		return fmt.Errorf("build envelope: %w", err)
	}
	data, err := env.Encode()
	if err != nil {
		return fmt.Errorf("encode envelope: %w", err)
	}
	// Snapshot enqueue: this report carries EVERY consuming node, so an unsent
	// predecessor is worthless. Without this, an outage leaves an hour of
	// superseded reports to publish in a burst on recovery — and Core discards
	// most of them for expiry on arrival anyway.
	if err := e.db.EnqueueSnapshotOutbox([][]byte{data}, protocol.SubjectLinesideLevelReport); err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	return nil
}
