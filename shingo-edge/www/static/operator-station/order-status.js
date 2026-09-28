// Order-status predicates for the operator-station HMI.
//
// Mirrors the Go-side predicates in protocol/status.go. The status
// arrays here are the SAME lex-sorted lists produced by
// protocol.TerminalStatusSQLList() etc. on the backend; a Go-side test
// (shingo-edge/www/order_status_js_drift_test.go) reads this file
// literally and asserts they match. The arrays MUST stay in lex order
// and the Go drift test will fail if they don't — adding a new status
// requires updating the protocol map AND this file together.
//
// Why not generate this from Go? Codegen adds build complexity for a
// 30-line file that changes maybe once a year. The drift test is the
// load-bearing piece: it makes silent disagreement between Go and JS
// impossible.

export const TERMINAL_STATUSES = ['cancelled', 'confirmed', 'failed', 'skipped'];

// (OPERATOR_VISIBLE_STATUSES was deleted 2026-09-26: no importer - the
// operator station asks isActive/isPreDispatch, and the Edge order-history
// window is spelled server-side in shingo-edge/store/orders/orders.go
// ListActive. Its Go twin IsOperatorVisible went with it; see the deletion
// note in protocol/status.go.)

// PRE-DISPATCH: born, waiting, and no robot committed yet. Mirrors
// protocol.IsPreDispatch / PreDispatchStatusSQLList, and the Go drift test pins
// it like the two lists above.
//
// It exists because the operator station was asking this question with literals
// — `status === 'queued' || status === 'pending'` — which silently EXCLUDED
// `sourcing`. So a station whose order was out hunting for material showed no
// demand card and no cause on the one screen a floor operator actually uses,
// which is the screen the visibility requirement is about.
export const PRE_DISPATCH_STATUSES = ['pending', 'queued', 'sourcing'];

export function isTerminal(status) {
    return TERMINAL_STATUSES.includes(status);
}

// isActive is the inverse of isTerminal. Most callers want "filter to
// the orders I still care about" which is exactly !isTerminal — this
// helper makes the intent explicit at the call site.
export function isActive(status) {
    return !isTerminal(status);
}

// isPreDispatch is "waiting, nothing is moving yet" — the demand-card question.
export function isPreDispatch(status) {
    return PRE_DISPATCH_STATUSES.includes(status);
}
