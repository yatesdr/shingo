// events.go — persistence for sourceability verdict CHANGES.
//
// SourceabilityMonitor recomputes per-(process, style) sourceability every two
// minutes, already runs the edge-triggered diff (wireChanged), broadcasts the
// result to every Edge — and writes nothing down.
//
// The 2026-07-21 incident's root physical condition was zero system stock on
// 74577-6SA0A.06. ShinGo knew that, continuously, for the whole window, and
// there is no record of it. This table is that record.
//
// What it buys: "SNF2 went unsourceable at 09:14 missing -6SA0B.06, recovered
// 09:41" — the substrate for starvation minutes, and the earliest recorded
// precursor of the amplification class. Combined with order_history.code
// (migration 55), starvation-by-cause becomes a GROUP BY.
//
// EDGE-TRIGGERED, not sampled. Dozens of verdicts recompute per cycle; a row
// is written only when one actually moves. Steady-state write volume is near
// zero, which is why this is a table and not a metrics pipeline.

package sourceability

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"shingo/shared/clock"
	"shingocore/domain"
)

// Event is re-exported from shingocore/domain so www handlers can name it
// without importing this persistence package (the depguard rule).
type Event = domain.SourceabilityEvent

// eventMetadata is the JSONB payload: the parts of the verdict that do not
// deserve a column of their own.
type eventMetadata struct {
	Missing     []string `json:"missing,omitempty"`
	AtRiskLines int      `json:"at_risk_lines,omitempty"`
	PrevStatus  string   `json:"prev_status,omitempty"`
}

// RecordChange writes one verdict change.
//
// prevStatus is "" for a process seen for the first time — a first observation
// IS a change (there was no prior verdict) and is recorded, so the history has
// a defined start rather than beginning at the first flap.
func RecordChange(db *sql.DB, processID, styleID string, s StyleState, prevStatus string) error {
	meta, err := json.Marshal(eventMetadata{
		Missing:     s.Missing,
		AtRiskLines: len(s.AtRisk),
		PrevStatus:  prevStatus,
	})
	if err != nil {
		return fmt.Errorf("marshal sourceability metadata: %w", err)
	}

	missing := ""
	if len(s.Missing) > 0 {
		missing = s.Missing[0]
	}
	reason := ""
	if len(s.Missing) > 0 {
		reason = "missing " + strings.Join(s.Missing, ", ")
	}

	_, err = db.Exec(`INSERT INTO sourceability_events
		(process_key, style_id, payload_code, sourceable, status, reason,
		 missing_payload, op, source, actor, metadata, observed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'sourceability_change','engine/sourceability_monitor.go','system',$8,$9)`,
		processID, styleID, missing, s.Status == StatusGreen, string(s.Status), reason,
		missing, string(meta), clock.Now().UTC())
	if err != nil {
		return fmt.Errorf("record sourceability change %s/%s: %w", processID, styleID, err)
	}
	return nil
}

// ListEvents returns recorded verdict changes since `since`, newest first.
// processID / payload "" mean "all".
func ListEvents(db *sql.DB, since time.Time, processID, payload string, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT id, process_key, style_id, payload_code, sourceable, status, reason,
		 missing_payload, op, source, actor, COALESCE(metadata::text,''), observed_at
		FROM sourceability_events WHERE observed_at >= $1`
	args := []any{since.UTC()}
	if processID != "" {
		args = append(args, processID)
		q += fmt.Sprintf(" AND process_key = $%d", len(args))
	}
	if payload != "" {
		args = append(args, payload)
		q += fmt.Sprintf(" AND missing_payload = $%d", len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY observed_at DESC, id DESC LIMIT $%d", len(args))

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list sourceability events: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.ProcessKey, &e.StyleID, &e.PayloadCode, &e.Sourceable,
			&e.Status, &e.Reason, &e.MissingPayload, &e.Op, &e.Source, &e.Actor,
			&e.Metadata, &e.ObservedAt); err != nil {
			return nil, fmt.Errorf("scan sourceability event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
