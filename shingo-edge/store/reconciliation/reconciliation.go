// Package reconciliation holds reconciliation/health queries for
// shingo-edge. It surfaces anomalies (stuck orders, unconfirmed
// deliveries) and a summary aggregating outbox + anomaly counts.
//
// Phase 5b of the architecture plan moved this CRUD out of the flat
// store/ package and into this sub-package. The outer store/ keeps
// type aliases (`store.ReconciliationAnomaly = reconciliation.Anomaly`,
// `store.ReconciliationSummary = reconciliation.Summary`) and one-line
// delegate methods on *store.DB so external callers see no API change.
//
// Note: this package imports shingoedge/store/messaging to share the
// MaxRetries const used to compute pending vs. dead-letter counts.
// Phase 6.0c renamed the package from outbox to messaging; the table
// name on disk is still `outbox`. The depguard cross-aggregate
// allow-list permits this single import.
package reconciliation

import (
	"database/sql"
	"fmt"
	"time"

	"shingo/protocol"
	"shingoedge/store/internal/helpers"
	"shingoedge/store/messaging"
)

// criticalOutboxAge is the age at which an unsent outbox message
// promotes the overall status from "degraded" to "critical".
const criticalOutboxAge = 5 * time.Minute

// Anomaly is one observed reconciliation issue.
type Anomaly struct {
	Severity          string     `json:"severity"`
	Issue             string     `json:"issue"`
	RecommendedAction string     `json:"recommended_action,omitempty"`
	OrderID           *int64     `json:"order_id,omitempty"`
	OrderUUID         string     `json:"order_uuid,omitempty"`
	Status            string     `json:"status,omitempty"`
	ObservedAt        *time.Time `json:"observed_at,omitempty"`
	Detail            string     `json:"detail,omitempty"`
}

// Summary aggregates anomaly + outbox counts and a derived overall
// status.
type Summary struct {
	TotalAnomalies       int        `json:"total_anomalies"`
	StuckOrders          int        `json:"stuck_orders"`
	DeliveredUnconfirmed int        `json:"delivered_unconfirmed"`
	OutboxPending        int        `json:"outbox_pending"`
	OldestOutboxAt       *time.Time `json:"oldest_outbox_at,omitempty"`
	DeadLetters          int        `json:"dead_letters"`
	Status               string     `json:"status"`
}

// ListAnomalies returns anomalies for stuck active orders and for
// orders that have been "delivered" but never confirmed within the
// edge thresholds.
func ListAnomalies(db *sql.DB) ([]*Anomaly, error) {
	var anomalies []*Anomaly

	// Edge mirrors Core's full status vocabulary: sourcing/dispatched/faulted
	// are stored on the Edge row, so the IN list matches what edge data actually
	// carries. Safe to splice the shared predicate.
	//
	// ── TWO THRESHOLDS, PICKED PER ROW BY QUEUE CODE ─────────────────────
	//
	// Mirroring Core's split (shingo-core/store/reconciliation: stuckOrderAge vs
	// materialWaitAge). An order waiting on MATERIAL is waiting by definition —
	// flagging it on the same half-hour bound as a dispatched leg that has stopped
	// moving turns the anomaly board into noise, and a board people ignore is
	// worse than the silence that preceded it.
	//
	// It was keyed on `status = 'queued'`, with `sourcing` deliberately kept on the
	// short bound "because it is meant to be transient, so resting there IS the
	// anomaly". That stopped being true: complex orders rest in `sourcing` while
	// they re-shop, and are now BORN there. Keyed on the rung, this board would
	// have started calling every ordinary complex material wait an anomaly at
	// thirty minutes.
	//
	// ── AND IT KEYS ON THE CODE, NOT THE CAUSE, BECAUSE THE CAUSE DOES NOT
	//    CROSS THE WIRE ─────────────────────────────────────────────────────
	//
	// queue_cause is engineer-facing and Core-only; queue_code is the coarse
	// category Core pushes (v28). So this is a COARSER instrument than Core's by
	// construction, and knowingly: `waiting_for_material` covers both the real
	// shortages and the fail-closed family (read-failed, claim-failed), which Core
	// separates by cause and the Edge cannot. The consequence is stated rather
	// than hidden — an outage on an Edge order gets two hours of quiet here where
	// Core would raise it at thirty minutes, and Core's board is the one to read
	// for that distinction.
	rows, err := db.Query(fmt.Sprintf(`SELECT id, uuid, status, updated_at
		FROM orders
		WHERE status IN (%s)
		  AND updated_at < datetime('now',
		        CASE WHEN queue_code = ? THEN ? ELSE ? END)
		ORDER BY updated_at ASC`, protocol.RuntimeStuckCandidateStatusSQLList()),
		string(protocol.QueueWaitingForMaterial), "-2 hours", "-30 minutes")
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
		observed := helpers.ScanTime(updatedAt)
		anomalies = append(anomalies, &Anomaly{
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
		observed := helpers.ScanTime(updatedAt)
		anomalies = append(anomalies, &Anomaly{
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

// GetSummary returns a Summary with anomaly + outbox counts and a
// derived overall status.
func GetSummary(db *sql.DB) (*Summary, error) {
	anomalies, err := ListAnomalies(db)
	if err != nil {
		return nil, err
	}
	summary := &Summary{TotalAnomalies: len(anomalies)}

	// MIN(created_at) returns NULL when no rows match, so scan into NullString.
	row := db.QueryRow(`SELECT COUNT(*), MIN(created_at) FROM outbox WHERE sent_at IS NULL AND retries < ?`, messaging.MaxRetries)
	var oldest sql.NullString
	if err := row.Scan(&summary.OutboxPending, &oldest); err != nil {
		return nil, err
	}
	if oldest.Valid && oldest.String != "" {
		t := helpers.ScanTime(oldest.String)
		summary.OldestOutboxAt = &t
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE sent_at IS NULL AND retries >= ?`, messaging.MaxRetries).Scan(&summary.DeadLetters); err != nil {
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
	} else if summary.OldestOutboxAt != nil && time.Since(summary.OldestOutboxAt.UTC()) >= criticalOutboxAge {
		summary.Status = "critical"
	}
	return summary, nil
}
