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

// faultedDwellMSExpr returns SQL for the total time a mission spent `faulted`,
// in milliseconds — the sum over each faulted transition of the gap to
// whatever status came next.
//
// Subtracted from execution time because a faulted robot is not working. The
// grace period is 'faulted' precisely so a transient fleet failure can recover
// (faulted→in_transit) or be finished by hand (faulted→delivered), and a
// mission that sat faulted for 45 minutes and then recovered has assignment
// long before completion — so without this, that 45 minutes reads as robot
// busy time. Utilization then inflates exactly on the days robots are stuck,
// which is precisely backwards.
//
// LEAD over the order's own history gives each faulted interval its end; a
// mission still faulted at its terminal gets the gap up to that terminal row,
// which is correct — it was stuck right up until it was given up on.
func faultedDwellMSExpr(alias string) string {
	return fmt.Sprintf(`COALESCE((
		SELECT SUM(EXTRACT(EPOCH FROM (h.next_at - h.created_at))) * 1000
		FROM (
			SELECT oh.created_at, oh.status,
			       LEAD(oh.created_at) OVER (ORDER BY oh.created_at, oh.id) AS next_at
			FROM order_history oh WHERE oh.order_id = %s.order_id
		) h
		WHERE h.status = 'faulted' AND h.next_at IS NOT NULL
	), 0)`, alias)
}

// executionMSExpr returns SQL for execution time in milliseconds for one
// mission: completion − assignment (both from order_history) MINUS the time
// the mission spent faulted. NULL when the mission has no assignment (never
// executed); aggregate callers COALESCE/AVG (NULLs skipped) or filter `> 0`.
// alias is the mission_telemetry table alias.
//
// Floored at 0: execution time is never negative, and a mission whose faulted
// dwell exceeds its span is a data problem, not a robot that worked backwards.
func executionMSExpr(alias string) string {
	return fmt.Sprintf(`GREATEST(EXTRACT(EPOCH FROM (%s - %s)) * 1000 - %s, 0)`,
		completionExpr(alias), assignmentExpr(alias), faultedDwellMSExpr(alias))
}
