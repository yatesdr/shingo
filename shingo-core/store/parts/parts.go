// Package parts computes the Operations dashboard "Parts" section metrics
// (plan §3.E): parts produced, cycle time, and consumption. Cross-table reads
// joining mission_telemetry → orders → payloads → payload_manifest (produced
// / cycle time) and cms_transactions → payloads → payload_manifest
// (consumption).
//
// NOTE: §3.E describes the join as "mission_telemetry → payloads", but
// mission_telemetry carries no payload reference — the realistic path is via
// orders.payload_code (Q-014). Cycle time is payload-level attributed to each
// part (the §3.E.2 caveat). Consumption routes through payload_code →
// manifest deterministically, ignoring cms_transactions.cat_id to avoid the
// §8 #15 silent-heuristic trap.
package parts

import (
	"database/sql"
	"fmt"
	"time"
)

// Produced is one part's produced quantity over the window.
type Produced struct {
	PartNumber string `json:"part_number"`
	Qty        int64  `json:"qty"`
	Missions   int64  `json:"missions"`
}

// Cycle is one part's payload-level cycle time over the window.
type Cycle struct {
	PartNumber    string `json:"part_number"`
	AvgDurationMS int64  `json:"avg_duration_ms"`
	P95DurationMS int64  `json:"p95_duration_ms"`
	Missions      int64  `json:"missions"`
}

// Consumption is one part's UoP burn over the window.
type Consumption struct {
	PartNumber string     `json:"part_number"`
	UoP        int64      `json:"uop"`
	Txns       int64      `json:"txns"`
	LastAt     *time.Time `json:"last_at,omitempty"`
}

// dateWhere builds a since/until predicate on the given column.
func dateWhere(col string, since, until *time.Time) (string, []any) {
	var conds []string
	var args []any
	if since != nil {
		args = append(args, *since)
		conds = append(conds, fmt.Sprintf("%s >= $%d", col, len(args)))
	}
	if until != nil {
		args = append(args, *until)
		conds = append(conds, fmt.Sprintf("%s <= $%d", col, len(args)))
	}
	if len(conds) == 0 {
		return "", args
	}
	w := " AND " + conds[0]
	if len(conds) > 1 {
		w += " AND " + conds[1]
	}
	return w, args
}

// GetProduced returns the top-N parts by produced quantity over finished
// missions in the window (plan §3.E.1).
func GetProduced(db *sql.DB, since, until *time.Time, top int) ([]Produced, error) {
	if top <= 0 {
		top = 10
	}
	dw, args := dateWhere("mt.core_completed", since, until)
	q := fmt.Sprintf(`SELECT pm.part_number, COALESCE(SUM(pm.quantity),0), COUNT(DISTINCT mt.order_id)
		FROM mission_telemetry mt
		JOIN orders o ON o.id = mt.order_id
		JOIN payloads p ON p.code = o.payload_code
		JOIN payload_manifest pm ON pm.payload_id = p.id
		WHERE mt.terminal_state IN ('FINISHED','delivered','confirmed') AND pm.part_number <> ''%s
		GROUP BY pm.part_number ORDER BY SUM(pm.quantity) DESC LIMIT %d`, dw, top)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Produced
	for rows.Next() {
		var r Produced
		if err := rows.Scan(&r.PartNumber, &r.Qty, &r.Missions); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetCycleTime returns the top-N parts by average payload-level cycle time
// over finished missions in the window (plan §3.E.2). The duration is the
// carrying payload's, attributed to each part in its manifest.
func GetCycleTime(db *sql.DB, since, until *time.Time, top int) ([]Cycle, error) {
	if top <= 0 {
		top = 10
	}
	dw, args := dateWhere("mt.core_completed", since, until)
	q := fmt.Sprintf(`SELECT pm.part_number,
		COALESCE(AVG(mt.duration_ms),0)::BIGINT,
		COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY mt.duration_ms),0)::BIGINT,
		COUNT(DISTINCT mt.order_id)
		FROM mission_telemetry mt
		JOIN orders o ON o.id = mt.order_id
		JOIN payloads p ON p.code = o.payload_code
		JOIN payload_manifest pm ON pm.payload_id = p.id
		WHERE mt.duration_ms > 0 AND pm.part_number <> ''%s
		GROUP BY pm.part_number ORDER BY AVG(mt.duration_ms) DESC LIMIT %d`, dw, top)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Cycle
	for rows.Next() {
		var r Cycle
		if err := rows.Scan(&r.PartNumber, &r.AvgDurationMS, &r.P95DurationMS, &r.Missions); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetConsumption returns the top-N parts by UoP consumed over the window
// (plan §3.E.3), routing cms_transactions through payload_code → manifest.
// delta is negated into a positive burn (consumption rows carry a negative
// delta); only negative-delta rows count as consumption.
func GetConsumption(db *sql.DB, since, until *time.Time, top int) ([]Consumption, error) {
	if top <= 0 {
		top = 10
	}
	dw, args := dateWhere("t.created_at", since, until)
	q := fmt.Sprintf(`SELECT pm.part_number, COALESCE(SUM(-t.delta),0), COUNT(*), MAX(t.created_at)
		FROM cms_transactions t
		JOIN payloads p ON p.code = t.payload_code
		JOIN payload_manifest pm ON pm.payload_id = p.id
		WHERE t.delta < 0 AND pm.part_number <> ''%s
		GROUP BY pm.part_number ORDER BY SUM(-t.delta) DESC LIMIT %d`, dw, top)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Consumption
	for rows.Next() {
		var r Consumption
		if err := rows.Scan(&r.PartNumber, &r.UoP, &r.Txns, &r.LastAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
