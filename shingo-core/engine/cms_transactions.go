package engine

import (
	"fmt"
	"strings"

	"shingocore/material"
	"shingocore/store/audit"
	"shingocore/store/cms"
)

// cms_transactions.go — thin engine wrappers around shingocore/material.
//
// The pure boundary walk and transaction builders live in
// shingocore/material and can be exercised without an engine or a
// database. This file is the persistence-and-emission boundary: it
// calls into material, writes any returned rows via
// e.db.CreateCMSTransactions, and emits EventCMSTransaction on the
// engine event bus.
//
// Call sites:
//   - wiring.go "CMS transaction logging" subscription -> RecordMovementTransactions
//   - www/handlers_telemetry.go apiBinClear -> ClearForReuseAndBookDeparture
//
// There is deliberately no *Engine method wrapping
// material.FindCMSBoundary. The one that used to live here collapsed
// every store error to a nil node, which downstream reads as "no
// boundary here" — a transient DB failure emitted zero transactions
// with only a log line as evidence. Its only callers were tests.
// Call material.FindCMSBoundary directly and handle the error.

// RecordMovementTransactions logs CMS transactions when a bin moves
// between different CMS boundaries. The build itself is pure; this
// wrapper handles persistence and event emission.
func (e *Engine) RecordMovementTransactions(ev BinUpdatedEvent) {
	// A replay re-emits a move the ledger already booked. Emitting a second
	// source-decrement / dest-increment pair for it is a phantom transfer at
	// the plant's inventory boundary, not a duplicate audit row.
	if ev.Replay {
		return
	}
	txns, uncounted, err := material.BuildMovementTransactions(e.db, material.MovementEvent{
		BinID:      ev.BinID,
		FromNodeID: ev.FromNodeID,
		ToNodeID:   ev.ToNodeID,
		RobotID:    ev.RobotID,
		OrderID:    ev.OrderID,
	})
	if err != nil {
		// COUNTED, not just logged. This drops a real physical move off the
		// CMS ledger and leaves no row anywhere — no transaction, therefore no
		// posting — so every count on the health page would report a plant that
		// simply did not move anything. The counter is the only trace.
		e.cmsBuildFailures.Add(1)
		e.logFn("engine: cms movement build for bin %d (%d -> %d): %v — "+
			"this move will NOT reach the CMS ledger",
			ev.BinID, ev.FromNodeID, ev.ToNodeID, err)
		return
	}
	if uncounted != nil {
		// THE SAME LOSS BY A THIRD DOOR, and the quietest one. The build
		// succeeded; part of what crossed the boundary just has no ratio to
		// count it by, so those parts are booked nowhere. It reached production
		// as a manifest that named payload codes where the template keys on
		// part numbers — every partial-release bin resolving to zero, producing
		// no rows, and looking exactly like a bin that had not moved.
		//
		// IT IS ALSO THE IDENTITY CORRECTION'S OWN WINDOW. v108 rewrites what a
		// TEMPLATE line names; a bin already standing on the floor still carries
		// the value its manifest was written with, and no migration rewrites a
		// bin's jsonb under a running plant. Those bins are uncountable until
		// they next cycle — every load and every partial release re-derives the
		// manifest from the template — and this line is what makes that window
		// a number on the health page instead of a silence.
		e.cmsBuildFailures.Add(1)
		e.logFn("engine: cms movement for bin %d (payload %q): the template counts none of "+
			"%s — %s NOT reach the CMS ledger. A manifest line has to name a part the "+
			"payload's template lists; a bin loaded before the identity correction names "+
			"what the template used to say, and is counted again on its next load.",
			ev.BinID, uncounted.PayloadCode, strings.Join(uncounted.CatIDs, ", "),
			pluralWill(len(uncounted.CatIDs)))
	}
	if len(txns) == 0 {
		return
	}
	if err := e.db.CreateCMSTransactions(txns); err != nil {
		// Same loss by a different door: the rows were built and could not be
		// stored, so nothing downstream will ever see them either.
		e.cmsBuildFailures.Add(1)
		e.logFn("engine: cms transactions for bin %d: %v — "+
			"%d rows will NOT reach the CMS ledger", ev.BinID, err, len(txns))
		return
	}
	e.Events.Emit(Event{Type: EventCMSTransaction, Payload: CMSTransactionEvent{Transactions: txns}})
}

// ClearForReuseAndBookDeparture clears a bin's manifest and, when the node it
// stands on resolves to a CMS boundary, books the departure of its contents in
// the SAME database transaction. Returns the new delta_epoch, as
// BinManifestService.ClearForReuse does.
//
// THIS IS THE UNLOADER'S DOOR. At Hopkinsville the unloader takes bins out of
// the AMR supermarket by clearing them and moving the material to another CMS
// zone, so the clear IS the departure. Before this, the arrival posted and the
// departure did not, and the storeroom climbed forever.
//
// ONE TRANSACTION, AND THE ORDER INSIDE IT IS THE POINT. The clear destroys the
// uop_remaining and the manifest the quantities are derived from. Rows written
// after it cannot be reconstructed if their write fails; rows committed before
// it become a departure the plant never made if the clear then fails. Committing
// both together is the only arrangement with neither failure — and a clear the
// unloader retries is recoverable, while a clear that silently drops inventory
// is not.
//
// NOT the UI's binClear (www/bin_actions.go). That is the admin door an operator
// reaches for to REPAIR a wrong record, and booking a repair as an inventory
// movement writes fiction into a ledger. It stays silent, deliberately.
func (e *Engine) ClearForReuseAndBookDeparture(binID, nodeID int64, binTypeID *int64) (int64, error) {
	txns := e.buildClearDeparture(binID, nodeID)
	if len(txns) == 0 {
		// THREE DIFFERENT REASONS ARRIVE HERE and all three mean the same thing
		// about the clear: nothing is owed to CMS, so it owns its own
		// transaction exactly as before. The node resolves to no boundary (an
		// untagged clear is invisible to CMS and that is correct); the bin is
		// drained or bare; or the build FAILED, which buildClearDeparture has
		// already counted and named.
		return e.binManifest.ClearForReuse(binID, binTypeID)
	}

	tx, err := e.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin clear-and-book tx for bin %d: %w", binID, err)
	}
	defer tx.Rollback()
	if err := cms.CreateInTx(tx, txns); err != nil {
		return 0, fmt.Errorf("book cms departure of bin %d: %w — the clear is REFUSED with it, "+
			"because clearing the bin destroys the counts these rows carry", binID, err)
	}
	epoch, err := e.binManifest.ClearForReuseTx(tx, binID, binTypeID, audit.OpClearForReuse,
		"engine/cms_transactions.go:ClearForReuseAndBookDeparture")
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit clear-and-book for bin %d: %w", binID, err)
	}

	// AFTER THE COMMIT, never before. The event is what turns these rows into a
	// posting, and a posting for rows a rollback removed would send CMS a
	// departure that did not happen.
	e.Events.Emit(Event{Type: EventCMSTransaction, Payload: CMSTransactionEvent{Transactions: txns}})
	return epoch, nil
}

// buildClearDeparture returns the rows a clear owes CMS, and is the loud half:
// nothing here refuses the clear, and everything it drops is counted and named.
//
// WHY A BUILD FAILURE DOES NOT BLOCK THE DOOR. The tempting reading is that a
// clear whose quantities could not be computed should be refused, since the
// clear destroys the evidence. It must not be, and the reason is which failures
// actually reach here: an unparseable manifest, a payload template whose lines
// name no part, a cycle in the node tree. Each of those makes the bin
// PERMANENTLY unclearable, which takes the operator's repair path away — the
// same trap as HK 2026-07-28, where the instinctive fix was the one action that
// guaranteed no recovery. A broken inventory feed must not become a brake on the
// plant; the docs' own rule is that the backlog is a number on the diagnostics
// page. So the loss is counted into cmsBuildFailures, which RANKS FIRST on the
// health verdict, above every other count.
//
// AND THE LOG LINE CARRIES THE QUANTITY'S INPUTS, which the movement path does
// not need to. There, the bin still stands with its contents and can be
// re-counted; here the clear is about to destroy them, so the payload and
// uop_remaining are named while they still exist and the loss stays
// reconstructable by a person.
func (e *Engine) buildClearDeparture(binID, nodeID int64) []*cms.Transaction {
	txns, uncounted, err := material.BuildClearTransactions(e.db, material.ClearEvent{
		BinID:  binID,
		NodeID: nodeID,
	})
	if err != nil {
		e.cmsBuildFailures.Add(1)
		e.logFn("engine: cms clear build for bin %d at node %d: %v — "+
			"this departure will NOT reach the CMS ledger, and the clear DESTROYS "+
			"what it would have been counted from (%s)",
			binID, nodeID, err, e.binContentsForLog(binID))
		return nil
	}
	if uncounted != nil {
		// THE SAME LOSS BY THE QUIETEST DOOR. The build succeeded; part of what
		// left the storeroom has no ratio to count it by, so those parts are
		// booked nowhere. A manifest line has to name a part the payload's
		// template lists — a bin loaded before the identity correction names
		// what the template used to say, and is counted again on its next load,
		// except that a clear is the end of this bin's load and there is no next
		// one for what was in it.
		e.cmsBuildFailures.Add(1)
		e.logFn("engine: cms clear for bin %d (payload %q): the template counts none of "+
			"%s — %s NOT reach the CMS ledger. The clear destroys the count, so this "+
			"one does not come back on the bin's next load.",
			binID, uncounted.PayloadCode, strings.Join(uncounted.CatIDs, ", "),
			pluralWill(len(uncounted.CatIDs)))
	}
	return txns
}

// binContentsForLog names what a bin holds, for a failure line written while the
// contents still exist. Best-effort by design: the caller is already on an error
// path, and a second failure here must not replace the first one's message.
func (e *Engine) binContentsForLog(binID int64) string {
	bin, err := e.db.GetBin(binID)
	if err != nil || bin == nil {
		return "bin contents unreadable"
	}
	manifest := ""
	if bin.Manifest != nil {
		manifest = *bin.Manifest
	}
	return fmt.Sprintf("payload=%q uop_remaining=%d manifest=%s", bin.PayloadCode, bin.UOPRemaining, manifest)
}

// pluralWill agrees the verb with the number of uncounted parts, so the log
// line reads as a sentence in both the one-part and many-part cases.
func pluralWill(n int) string {
	if n == 1 {
		return "that part will"
	}
	return "those parts will"
}
