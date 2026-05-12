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

// Operator-visible statuses on edge HMI surfaces. Failed stays visible
// so the operator can retry/acknowledge; confirmed/cancelled/skipped
// are "done from the operator's POV" and disappear.
export const OPERATOR_VISIBLE_STATUSES = [
    'acknowledged', 'delivered', 'dispatched', 'failed', 'faulted',
    'in_transit', 'pending', 'queued', 'reshuffling', 'sourcing',
    'staged', 'submitted',
];

export function isTerminal(status) {
    return TERMINAL_STATUSES.includes(status);
}

// isActive is the inverse of isTerminal. Most callers want "filter to
// the orders I still care about" which is exactly !isTerminal — this
// helper makes the intent explicit at the call site.
export function isActive(status) {
    return !isTerminal(status);
}

export function isOperatorVisible(status) {
    return OPERATOR_VISIBLE_STATUSES.includes(status);
}
