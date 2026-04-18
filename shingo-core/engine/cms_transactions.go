package engine

import (
	"shingocore/material"
	"shingocore/store"
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
// Call sites (unchanged — preserved so Stage 6 edits zero callers):
//   - wiring.go "CMS transaction logging" subscription -> RecordMovementTransactions
//   - corrections.go ApplyBatchCorrection -> RecordCorrectionTransactions

// FindCMSBoundary delegates to material.FindCMSBoundary. Errors
// (cycle detection, store lookup failures) are logged and collapsed
// back to nil so the method keeps the single-return contract the
// rest of the engine expects.
func (e *Engine) FindCMSBoundary(nodeID int64) *store.Node {
	node, err := material.FindCMSBoundary(e.db, nodeID)
	if err != nil {
		e.logFn("engine: cms boundary walk from node %d: %v", nodeID, err)
		return nil
	}
	return node
}

// RecordMovementTransactions logs CMS transactions when a bin moves
// between different CMS boundaries. The build itself is pure; this
// wrapper handles persistence and event emission.
func (e *Engine) RecordMovementTransactions(ev BinUpdatedEvent) {
	txns, err := material.BuildMovementTransactions(e.db, material.MovementEvent{
		BinID:      ev.BinID,
		FromNodeID: ev.FromNodeID,
		ToNodeID:   ev.ToNodeID,
	})
	if err != nil {
		e.logFn("engine: cms movement build: %v", err)
		return
	}
	if len(txns) == 0 {
		return
	}
	if err := e.db.CreateCMSTransactions(txns); err != nil {
		e.logFn("engine: cms transactions: %v", err)
		return
	}
	e.Events.Emit(Event{Type: EventCMSTransaction, Payload: CMSTransactionEvent{Transactions: txns}})
}

// RecordCorrectionTransactions logs CMS adjustment transactions when
// a bin's manifest is edited. Persistence and emission are the only
// concerns that live here; the diff itself is done in material.
func (e *Engine) RecordCorrectionTransactions(binID, nodeID int64, oldManifest, newManifest []store.ManifestEntry, reason string) {
	txns, err := material.BuildCorrectionTransactions(e.db, binID, nodeID, oldManifest, newManifest, reason)
	if err != nil {
		e.logFn("engine: cms correction build: %v", err)
		return
	}
	if len(txns) == 0 {
		return
	}
	if err := e.db.CreateCMSTransactions(txns); err != nil {
		e.logFn("engine: cms correction transactions: %v", err)
		return
	}
	e.Events.Emit(Event{Type: EventCMSTransaction, Payload: CMSTransactionEvent{Transactions: txns}})
}
