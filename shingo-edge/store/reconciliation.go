package store

import "time"

const criticalEdgeOutboxAge = 5 * time.Minute
const edgeStuckOrderAge = 30 * time.Minute
const edgeDeliveredConfirmAge = 10 * time.Minute

type ReconciliationAnomaly struct {
	Severity          string     `json:"severity"`
	Issue             string     `json:"issue"`
	RecommendedAction string     `json:"recommended_action,omitempty"`
	OrderID           *int64     `json:"order_id,omitempty"`
	OrderUUID         string     `json:"order_uuid,omitempty"`
	Status            string     `json:"status,omitempty"`
	ObservedAt        *time.Time `json:"observed_at,omitempty"`
	Detail            string     `json:"detail,omitempty"`
}

type ReconciliationSummary struct {
	TotalAnomalies       int        `json:"total_anomalies"`
	StuckOrders          int        `json:"stuck_orders"`
	DeliveredUnconfirmed int        `json:"delivered_unconfirmed"`
	OutboxPending        int        `json:"outbox_pending"`
	OldestOutboxAt       *time.Time `json:"oldest_outbox_at,omitempty"`
	DeadLetters          int        `json:"dead_letters"`
	Status               string     `json:"status"`
}

func (db *DB) ListReconciliationAnomalies() ([]*ReconciliationAnomaly, error) {
	var anomalies []*ReconciliationAnomaly

	rows, err := db.Query(`SELECT id, uuid, status, updated_at
		FROM orders
		WHERE status IN ('pending', 'submitted', 'acknowledged', 'in_transit', 'staged')
		  AND updated_at < datetime('now', ?)
		ORDER BY updated_at ASC`, "-30 minutes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var orderID int64
		var orderUUID, status, updatedAt string
		if err := rows.Scan(&orderID, &orderUUID, &status, &updatedAt); err != nil {
			return nil, err
		}
		observed := scanTime(updatedAt)
		anomalies = append(anomalies, &ReconciliationAnomaly{
			Severity:          "degraded",
			Issue:             "active_order_stuck",
			RecommendedAction: "sync_order_status",
			OrderID:           &orderID,
			OrderUUID:         orderUUID,
			Status:            status,
			ObservedAt:        &observed,
			Detail:            "order has not advanced within the local threshold",
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = db.Query(`SELECT id, uuid, status, updated_at
		FROM orders
		WHERE status = 'delivered'
		  AND updated_at < datetime('now', ?)
		ORDER BY updated_at ASC`, "-10 minutes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var orderID int64
		var orderUUID, status, updatedAt string
		if err := rows.Scan(&orderID, &orderUUID, &status, &updatedAt); err != nil {
			return nil, err
		}
		observed := scanTime(updatedAt)
		anomalies = append(anomalies, &ReconciliationAnomaly{
			Severity:          "degraded",
			Issue:             "delivered_order_unconfirmed",
			RecommendedAction: "sync_order_status",
			OrderID:           &orderID,
			OrderUUID:         orderUUID,
			Status:            status,
			ObservedAt:        &observed,
			Detail:            "delivery was recorded locally but has remained unconfirmed",
		})
	}
	return anomalies, rows.Err()
}

func (db *DB) GetReconciliationSummary() (*ReconciliationSummary, error) {
	anomalies, err := db.ListReconciliationAnomalies()
	if err != nil {
		return nil, err
	}
	summary := &ReconciliationSummary{TotalAnomalies: len(anomalies)}

	row := db.QueryRow(`SELECT COUNT(*), MIN(created_at) FROM outbox WHERE sent_at IS NULL AND retries < ?`, MaxOutboxRetries)
	var oldest string
	if err := row.Scan(&summary.OutboxPending, &oldest); err != nil {
		return nil, err
	}
	if oldest != "" {
		t := scanTime(oldest)
		summary.OldestOutboxAt = &t
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE sent_at IS NULL AND retries >= ?`, MaxOutboxRetries).Scan(&summary.DeadLetters); err != nil {
		return nil, err
	}
	for _, a := range anomalies {
		switch a.Issue {
		case "active_order_stuck":
			summary.StuckOrders++
		case "delivered_order_unconfirmed":
			summary.DeliveredUnconfirmed++
		}
	}
	summary.Status = "ok"
	if summary.TotalAnomalies > 0 || summary.OutboxPending > 0 {
		summary.Status = "degraded"
	}
	if summary.DeadLetters > 0 {
		summary.Status = "critical"
	} else if summary.OldestOutboxAt != nil && time.Since(summary.OldestOutboxAt.UTC()) >= criticalEdgeOutboxAge {
		summary.Status = "critical"
	}
	return summary, nil
}
