package engine

import "shingocore/material"

// cms_transactions.go — thin engine wrappers around shingocore/material.
//
// The pure boundary walk and transaction builders live in
// shingocore/material and can be exercised without an engine or a
// database. This file is the persistence-and-emission boundary: it
// calls into material, writes any returned rows via
// e.db.CreateCMSTransactions, and emits EventCMSTransaction on the
// engine event bus.
//
// Call site:
//   - wiring.go "CMS transaction logging" subscription -> RecordMovementTransactions
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
	txns, err := material.BuildMovementTransactions(e.db, material.MovementEvent{
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
