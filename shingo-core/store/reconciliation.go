package store

import "time"

const criticalOutboxAge = 5 * time.Minute
const stuckOrderAge = 30 * time.Minute

type OrderCompletionAnomaly struct {
	OrderID     int64  `json:"order_id"`
	BinID       *int64 `json:"bin_id,omitempty"`
	OrderStatus string `json:"order_status"`
	BinStatus   string `json:"bin_status,omitempty"`
	Issue       string `json:"issue"`
}

type ReconciliationAnomaly struct {
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

type ReconciliationSummary struct {
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

// ListOrderCompletionAnomalies surfaces high-risk drift between terminal orders and bin claim state.
func (db *DB) ListOrderCompletionAnomalies() ([]*OrderCompletionAnomaly, error) {
	rows, err := db.Query(`
		SELECT o.id, b.id, o.status, b.status, 'terminal_order_still_claims_bin' AS issue
		FROM orders o
		JOIN bins b ON b.claimed_by = o.id
		WHERE o.completed_at IS NOT NULL OR o.status IN ('cancelled', 'failed')
		UNION ALL
		SELECT o.id, NULL::bigint, o.status, '' AS bin_status, 'completed_order_missing_bin' AS issue
		FROM orders o
		WHERE o.completed_at IS NOT NULL AND o.bin_id IS NULL
		UNION ALL
		SELECT o.id, o.bin_id, o.status, COALESCE(b.status, '') AS bin_status, 'confirmed_without_completed_at' AS issue
		FROM orders o
		LEFT JOIN bins b ON b.id = o.bin_id
		WHERE o.status = 'confirmed' AND o.completed_at IS NULL
		ORDER BY order_id, issue`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var anomalies []*OrderCompletionAnomaly
	for rows.Next() {
		var a OrderCompletionAnomaly
		var binID *int64
		if err := rows.Scan(&a.OrderID, &binID, &a.OrderStatus, &a.BinStatus, &a.Issue); err != nil {
			return nil, err
		}
		a.BinID = binID
		anomalies = append(anomalies, &a)
	}
	return anomalies, rows.Err()
}

func (db *DB) ListReconciliationAnomalies() ([]*ReconciliationAnomaly, error) {
	completion, err := db.ListOrderCompletionAnomalies()
	if err != nil {
		return nil, err
	}

	var anomalies []*ReconciliationAnomaly
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
		anomalies = append(anomalies, &ReconciliationAnomaly{
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
		anomalies = append(anomalies, &ReconciliationAnomaly{
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
		anomalies = append(anomalies, &ReconciliationAnomaly{
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
		anomalies = append(anomalies, &ReconciliationAnomaly{
			Category:          "edge_connectivity",
			Severity:          "degraded",
			Issue:             "edge_marked_stale",
			RecommendedAction: "request_reregistration",
			StationID:         stationID,
			ObservedAt:        observedAt,
		})
	}
	return anomalies, rows.Err()
}

func (db *DB) GetReconciliationSummary() (*ReconciliationSummary, error) {
	completion, err := db.ListOrderCompletionAnomalies()
	if err != nil {
		return nil, err
	}
	anomalies, err := db.ListReconciliationAnomalies()
	if err != nil {
		return nil, err
	}

	summary := &ReconciliationSummary{
		CompletionAnomalies: len(completion),
		TotalAnomalies:      len(anomalies),
	}

	row := db.QueryRow(`SELECT COUNT(*), MIN(created_at) FROM outbox WHERE sent_at IS NULL AND retries < $1`, MaxOutboxRetries)
	if err := row.Scan(&summary.OutboxPending, &summary.OldestOutboxAt); err != nil {
		return nil, err
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE sent_at IS NULL AND retries >= $1`, MaxOutboxRetries).Scan(&summary.DeadLetters); err != nil {
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
