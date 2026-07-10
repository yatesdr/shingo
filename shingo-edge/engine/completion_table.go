package engine

import "shingo/protocol"

// completion_table.go — Order-completion dispatch table.
//
// The cascade replaces what was once eight hand-written
// `if e.handleX(ctx) { return }` lines in handleNodeOrderCompleted, where
// precedence was encoded purely by source-order and each handler bundled
// its own match predicate at the top. Adding a new completion handler
// meant wedging another if-return into the cascade — the structure that
// produced the 2026-04-28 cancelled-L1-fired-L2 incident.
//
// In the table form, each row carries an explicit Match predicate (the
// "is this row applicable?" question) separate from its Apply side-effects.
// The dispatcher walks completionChain top-down; the first row whose Match
// returns true gets to Apply, and the cascade stops when Apply returns
// true. loader_empty_in uses Apply's bool to signal "matched but malformed
// config — log diagnostic and fall through to normal_replenishment for
// default cleanup"; every other row's Apply returns true unconditionally
// because Match has already gated it.
//
// Adding a new completion case is now: write Match + Apply, insert a row.
// No cascade edits, no precedence-by-source-order, no precondition-gate
// wedge.

// completionCase is one row in the order-completion dispatch table.
//
// Match is the row's pure predicate — does this row's Apply apply to the
// current ctx? Side-effect-free. Multiple Match calls per cascade are
// expected to be cheap; lazy fields on orderCompletionCtx (Claim, ToClaim)
// cache expensive lookups so a Match that needs them doesn't re-query.
//
// Apply runs the row's side effects when Match returned true. The bool
// return is "did this row handle the order? if so, stop the cascade." For
// most rows the return is unconditionally true (Match is the gate, Apply
// always handles). The loader/unloader side-cycle rows use false to signal
// malformed-claim diagnostic logging + fall-through to normal_replenishment.
type completionCase struct {
	Name  string
	Match func(*orderCompletionCtx) bool
	Apply func(*Engine, *orderCompletionCtx) bool
}

// matchAlways is the gate for the terminal row (normal_replenishment),
// which is the cascade's unconditional fallback. Not used by any other row
// — every other row carries a real predicate.
func matchAlways(*orderCompletionCtx) bool { return true }

// completionChain declares the order-completion precedence. Order matters —
// the dispatcher walks top-down and the first row whose Match passes AND
// whose Apply reports "handled" wins. normal_replenishment is the terminal
// row: matchAlways always passes; applyNormalReplenishmentTerminal always
// reports handled.
//
// Precedence:
//  1. staged_delivery        — Order A → inbound staging slot
//  2. order_b_complex        — Order B (old material release), complex
//     order type, swap or evacuate situation
//  3. order_b_simple         — Order B (old material release), manual
//     move or non-swap-evacuate complex
//  4. changeover_release     — Order A → direct (non-staged) delivery
//  5. loader_empty_in        — L1 confirm fires L2 (filled-out)
//  6. manual_swap            — Move order on a manual_swap node
//  7. normal_replenishment   — terminal fallback (retrieve/complex)
//
// The unloader empty-out (U2) is NOT a completion case: it is driven by the
// operator's CLEAR tap (ClearBin → createUnloaderEmptyOut), not by a U1 retrieve
// completing — a press/forklift-fed drain has no U1. See ClearBin in operator_bin_ops.go.
//
// order_b_complex's KeepStaged branch is preserved as a no-op stub
// (handleKeepStagedOrderBCompletion) rather than a separate table row;
// the shelved-rewire-seam rationale lives in the stub's docstring.
var completionChain = []completionCase{
	{Name: "staged_delivery", Match: matchStagedDelivery, Apply: applyStagedDelivery},
	{Name: "order_b_complex", Match: matchOrderBComplex, Apply: applyOrderBComplex},
	{Name: "order_b_simple", Match: matchOrderBSimple, Apply: applyOrderBSimple},
	{Name: "changeover_release", Match: matchChangeoverRelease, Apply: applyChangeoverRelease},
	{Name: "loader_empty_in", Match: matchLoaderEmptyIn, Apply: applyLoaderEmptyIn},
	{Name: string(protocol.SwapModeManualSwap), Match: matchManualSwap, Apply: applyManualSwap},
	{Name: "normal_replenishment", Match: matchAlways, Apply: applyNormalReplenishmentTerminal},
}

// applyNormalReplenishmentTerminal adapts handleNormalReplenishment (void
// terminal handler) to the completionCase.Apply signature, returning true
// so the dispatcher exits the loop.
func applyNormalReplenishmentTerminal(e *Engine, ctx *orderCompletionCtx) bool {
	e.handleNormalReplenishment(ctx)
	return true
}
