// handler_bin_return.go — Edge handler for SubjectBinReturn.
//
// Core sends it when a bin that a CANCELLED order left on a robot's deck is
// being returned to storage, has been returned, or is held for an engineer.
// NOTICE ONLY: the cancelled order is already terminal here, so nothing about
// its lifecycle moves, and no lineside count changes. The notice is stored in
// bin_returns, keyed by the cancelled order's uuid, and the orders board and
// the changeover row print one line from it.
package engine

import (
	"shingo/protocol"
	"shingoedge/store"
)

// HandleBinReturn stores Core's BinReturn notice. Best-effort — failures log
// and continue. Out-of-order arrival is handled by the store's precedence
// rule (store.BinReturnSupersedes): a late returning never replaces a final
// returned or held.
//
// An order uuid this station does not know is still stored: the row is
// display-only and costs nothing if nothing ever joins to it.
func (e *Engine) HandleBinReturn(br protocol.BinReturn) {
	wrote, err := e.db.UpsertBinReturn(store.BinReturn{
		OrderUUID:   br.OrderUUID,
		BinLabel:    br.BinLabel,
		PayloadCode: br.PayloadCode,
		State:       br.State,
		Destination: br.Destination,
		Reason:      br.Reason,
		UpdatedAt:   br.At,
	})
	if err != nil {
		e.logFn("bin_return: order=%s state=%s: %v", br.OrderUUID, br.State, err)
		return
	}
	if !wrote {
		e.logFn("bin_return: order=%s state=%s arrived after a final state; kept the stored one",
			br.OrderUUID, br.State)
	}
}
