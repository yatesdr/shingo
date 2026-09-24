package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"shingo/protocol"
	"shingocore/store/bins"
)

// lineside_divergence.go — the reads and writes behind the lineside checksum
// (seat-count round 1 S2, round 2 S8). The comparison itself is in
// service/lineside_divergence.go; this file only fetches Core's side of one
// report and records the episodes it finds.
//
// COST PER REPORT MESSAGE, whatever its row count: one read of Core's side
// (LinesideCoreSide) and one read of the station's open episodes
// (OpenReportDivergenceKeys). A write happens only when an episode opens or
// closes (RecordReportDivergences). Pinned by
// messaging.TestLinesideReport_StatementsPerEnvelope.

// ExcReportDivergence is the bin_uop_exception kind for a lineside report that
// disagrees with Core's replica. One row is one episode: occurred_at is when it
// opened, recovered_at when a later report from the same station no longer
// showed it. op carries the class (the ReportDivergence* constants in the
// service), actor the station, bin_id the carrier (NULL for a bucket), and
// detail the key and the two sides as first seen.
const ExcReportDivergence = "report_divergence"

// LinesideReportCarrier is one row the comparison needs from the report: a
// bound carrier and the generation the Edge counts it under. The dedup row is
// read for exactly this (bin, epoch).
type LinesideReportCarrier struct {
	BinID int64 `json:"bin_id"`
	Epoch int64 `json:"epoch"`
}

// LinesideReportSeat is one (seat, part) the report carries a bucket figure for.
type LinesideReportSeat struct {
	Node    string `json:"node"`
	Payload string `json:"payload"`
}

// LinesideCoreCarrier is one Core bin the comparison looks at: every carrier
// the report names, and every counted carrier at one of the station's consume
// seats.
type LinesideCoreCarrier struct {
	BinID    int64
	NodeName string
	Payload  string
	Epoch    int64
	UOP      int
	// AtSeat: the bin is at one of the station's consume seats — a node the
	// station has reported (edge_lineside_reports) that an active style claims
	// as a consume node in the plant-claims mirror — AND it counts: it holds a
	// payload and a non-zero count, in a live status (bins.SourceableStatusSQL:
	// available or staged). That allow-list is narrower than the reject-list
	// SystemUOPForPayload still uses (frozen, awaiting its own ruling — see
	// store/bins/one_sourcing_predicate_test.go); a carrier at a seat in any
	// other status is not called unbound.
	AtSeat bool
	// LastSeq is Core's inventory_delta_dedup.last_seq for (station, bin, the
	// epoch the REPORT names), 0 when there is no row. Only meaningful for a
	// carrier the report names.
	LastSeq int64
}

// LinesideCoreSide is Core's side of one report.
type LinesideCoreSide struct {
	Carriers []LinesideCoreCarrier
	// Buckets is Core's lineside_buckets sum for this station per (node, part),
	// for the (node, part) pairs the report carries. A pair with no Core row is
	// absent (and compares as 0).
	Buckets map[LinesideReportSeat]int
}

// LinesideCoreSide reads, in ONE statement, what Core holds for one station's
// report: the carriers the report names with their dedup high-water mark, the
// counted carriers at the station's consume seats, and the bucket mirror for
// the report's (seat, part) pairs. The sets travel as JSON so the statement
// has three parameters however many rows the report has.
func (db *DB) LinesideCoreSide(station string, carriers []LinesideReportCarrier, seats []LinesideReportSeat) (LinesideCoreSide, error) {
	out := LinesideCoreSide{Buckets: map[LinesideReportSeat]int{}}
	if carriers == nil {
		carriers = []LinesideReportCarrier{}
	}
	if seats == nil {
		seats = []LinesideReportSeat{}
	}
	cj, err := json.Marshal(carriers)
	if err != nil {
		return out, fmt.Errorf("lineside core side: encode carriers: %w", err)
	}
	sj, err := json.Marshal(seats)
	if err != nil {
		return out, fmt.Errorf("lineside core side: encode seats: %w", err)
	}
	rows, err := db.Query(`
		WITH r AS (
			SELECT * FROM jsonb_to_recordset($2::jsonb) AS r(bin_id bigint, epoch bigint)
		), seats AS (
			SELECT DISTINCT e.core_node_name AS name
			FROM edge_lineside_reports e
			WHERE e.station = $1
			  AND EXISTS (
				SELECT 1 FROM style_claims sc
				JOIN process_styles ps ON ps.process_id = sc.process_id AND ps.style_id = sc.style_id
				WHERE sc.core_node_name = e.core_node_name AND sc.role = 'consume' AND ps.is_active)
		), counted AS (
			SELECT b.id, COALESCE(n.name, '') AS node, b.payload_code, b.delta_epoch, b.uop_remaining,
			       (n.name IN (SELECT name FROM seats)
			        AND b.uop_remaining <> 0 AND b.payload_code <> ''
			        AND `+bins.SourceableStatusSQL+`) AS at_seat
			FROM bins b
			LEFT JOIN nodes n ON n.id = b.node_id
			WHERE b.id IN (SELECT bin_id FROM r) OR n.name IN (SELECT name FROM seats)
		)
		SELECT 'bin', c.id, c.node, c.payload_code, c.delta_epoch, c.uop_remaining,
		       COALESCE(c.at_seat, false), COALESCE(d.last_seq, 0)
		FROM counted c
		LEFT JOIN r ON r.bin_id = c.id
		LEFT JOIN inventory_delta_dedup d
		       ON d.station = $1 AND d.scope_kind = $4 AND d.scope_key = c.id::text AND d.epoch = r.epoch
		WHERE r.bin_id IS NOT NULL OR COALESCE(c.at_seat, false)
		UNION ALL
		SELECT 'bucket', 0, lb.core_node_name, lb.payload_code, 0, SUM(lb.qty)::int, false, 0
		FROM lineside_buckets lb
		JOIN jsonb_to_recordset($3::jsonb) AS p(node text, payload text)
		  ON p.node = lb.core_node_name AND p.payload = lb.payload_code
		WHERE lb.station = $1
		GROUP BY lb.core_node_name, lb.payload_code`,
		station, string(cj), string(sj), protocol.InvDeltaScopeBin)
	if err != nil {
		return out, fmt.Errorf("lineside core side %s: %w", station, err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var c LinesideCoreCarrier
		if err := rows.Scan(&kind, &c.BinID, &c.NodeName, &c.Payload, &c.Epoch, &c.UOP, &c.AtSeat, &c.LastSeq); err != nil {
			return out, fmt.Errorf("lineside core side %s: scan: %w", station, err)
		}
		if kind == "bucket" {
			out.Buckets[LinesideReportSeat{Node: c.NodeName, Payload: c.Payload}] = c.UOP
			continue
		}
		out.Carriers = append(out.Carriers, c)
	}
	return out, rows.Err()
}

// ReportDivergence is one disagreement the comparison found, keyed so the same
// disagreement seen in the next report is the same episode.
type ReportDivergence struct {
	// Key identifies the episode within its station: class|node|payload|bin.
	Key       string
	Class     string
	BinID     *int64
	Node      string
	Payload   string
	EdgeEpoch *int64
	CoreEpoch *int64
	EdgeCount *int
	CoreCount *int
	// FlushedSeq and CoreLastSeq are the two sides of the in-flight test, kept
	// for the reader of a count episode.
	FlushedSeq  *int64
	CoreLastSeq *int64
}

// reportDivergenceDetail is the detail JSON of a report_divergence row.
type reportDivergenceDetail struct {
	Key         string `json:"key"`
	Node        string `json:"node"`
	EdgeEpoch   *int64 `json:"edge_epoch"`
	CoreEpoch   *int64 `json:"core_epoch"`
	EdgeCount   *int   `json:"edge_count"`
	CoreCount   *int   `json:"core_count"`
	FlushedSeq  *int64 `json:"flushed_seq,omitempty"`
	CoreLastSeq *int64 `json:"core_last_seq,omitempty"`
}

// OpenReportDivergenceKeys returns the station's open report_divergence
// episodes as key → row id. Served by the partial index on
// (kind, occurred_at) WHERE recovered_at IS NULL.
func (db *DB) OpenReportDivergenceKeys(station string) (map[string]int64, error) {
	rows, err := db.Query(`SELECT id, COALESCE(detail->>'key', '') FROM bin_uop_exception
		WHERE kind = $1 AND recovered_at IS NULL AND actor = $2`, ExcReportDivergence, station)
	if err != nil {
		return nil, fmt.Errorf("open report divergences %s: %w", station, err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id int64
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			return nil, fmt.Errorf("open report divergences %s: scan: %w", station, err)
		}
		out[key] = id
	}
	return out, rows.Err()
}

// RecordReportDivergences opens one episode per divergence in open and marks
// every row id in closed recovered, in one transaction, at the report's time on
// Core's clock. A call with nothing to do issues nothing.
func (db *DB) RecordReportDivergences(station string, open []ReportDivergence, closed []int64, at time.Time) error {
	if len(open) == 0 && len(closed) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("record report divergences %s: begin: %w", station, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	for _, d := range open {
		det, err := json.Marshal(reportDivergenceDetail{
			Key: d.Key, Node: d.Node, EdgeEpoch: d.EdgeEpoch, CoreEpoch: d.CoreEpoch,
			EdgeCount: d.EdgeCount, CoreCount: d.CoreCount, FlushedSeq: d.FlushedSeq, CoreLastSeq: d.CoreLastSeq,
		})
		if err != nil {
			return fmt.Errorf("record report divergence %s %s: encode: %w", station, d.Key, err)
		}
		var bin any
		if d.BinID != nil {
			bin = *d.BinID
		}
		if _, err := tx.Exec(`INSERT INTO bin_uop_exception
			(kind, bin_id, payload_code, actor, occurred_at, op, detail)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			ExcReportDivergence, bin, d.Payload, station, at.UTC(), d.Class, det); err != nil {
			return fmt.Errorf("open report divergence %s %s: %w", station, d.Key, err)
		}
	}
	if len(closed) > 0 {
		ids, err := json.Marshal(closed)
		if err != nil {
			return fmt.Errorf("record report divergences %s: encode closes: %w", station, err)
		}
		if _, err := tx.Exec(`UPDATE bin_uop_exception SET recovered_at = $1
			WHERE kind = $2 AND recovered_at IS NULL
			  AND id IN (SELECT jsonb_array_elements_text($3::jsonb)::bigint)`,
			at.UTC(), ExcReportDivergence, string(ids)); err != nil {
			return fmt.Errorf("close report divergences %s: %w", station, err)
		}
	}
	return tx.Commit()
}

// OpenReportDivergence is one open episode as the Inventory page lists it.
type OpenReportDivergence struct {
	ID        int64     `json:"id"`
	Class     string    `json:"class"`
	Station   string    `json:"station"`
	BinID     *int64    `json:"bin_id"`
	BinLabel  string    `json:"bin_label"`
	Node      string    `json:"node"`
	Payload   string    `json:"payload_code"`
	EdgeEpoch *int64    `json:"edge_epoch"`
	CoreEpoch *int64    `json:"core_epoch"`
	EdgeCount *int      `json:"edge_count"`
	CoreCount *int      `json:"core_count"`
	OpenedAt  time.Time `json:"opened_at"`
}

// ListOpenReportDivergences returns every open report_divergence episode,
// oldest first. Blank on a good day.
func (db *DB) ListOpenReportDivergences() ([]OpenReportDivergence, error) {
	rows, err := db.Query(`SELECT e.id, e.op, e.actor, e.bin_id, COALESCE(b.label, ''), e.payload_code,
		       e.occurred_at, COALESCE(e.detail, '{}'::jsonb)
		FROM bin_uop_exception e
		LEFT JOIN bins b ON b.id = e.bin_id
		WHERE e.kind = $1 AND e.recovered_at IS NULL
		ORDER BY e.occurred_at, e.id`, ExcReportDivergence)
	if err != nil {
		return nil, fmt.Errorf("list open report divergences: %w", err)
	}
	defer rows.Close()
	var out []OpenReportDivergence
	for rows.Next() {
		var d OpenReportDivergence
		var bin sql.NullInt64
		var raw []byte
		if err := rows.Scan(&d.ID, &d.Class, &d.Station, &bin, &d.BinLabel, &d.Payload, &d.OpenedAt, &raw); err != nil {
			return nil, fmt.Errorf("list open report divergences: scan: %w", err)
		}
		if bin.Valid {
			v := bin.Int64
			d.BinID = &v
		}
		var det reportDivergenceDetail
		if err := json.Unmarshal(raw, &det); err != nil {
			return nil, fmt.Errorf("list open report divergences: detail of %d: %w", d.ID, err)
		}
		d.Node, d.EdgeEpoch, d.CoreEpoch, d.EdgeCount, d.CoreCount = det.Node, det.EdgeEpoch, det.CoreEpoch, det.EdgeCount, det.CoreCount
		out = append(out, d)
	}
	return out, rows.Err()
}
