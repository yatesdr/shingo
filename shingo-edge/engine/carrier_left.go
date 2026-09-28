// carrier_left.go — the one verb for "the carrier left this slot".

package engine

import (
	"shingoedge/domain"
)

// carrierLeft is THE verb for a carrier leaving a slot: the pointer and the
// count go (one statement, uop.ClearActiveBin), and the identity goes with
// them through the lineside doorway, recorded as a departure. Every path that
// nulls active_bin_id comes through here — the bin pickup, the admin-Move
// release announcement, the cancel reconcile's confirmed-empty arm, Order B's
// completion, and the produce finalize/ingest resets — so none of them can
// clear the pointer and leave the previous occupant's identity standing for
// the next one to inherit (TestArch_CarrierLeavesThroughOneVerb).
//
// ── WHY IT LIVES HERE AND NOT ON uop.Mutator ─────────────────────────────
//
// The doorway (recordLinesideCarrier) is an engine method, and it has to be
// the one used: it is the single writer of the identity and names who said
// it. uop cannot call it — engine imports uop, so the call would be a cycle —
// and handing the Mutator an engine callback would hide the doorway behind
// wiring. So the Mutator keeps the pointer/count half, and the verb that
// composes both halves sits beside the doorway.
//
// ── TWO STATEMENTS, IN THIS ORDER ────────────────────────────────────────
//
// The pointer and count first, then the identity. A reader landing between
// the two sees an empty slot still carrying the departed carrier's identity
// for one statement — the same window the pickup always had before the
// identity briefly rode the store's pointer statement. The identity is
// cleared even when the pointer write failed: the carrier physically left,
// and a held-over identity is a confident wrong answer about the next one.
//
// nil sink (tests and off-modes): nothing is written, pointer or identity,
// matching the nil-guard every caller of the sink had before.
func (e *Engine) carrierLeft(nodeID int64, nodeName string) error {
	if e.inventoryDelta == nil {
		return nil
	}
	err := e.inventoryDelta.ClearActiveBin(nodeID)
	e.recordLinesideCarrier(nodeID, nodeName, domain.UnknownCarrier(), domain.CarrierDeparted)
	return err
}

// carrierClearedInPlace records a Core clear-for-reuse of the carrier bound at
// a slot that the carrier does NOT leave: the count and the new generation
// stamp land (uop.SetClaimCountAndEpoch), and the carrier is known-empty, said
// by the operator whose clear it was — the shape operator_bin_ops.go's CLEAR
// records. Used by ClearLoaderHome and the market-pullback auto-clear. Not a
// departure: the pointer stays, and the identity is an answer, not an absence.
func (e *Engine) carrierClearedInPlace(nodeID int64, nodeName string, claimID *int64, binID, deltaEpoch int64) error {
	if e.inventoryDelta == nil {
		return nil
	}
	err := e.inventoryDelta.SetClaimCountAndEpoch(nodeID, claimID, 0, binID, deltaEpoch)
	e.recordLinesideCarrier(nodeID, nodeName, domain.KnownCarrier(""), domain.CarrierFromOperator)
	return err
}
