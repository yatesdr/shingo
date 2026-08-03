// Package reconciliation holds cross-aggregate anomaly detection for
// shingo-core (stuck orders, expired staged bins, stale edges, orphaned
// bin claims, manifests without active orders).
//
// Phase 5 of the architecture plan moved the reconciliation query
// fan-out out of the flat store/ package and into this sub-package.
// The outer store/ keeps type aliases and one-line delegate methods on
// *store.DB so external callers see no API change.
//
// Reconciliation is grouped as its own aggregate (rather than colocated
// with orders, bins, or edges) because the whole point of the module is
// cross-aggregate drift detection — putting it under any one aggregate
// would misrepresent the dependency direction.
package reconciliation

import (
	"database/sql"
	"fmt"
	"time"

	"shingo/protocol"
	"shingocore/store/messaging"
)

const criticalOutboxAge = 5 * time.Minute
const stuckOrderAge = 30 * time.Minute

// queuedOrderAge is the SEPARATE, longer staleness bound for an order that is
// still acquiring (queued or sourcing). Waiting is what these statuses are FOR,
// so they need a different threshold from the ones where the fleet already has
// the order and nothing is moving.
//
// 30 minutes is the right question to ask of a `dispatched` leg — a robot that
// has not moved in half an hour has been forgotten. It is the wrong question to
// ask of an order waiting on material: a loop can legitimately sit unfillable
// across a shift change or a supplier gap, and flagging every one of those at
// 30 minutes trains people to ignore the anomaly board, which costs more than
// the silence did.
//
// Two hours is the operator judgement (2026-08-03): long enough that ordinary
// material churn clears on its own, short enough to catch a wedge inside one
// shift. Measured against the live Springfield board the day it was set, this
// separates a genuinely stuck window (4h41m) from routine contention (48m).
const queuedOrderAge = 2 * time.Hour

// CompletionAnomalyWindow is how far back a completion anomaly still counts
// AS A VERDICT.
//
// The list is deliberately still all-time — a completed order with a NULL
// bin_id is a real inconsistency whenever it happened, and hiding the old ones
// would make them unfindable. What changes is what "Core degraded" MEANS.
//
// Measured at Springfield 2026-07-29: ten anomalies, every one an order
// created and completed between 2026-03-30 and 2026-04-06, pinning the strip
// red for four months while database, fleet and messaging were all up and the
// pool was idle. An unbounded count turns a health verdict into a permanent
// latch on the oldest mistake anyone ever made — the strip says "something is
// wrong now" and means "something was wrong once".
const CompletionAnomalyWindow = 24 * time.Hour

// CompletionAnomaly describes drift between terminal orders and bin
// claim state.
type CompletionAnomaly struct {
	OrderID     int64  `json:"order_id"`
	BinID       *int64 `json:"bin_id,omitempty"`
	OrderStatus string `json:"order_status"`
	BinStatus   string `json:"bin_status,omitempty"`
	Issue       string `json:"issue"`
	// ObservedAt is when the order reached the state being complained about:
	// completed_at where there is one, updated_at for the confirmed-but-never-
	// completed case, which by definition has no completed_at. Carried on the
	// row rather than derived in the caller so the anomaly LIST can print an
	// age — the thing that would have made the Springfield ten obvious on
	// sight instead of costing a psql session.
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

// Anomaly describes one reconciliation finding.
type Anomaly struct {
	Category          string     `json:"category"`
	Severity          string     `json:"severity"`
	Issue             string     `json:"issue"`
	RecommendedAction string     `json:"recommended_action,omitempty"`
	OrderID           *int64     `json:"order_id,omitempty"`
	BinID             *int64     `json:"bin_id,omitempty"`
	StationID         string     `json:"station_id,omitempty"`
	OrderStatus       string     `json:"order_status,omitempty"`
	BinStatus         string     `json:"bin_status,omitempty"`
	Detail            string     `json:"detail,omitempty"`
	ObservedAt        *time.Time `json:"observed_at,omitempty"`
}

// Summary aggregates reconciliation counts for the health endpoint.
type Summary struct {
	// CompletionAnomalies counts only those inside CompletionAnomalyWindow.
	// This is the one the verdict reads.
	CompletionAnomalies int `json:"completion_anomalies"`
	// CompletionAnomaliesTotal is every one on record. Reported so a windowed
	// zero cannot be mistaken for "there are none" — the old rows are still
	// real, still worth fixing, and still one query away.
	CompletionAnomaliesTotal int `json:"completion_anomalies_total"`
	// CompletionAnomalyWindowHours is CompletionAnomalyWindow, reported rather
	// than restated: the strip labels its tile from this, so the label cannot
	// drift from the constant the verdict actually uses.
	CompletionAnomalyWindowHours int        `json:"completion_anomaly_window_hours"`
	StuckOrders                  int        `json:"stuck_orders"`
	ExpiredStagedBins            int        `json:"expired_staged_bins"`
	StaleEdges                   int        `json:"stale_edges"`
	TotalAnomalies               int        `json:"total_anomalies"`
	OutboxPending                int        `json:"outbox_pending"`
	OldestOutboxAt               *time.Time `json:"oldest_outbox_at,omitempty"`
	DeadLetters                  int        `json:"dead_letters"`
	Status                       string     `json:"status"`
}

// ListOrderCompletionAnomalies surfaces high-risk drift between
// terminal orders and bin claim state.
func ListOrderCompletionAnomalies(db *sql.DB) ([]*CompletionAnomaly, error) {
	rows, err := db.Query(fmt.Sprintf(`
		SELECT o.id AS order_id, b.id AS bin_id, o.status AS order_status, b.status AS bin_status, 'terminal_order_still_claims_bin' AS issue, COALESCE(o.completed_at, o.updated_at) AS observed_at
		FROM orders o
		JOIN bins b ON b.claimed_by = o.id
		WHERE o.completed_at IS NOT NULL OR o.status IN (%s)
		UNION ALL
		SELECT o.id AS order_id, NULL::bigint AS bin_id, o.status AS order_status, '' AS bin_status, 'completed_order_missing_bin' AS issue, COALESCE(o.completed_at, o.updated_at) AS observed_at
		FROM orders o
		WHERE o.completed_at IS NOT NULL AND o.bin_id IS NULL
		UNION ALL
		SELECT o.id AS order_id, o.bin_id AS bin_id, o.status AS order_status, COALESCE(b.status, '') AS bin_status, 'confirmed_without_completed_at' AS issue, COALESCE(o.completed_at, o.updated_at) AS observed_at
		FROM orders o
		LEFT JOIN bins b ON b.id = o.bin_id
		WHERE o.status = 'confirmed' AND o.completed_at IS NULL
		ORDER BY order_id, issue`, protocol.FailureTerminalStatusSQLList()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var anomalies []*CompletionAnomaly
	for rows.Next() {
		var a CompletionAnomaly
		var binID *int64
		var observedAt *time.Time
		if err := rows.Scan(&a.OrderID, &binID, &a.OrderStatus, &a.BinStatus, &a.Issue, &observedAt); err != nil {
			return nil, err
		}
		a.BinID = binID
		a.ObservedAt = observedAt
		anomalies = append(anomalies, &a)
	}
	return anomalies, rows.Err()
}

// countRecent returns how many of the anomalies fall inside the window.
//
// A row with no timestamp counts as recent. The alternative — silently
// dropping it — makes a NULL look like health, which is the failure family
// this whole panel exists to catch.
func countRecent(anomalies []*CompletionAnomaly, now time.Time, window time.Duration) int {
	cutoff := now.Add(-window)
	n := 0
	for _, a := range anomalies {
		if a.ObservedAt == nil || a.ObservedAt.After(cutoff) {
			n++
		}
	}
	return n
}

// ListAnomalies returns all reconciliation findings across categories.
func ListAnomalies(db *sql.DB) ([]*Anomaly, error) {
	completion, err := ListOrderCompletionAnomalies(db)
	if err != nil {
		return nil, err
	}

	var anomalies []*Anomaly
	for _, a := range completion {
		issue := a.Issue
		action := ""
		switch issue {
		case "confirmed_without_completed_at":
			action = "reapply_completion"
		case "terminal_order_still_claims_bin":
			action = "release_terminal_claim"
		}
		orderID := a.OrderID
		anomalies = append(anomalies, &Anomaly{
			Category:          "order_completion",
			Severity:          "critical",
			Issue:             issue,
			RecommendedAction: action,
			OrderID:           &orderID,
			BinID:             a.BinID,
			OrderStatus:       a.OrderStatus,
			BinStatus:         a.BinStatus,
		})
	}

	// Two thresholds, picked per row by status — see queuedOrderAge.
	//
	// QUEUED ONLY, not both acquiring statuses. `sourcing` is meant to be
	// transient (MoveToSourcing sits at the start of the reserve attempt, so few
	// orders rest there), which makes half an hour in `sourcing` a real signal
	// worth keeping. Widening the longer bound to cover it would silence an alarm
	// that currently works, to fix a problem it does not have.
	//
	// Splitting it in SQL rather than running two queries keeps the ORDER BY over
	// the whole result, so the oldest anomaly is still first regardless of which
	// bound produced it.
	//
	// THE ::int CASTS ARE LOAD-BEARING. The driver sends these parameters
	// untyped, so without them Postgres infers the CASE result as `text` and the
	// whole query dies at RUNTIME on `operator does not exist: text * interval`
	// — a live 500 on the health endpoint, from a query that builds and vets
	// clean. It also survives a psql `PREPARE stuckq(int, int, text)` check,
	// because declaring the types is exactly what the driver does not do.
	// TestListAnomalies_QueuedGetsTheLongerBound exercises this through the
	// driver, which is the only check that would have caught it.
	rows, err := db.Query(fmt.Sprintf(`
		SELECT id, status, updated_at
		FROM orders
		WHERE status IN (%s)
		  AND updated_at < NOW() - (
		        CASE WHEN status = $3 THEN $2::int ELSE $1::int END * INTERVAL '1 second')
		ORDER BY updated_at ASC`, protocol.RuntimeStuckCandidateStatusSQLList()),
		int(stuckOrderAge.Seconds()), int(queuedOrderAge.Seconds()), string(protocol.StatusQueued))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var orderID int64
		var status string
		var updatedAt time.Time
		if err := rows.Scan(&orderID, &status, &updatedAt); err != nil {
			return nil, err
		}
		anomalies = append(anomalies, &Anomaly{
			Category:          "order_runtime",
			Severity:          "degraded",
			Issue:             "active_order_stuck",
			RecommendedAction: "cancel_stuck_order",
			OrderID:           &orderID,
			OrderStatus:       status,
			ObservedAt:        &updatedAt,
			Detail:            "order has not advanced within the allowed age threshold",
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = db.Query(`
		SELECT id, status, staged_expires_at
		FROM bins
		WHERE status='staged'
		  AND staged_expires_at IS NOT NULL
		  AND staged_expires_at < NOW()
		ORDER BY staged_expires_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var binID int64
		var status string
		var observedAt time.Time
		if err := rows.Scan(&binID, &status, &observedAt); err != nil {
			return nil, err
		}
		anomalies = append(anomalies, &Anomaly{
			Category:          "bin_staging",
			Severity:          "degraded",
			Issue:             "staged_bin_expired",
			RecommendedAction: "release_staged_bin",
			BinID:             &binID,
			BinStatus:         status,
			ObservedAt:        &observedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = db.Query(`
		SELECT station_id, last_heartbeat
		FROM edge_registry
		WHERE status='stale'
		ORDER BY station_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var stationID string
		var observedAt *time.Time
		if err := rows.Scan(&stationID, &observedAt); err != nil {
			return nil, err
		}
		anomalies = append(anomalies, &Anomaly{
			Category:          "edge_connectivity",
			Severity:          "degraded",
			Issue:             "edge_marked_stale",
			RecommendedAction: "request_reregistration",
			StationID:         stationID,
			ObservedAt:        observedAt,
		})
	}

	// Detect bins stacked at a non-storage, non-staging concrete node — i.e.,
	// more than one bin physically present at a process node (line node,
	// dropoff target, etc.). This indicates a prior cycle's evac order failed
	// to complete the bin handoff (e.g., Robot B faulted en route from core
	// to AMR group, operator took manual control, transaction never finalized)
	// while subsequent cycles continued to deliver new bins to the same node.
	// See bug-fix-review-plan.md item 3.1.
	//
	// Excluded — these are aggregate/synthetic types, not concrete physical
	// positions. Their bin_count rolls up across child slots and is
	// meaningless for the "stacked at one position" check:
	//   NGRP    — synthetic parent for lanes / direct nodes
	//   LANE    — depth-ordered slot group (children are the actual slots)
	//   STOR    — supermarket storage aggregate
	//   TRANSIT — logical in-flight bin model (many bins can be "in transit")
	//
	// All other concrete node types (line nodes, dropoff targets, STAG
	// staging positions, OVFL overflow positions) hold one physical bin at
	// a time. >1 at the same node ID is the anomaly we want to surface.
	rows, err = db.Query(`
		SELECT n.id, n.name, COUNT(b.id) AS bin_count
		FROM bins b
		JOIN nodes n ON n.id = b.node_id
		JOIN node_types nt ON nt.id = n.node_type_id
		WHERE n.is_synthetic = false
		  AND nt.code NOT IN ('NGRP', 'LANE', 'STOR', 'TRANSIT')
		  AND n.parent_id IS NULL
		GROUP BY n.id, n.name
		HAVING COUNT(b.id) > 1
		ORDER BY n.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID int64
		var nodeName string
		var binCount int
		if err := rows.Scan(&nodeID, &nodeName, &binCount); err != nil {
			return nil, err
		}
		anomalies = append(anomalies, &Anomaly{
			Category:          "node_inventory",
			Severity:          "critical",
			Issue:             "multi_bin_at_non_storage_node",
			RecommendedAction: "clear_stacked_bins",
			Detail: fmt.Sprintf("node %s has %d bins stacked — likely prior evac handoff failed; clear via admin bin-move",
				nodeName, binCount),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Detect bins with speculative manifest but no active claiming order.
	// This is informational only — manifest represents physical reality and
	// should NOT be cleared. The detection surfaces these bins for review.
	rows, err = db.Query(fmt.Sprintf(`
		SELECT b.id, b.label, b.status, b.claimed_by,
		       COALESCE(o.status, 'no_order') AS order_status
		FROM bins b
		LEFT JOIN orders o ON o.id = b.claimed_by
		WHERE b.manifest IS NOT NULL
		  AND (b.claimed_by IS NULL
		       OR o.status IN (%s))
		ORDER BY b.id`, protocol.TerminalStatusSQLList()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var binID int64
		var label, binStatus string
		var claimedBy *int64
		var orderStatus string
		if err := rows.Scan(&binID, &label, &binStatus, &claimedBy, &orderStatus); err != nil {
			return nil, err
		}
		anomalies = append(anomalies, &Anomaly{
			Category:          "bin_manifest",
			Severity:          "info",
			Issue:             "manifest_without_active_order",
			RecommendedAction: "review_manifest",
			BinID:             &binID,
			BinStatus:         binStatus,
			OrderID:           claimedBy,
			OrderStatus:       orderStatus,
			Detail:            fmt.Sprintf("bin %s has manifest but no active claiming order (claimed_by=%v, order_status=%s)", label, claimedBy, orderStatus),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return anomalies, nil
}

// GetSummary rolls ListAnomalies plus outbox counts into a single
// status payload for the health endpoint.
func GetSummary(db *sql.DB) (*Summary, error) {
	completion, err := ListOrderCompletionAnomalies(db)
	if err != nil {
		return nil, err
	}
	anomalies, err := ListAnomalies(db)
	if err != nil {
		return nil, err
	}

	summary := &Summary{
		CompletionAnomalies:          countRecent(completion, time.Now().UTC(), CompletionAnomalyWindow),
		CompletionAnomaliesTotal:     len(completion),
		CompletionAnomalyWindowHours: int(CompletionAnomalyWindow / time.Hour),
		TotalAnomalies:               len(anomalies),
	}

	row := db.QueryRow(`SELECT COUNT(*), MIN(created_at) FROM outbox WHERE sent_at IS NULL AND retries < $1`, messaging.MaxOutboxRetries)
	if err := row.Scan(&summary.OutboxPending, &summary.OldestOutboxAt); err != nil {
		return nil, err
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE sent_at IS NULL AND retries >= $1`, messaging.MaxOutboxRetries).Scan(&summary.DeadLetters); err != nil {
		return nil, err
	}

	for _, a := range anomalies {
		switch a.Issue {
		case "active_order_stuck":
			summary.StuckOrders++
		case "staged_bin_expired":
			summary.ExpiredStagedBins++
		case "edge_marked_stale":
			summary.StaleEdges++
		}
	}
	summary.Status = "ok"
	if summary.OutboxPending > 0 || summary.StuckOrders > 0 || summary.ExpiredStagedBins > 0 || summary.StaleEdges > 0 {
		summary.Status = "degraded"
	}
	if summary.CompletionAnomalies > 0 || summary.DeadLetters > 0 {
		summary.Status = "critical"
	} else if summary.OldestOutboxAt != nil && time.Since(summary.OldestOutboxAt.UTC()) >= criticalOutboxAge {
		summary.Status = "critical"
	}

	return summary, nil
}

// ReleaseOrphanedClaims finds bins still claimed by terminal orders and
// releases them. Defense-in-depth sweep that catches any claims that
// leaked past the atomic status transitions (e.g. due to a process
// crash mid-transaction). Returns the number of claims released.
func ReleaseOrphanedClaims(db *sql.DB) (int, error) {
	result, err := db.Exec(fmt.Sprintf(`
		UPDATE bins
		SET claimed_by = NULL, updated_at = NOW()
		WHERE claimed_by IS NOT NULL
		  AND claimed_by IN (
		    SELECT id FROM orders
		    WHERE status IN (%s)
		  )`, protocol.TerminalStatusSQLList()))
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()

	// Store dual: release destination-slot claims (nodes.claimed_by) held by
	// terminal orders. Catches any slot claim that leaked past the atomic
	// terminal transitions (FailOrderAtomic/SkipOrderAtomic/CancelOrderAtomic),
	// same as the bin sweep above.
	slotRes, err := db.Exec(fmt.Sprintf(`
		UPDATE nodes
		SET claimed_by = NULL, updated_at = NOW()
		WHERE claimed_by IS NOT NULL
		  AND claimed_by IN (
		    SELECT id FROM orders
		    WHERE status IN (%s)
		  )`, protocol.TerminalStatusSQLList()))
	if err != nil {
		return int(n), err
	}
	sn, _ := slotRes.RowsAffected()
	return int(n + sn), nil
}
