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

	"shingocore/store/messaging"
)

const criticalOutboxAge = 5 * time.Minute
const stuckOrderAge = 30 * time.Minute

// CompletionAnomaly describes drift between terminal orders and bin
// claim state.
type CompletionAnomaly struct {
	OrderID     int64  `json:"order_id"`
	BinID       *int64 `json:"bin_id,omitempty"`
	OrderStatus string `json:"order_status"`
	BinStatus   string `json:"bin_status,omitempty"`
	Issue       string `json:"issue"`
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
	CompletionAnomalies int        `json:"completion_anomalies"`
	StuckOrders         int        `json:"stuck_orders"`
	ExpiredStagedBins   int        `json:"expired_staged_bins"`
	StaleEdges          int        `json:"stale_edges"`
	TotalAnomalies      int        `json:"total_anomalies"`
	OutboxPending       int        `json:"outbox_pending"`
	OldestOutboxAt      *time.Time `json:"oldest_outbox_at,omitempty"`
	DeadLetters         int        `json:"dead_letters"`
	Status              string     `json:"status"`
}

// ListOrderCompletionAnomalies surfaces high-risk drift between
// terminal orders and bin claim state.
func ListOrderCompletionAnomalies(db *sql.DB) ([]*CompletionAnomaly, error) {
	rows, err := db.Query(`
		SELECT o.id AS order_id, b.id AS bin_id, o.status AS order_status, b.status AS bin_status, 'terminal_order_still_claims_bin' AS issue
		FROM orders o
		JOIN bins b ON b.claimed_by = o.id
		WHERE o.completed_at IS NOT NULL OR o.status IN ('cancelled', 'failed')
		UNION ALL
		SELECT o.id AS order_id, NULL::bigint AS bin_id, o.status AS order_status, '' AS bin_status, 'completed_order_missing_bin' AS issue
		FROM orders o
		WHERE o.completed_at IS NOT NULL AND o.bin_id IS NULL
		UNION ALL
		SELECT o.id AS order_id, o.bin_id AS bin_id, o.status AS order_status, COALESCE(b.status, '') AS bin_status, 'confirmed_without_completed_at' AS issue
		FROM orders o
		LEFT JOIN bins b ON b.id = o.bin_id
		WHERE o.status = 'confirmed' AND o.completed_at IS NULL
		ORDER BY order_id, issue`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var anomalies []*CompletionAnomaly
	for rows.Next() {
		var a CompletionAnomaly
		var binID *int64
		if err := rows.Scan(&a.OrderID, &binID, &a.OrderStatus, &a.BinStatus, &a.Issue); err != nil {
			return nil, err
		}
		a.BinID = binID
		anomalies = append(anomalies, &a)
	}
	return anomalies, rows.Err()
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

	rows, err := db.Query(`
		SELECT id, status, updated_at
		FROM orders
		WHERE status IN ('pending','sourcing','submitted','acknowledged','dispatched','in_transit','staged')
		  AND updated_at < NOW() - ($1 * INTERVAL '1 second')
		ORDER BY updated_at ASC`, int(stuckOrderAge.Seconds()))
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
	rows, err = db.Query(`
		SELECT b.id, b.label, b.status, b.claimed_by,
		       COALESCE(o.status, 'no_order') AS order_status
		FROM bins b
		LEFT JOIN orders o ON o.id = b.claimed_by
		WHERE b.manifest IS NOT NULL
		  AND (b.claimed_by IS NULL
		       OR o.status IN ('confirmed', 'failed', 'cancelled'))
		ORDER BY b.id`)
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
		CompletionAnomalies: len(completion),
		TotalAnomalies:      len(anomalies),
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
	result, err := db.Exec(`
		UPDATE bins
		SET claimed_by = NULL, updated_at = NOW()
		WHERE claimed_by IS NOT NULL
		  AND claimed_by IN (
		    SELECT id FROM orders
		    WHERE status IN ('confirmed', 'failed', 'cancelled')
		  )`)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}
