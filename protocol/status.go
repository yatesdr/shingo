package protocol

import (
	"database/sql/driver"
	"sort"
	"strings"
)

// Status is the typed canonical order status. Wraps string so it serializes
// natively over JSON / SQL while gaining compile-time distinction from raw
// strings and other enum-shaped string types (e.g. bin status, ack outcome).
type Status string

// Canonical order status constants shared by core and edge.
const (
	StatusPending      Status = "pending"
	StatusSourcing     Status = "sourcing"
	StatusQueued       Status = "queued"
	StatusSubmitted    Status = "submitted"
	StatusDispatched   Status = "dispatched"
	StatusAcknowledged Status = "acknowledged"
	StatusInTransit    Status = "in_transit"
	StatusDelivered    Status = "delivered"
	StatusConfirmed    Status = "confirmed"
	StatusStaged       Status = "staged"
	StatusFaulted      Status = "faulted"
	StatusFailed       Status = "failed"
	StatusCancelled    Status = "cancelled"
	StatusReshuffling  Status = "reshuffling"
	// StatusSkipped is the "the work was never needed" terminal status —
	// distinct from Failed (work attempted and errored) and Cancelled (work
	// aborted by external decision). Today its sole producer is
	// DispatchPreparedComplex: when ApplyComplexPlan finds zero bins at every
	// pickup node (the source was emptied externally — quality hold, manual
	// removal, etc.), the order moves to Skipped instead of Failed so the
	// operator-facing surface treats it as a no-op rather than an alarm.
	StatusSkipped Status = "skipped"
)

// IsTerminal reports whether the status has no outgoing transitions.
// Delegates to the package-level function to stay single-source-of-truth.
func (s Status) IsTerminal() bool {
	return IsTerminal(s)
}

// CanTransitionTo reports whether the (s, to) transition is allowed by the
// canonical state machine.
func (s Status) CanTransitionTo(to Status) bool {
	return IsValidTransition(s, to)
}

// String satisfies fmt.Stringer; convenient for log lines and debug output.
func (s Status) String() string {
	return string(s)
}

// Scan implements sql.Scanner for reading from a database column. Accepts
// string or []byte (both are common across drivers); NULL becomes the empty
// Status. Does not validate the value against AllStatuses() — historical rows
// from retired statuses must still load. Validation belongs at write time
// via DB CHECK constraints (deferred work) or at the LifecycleService.
func (s *Status) Scan(v any) error {
	return ScanEnumNamed(s, v, "protocol.Status.Scan")
}

// Value implements driver.Valuer for writing to a database column.
func (s Status) Value() (driver.Value, error) {
	return ValueEnum(s)
}

// AllStatuses returns every status defined in this module, used by
// table-driven tests that exhaustively cover the (from, to) matrix.
func AllStatuses() []Status {
	return []Status{
		StatusPending, StatusSourcing, StatusQueued, StatusSubmitted,
		StatusDispatched, StatusAcknowledged, StatusInTransit, StatusStaged,
		StatusDelivered, StatusConfirmed, StatusFaulted, StatusFailed, StatusCancelled,
		StatusReshuffling, StatusSkipped,
	}
}

// ─── Status set predicates ────────────────────────────────────────────────
//
// These predicates classify statuses by *intent*. Callers across the
// codebase (SQL filters, Go-side branches, dashboard filters) ask one
// of these predicates rather than hand-enumerating status lists. When a
// new status is added, the author has to consciously classify it into
// each predicate — the drift-detection tests in status_test.go force
// that decision.
//
// Layering note: IsTerminal lives in types.go because it derives from
// the state-machine table. The other predicates live here because they
// express application semantics (operator visibility, vendor lifecycle,
// dispatcher lifecycle) that overlap with terminality but aren't
// derivable from the transition graph alone.

// IsFailureTerminal reports whether the status represents an unsuccessful
// terminal outcome — work was attempted or aborted and ended badly.
// Excludes Confirmed (successful completion) and Skipped (deliberate
// no-op, the work was never needed). Used by anomaly detection that
// distinguishes "operator should look at this" from "this is done."
func IsFailureTerminal(s Status) bool {
	return s == StatusCancelled || s == StatusFailed
}

// IsFailureTerminal is the method form; delegates to the package function.
func (s Status) IsFailureTerminal() bool { return IsFailureTerminal(s) }

// IsVendorActive reports whether the fleet vendor has the order and is
// actively working on it. The order has crossed from Core's planning
// space into the floor-execution space. Used by adapter-poll filters
// and capacity gates that key on "is the robot touching this right now."
func IsVendorActive(s Status) bool {
	return s == StatusDispatched || s == StatusInTransit || s == StatusStaged
}

// IsVendorActive is the method form.
func (s Status) IsVendorActive() bool { return IsVendorActive(s) }

// IsPreDispatch reports whether the order is still in Core's planning
// space and has not yet been sent to the fleet vendor. Used by
// source-reference guards that need to know "would re-parenting this
// node break a not-yet-dispatched order."
func IsPreDispatch(s Status) bool {
	return s == StatusPending || s == StatusSourcing || s == StatusQueued
}

// IsPreDispatch is the method form.
func (s Status) IsPreDispatch() bool { return IsPreDispatch(s) }

// IsAcquiring reports whether the order is actively trying to acquire its source
// bin — either queued (waiting for the fulfillment scanner to pick it up) or
// sourcing (mid-reserve; the scanner retries it once commit 4 moves
// MoveToSourcing to the start of the reserve attempt). The scanner's scan set and
// re-check, and DispatchPreparedComplex's entry guard, all key on this so
// "retryable pre-dispatch state" has one definition. Narrower than IsPreDispatch,
// which also includes `pending` (pre-intake, not yet a scanner-retry candidate).
func IsAcquiring(s Status) bool {
	return s == StatusQueued || s == StatusSourcing
}

// IsAcquiring is the method form.
func (s Status) IsAcquiring() bool { return IsAcquiring(s) }

// IsRuntimeStuckCandidate reports whether an order whose updated_at is
// far in the past should be flagged as runtime-stuck. Excludes Faulted
// (intentional grace-period non-terminal), Delivered (waits for operator
// confirmation, no machine deadline), and Reshuffling (compound parent
// waiting on children — has its own watchdog). Used by reconciliation's
// stuck-order detector.
func IsRuntimeStuckCandidate(s Status) bool {
	switch s {
	case StatusPending, StatusSourcing, StatusSubmitted, StatusAcknowledged,
		StatusDispatched, StatusInTransit, StatusStaged:
		return true
	}
	return false
}

// IsRuntimeStuckCandidate is the method form.
func (s Status) IsRuntimeStuckCandidate() bool { return IsRuntimeStuckCandidate(s) }

// IsOperatorVisible reports whether the status should still appear on
// operator-facing HMI surfaces (edge ListActive, kanban demand pages,
// manual-message picker). Distinct from !IsTerminal: Failed orders stay
// operator-visible so the operator can retry or acknowledge them, even
// though they're terminal. Skipped/Confirmed/Cancelled are "done from
// the operator's POV" and disappear from the surface.
func IsOperatorVisible(s Status) bool {
	return s != StatusConfirmed && s != StatusCancelled && s != StatusSkipped
}

// IsOperatorVisible is the method form.
func (s Status) IsOperatorVisible() bool { return IsOperatorVisible(s) }

// ─── SQL projectors ──────────────────────────────────────────────────────
//
// Each predicate above has a matching SQL list helper that returns the
// comma-separated quoted status values for which the predicate is true,
// suitable for splicing into a SQL `status IN (...)` or `NOT IN (...)`
// clause. The list is built once at package init by walking AllStatuses()
// through the predicate; values are sorted lexically for deterministic
// output (drift tests depend on it).
//
// Splice is safe — values come from the Status enum, not user input.
// Callers use the helpers as:
//
//     fmt.Sprintf(`... WHERE status IN (%s) ...`, protocol.TerminalStatusSQLList())
//     fmt.Sprintf(`... WHERE status NOT IN (%s) ...`, protocol.TerminalStatusSQLList())

var (
	terminalStatusSQLList              = buildStatusSQLList(IsTerminal)
	nonTerminalStatusSQLList           = buildStatusSQLList(func(s Status) bool { return !IsTerminal(s) })
	failureTerminalStatusSQLList       = buildStatusSQLList(IsFailureTerminal)
	vendorActiveStatusSQLList          = buildStatusSQLList(IsVendorActive)
	preDispatchStatusSQLList           = buildStatusSQLList(IsPreDispatch)
	acquiringStatusSQLList             = buildStatusSQLList(IsAcquiring)
	runtimeStuckCandidateStatusSQLList = buildStatusSQLList(IsRuntimeStuckCandidate)
	operatorVisibleStatusSQLList       = buildStatusSQLList(IsOperatorVisible)
)

// buildStatusSQLList walks every known status, filters by the predicate,
// quotes each value, and joins with commas. Sorted lex for deterministic
// output so drift tests (status_test.go) can pin the exact string form.
func buildStatusSQLList(pred func(Status) bool) string {
	var parts []string
	for _, s := range AllStatuses() {
		if pred(s) {
			parts = append(parts, "'"+string(s)+"'")
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// TerminalStatusSQLList returns 'cancelled','confirmed','failed','skipped'.
// Use in `status IN (TerminalStatusSQLList())` or its NOT IN inverse.
func TerminalStatusSQLList() string { return terminalStatusSQLList }

// NonTerminalStatusSQLList returns every status that is not terminal.
// Provided as a positive form for queries that read more naturally as
// `status IN (...)` than `NOT IN (...)`. Most callers want the negation
// of TerminalStatusSQLList and should use that with NOT IN instead;
// this helper exists for the handful of cases where positive-form IN
// is clearer.
func NonTerminalStatusSQLList() string { return nonTerminalStatusSQLList }

// FailureTerminalStatusSQLList returns 'cancelled','failed'.
// Use for anomaly detection that wants "the operator should look at
// this" rather than the broader "this is done."
func FailureTerminalStatusSQLList() string { return failureTerminalStatusSQLList }

// VendorActiveStatusSQLList returns 'dispatched','in_transit','staged'.
// Use for vendor-side polling filters and floor-execution capacity gates.
func VendorActiveStatusSQLList() string { return vendorActiveStatusSQLList }

// PreDispatchStatusSQLList returns 'pending','queued','sourcing'.
// Use for source-reference guards (re-parent, delete, rename a node
// referenced by not-yet-dispatched orders).
func PreDispatchStatusSQLList() string { return preDispatchStatusSQLList }

// AcquiringStatusSQLList returns 'queued','sourcing' — the fulfillment scanner's
// retry set. Use in `status IN (AcquiringStatusSQLList())`.
func AcquiringStatusSQLList() string { return acquiringStatusSQLList }

// RuntimeStuckCandidateStatusSQLList returns the non-terminal subset
// that should be watched for stale updated_at — excludes faulted,
// delivered, reshuffling per the predicate's doc.
func RuntimeStuckCandidateStatusSQLList() string { return runtimeStuckCandidateStatusSQLList }

// OperatorVisibleStatusSQLList returns the statuses that should still
// appear on Edge HMI surfaces. Skipped/Confirmed/Cancelled are excluded;
// Failed is intentionally included so the operator can retry.
func OperatorVisibleStatusSQLList() string { return operatorVisibleStatusSQLList }
