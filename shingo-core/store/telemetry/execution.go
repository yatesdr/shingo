package telemetry

import "fmt"

// Execution time vs lead time (Q-031).
//
// duration_ms on mission_telemetry is *lead time*: order-created → terminal,
// which includes however long the order queued waiting for a robot. That's the
// right number for "how long from asking to receiving," but wrong for "what the
// robot actually spent" — utilization, robot busy-time, and the duration trend
// graphs all want the latter.
//
// Execution time = assignment → completion, BOTH endpoints from order_history —
// shingo's OWN lifecycle log (written by LifecycleService), keyed on canonical
// statuses. mission_events is the WRONG source: it's fed by the fleet/RDS status
// poller, so its new_state column holds raw vendor states (RUNNING/WAITING in
// sim, SEER states in prod), not our canonical acknowledged/in_transit.
//
//   - Assignment = earliest acknowledged/in_transit transition (robot started).
//     Excludes the pre-dispatch queue (pending/sourcing/queued) and 'dispatched'
//     (Core→vendor handoff, which can sit in the vendor's queue).
//   - Completion = earliest terminal-ish transition; for a confirmed mission
//     that's 'delivered' (robot dropped the load), NOT 'confirmed' — so the
//     operator-confirm wait is excluded. For failures it's the failure
//     transition.
//
// Sourcing BOTH endpoints from order_history (rather than mt.core_completed)
// keeps the two on the same clock — correct on prod, and it also survives the
// dev sim's fast-forward clock drift (which otherwise makes core_completed
// inconsistent with order_history). Computed on read via correlated subqueries
// over order_history (indexed by order_id) — migration-free and retroactive.

// assignmentStatesSQL / completionStatesSQL are the order_history.status sets
// bounding robot execution. Kept as SQL literals so the persistence layer takes
// no protocol import; order_history stores lowercase canonical values.
const (
	assignmentStatesSQL = `'acknowledged','in_transit'`
	completionStatesSQL = `'delivered','confirmed','failed','cancelled','skipped'`
)

// assignmentExpr returns the SQL correlated subquery for a mission's execution
// start — MIN(created_at) over its acknowledged/in_transit transitions in
// order_history. NULL when the mission never reached a robot (e.g. failed in
// Core's planning space). alias is the mission_telemetry table alias in the
// surrounding query.
func assignmentExpr(alias string) string {
	return fmt.Sprintf(`(SELECT MIN(oh.created_at) FROM order_history oh
		WHERE oh.order_id = %s.order_id AND oh.status IN (`+assignmentStatesSQL+`))`, alias)
}

// completionExpr returns the SQL correlated subquery for a mission's execution
// end — MIN(created_at) over its terminal-ish transitions in order_history
// (delivered wins over confirmed for a confirmed mission). NULL when none
// recorded. alias is the mission_telemetry table alias.
func completionExpr(alias string) string {
	return fmt.Sprintf(`(SELECT MIN(oh.created_at) FROM order_history oh
		WHERE oh.order_id = %s.order_id AND oh.status IN (`+completionStatesSQL+`))`, alias)
}

// executionMSExpr returns SQL for execution time in milliseconds for one
// mission: completion − assignment, both from order_history. NULL when the
// mission has no assignment (never executed); aggregate callers COALESCE/AVG
// (NULLs skipped) or filter `> 0`. alias is the mission_telemetry table alias.
func executionMSExpr(alias string) string {
	return fmt.Sprintf(`(EXTRACT(EPOCH FROM (%s - %s)) * 1000)`, completionExpr(alias), assignmentExpr(alias))
}
