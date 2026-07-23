// System-wide bin count for kanban demand math.
//
// This is intentionally separate from PreflightAvailability (which has
// "available for sourcing right now" semantics and excludes staged,
// flagged, maintenance, quality_hold, retired bins as well as bins at
// non-storage nodes). The kanban demand math has a different question:
// "how many bins of this payload are physically in the loop right now,
// regardless of whether they're parked at storage, en route, or staged
// at a consumer line?"
//
// Inclusion policy (decided 2026-05-11 with plant lead): count bins
// anywhere in the active lifecycle — at storage, in transit, staged at
// a consumer line, or being filled at a loader. A bin partway through
// being consumed at the line is still inventory; it can come back as a
// partial.
//
//   - Include  : available, staged — bins still in productive
//                circulation
//   - Exclude  : flagged, maintenance, quality_hold, retired — bins
//                that production can't rely on
//
// Flagged means the operator marked it for investigation; not assumed
// to return. Maintenance and quality_hold are off the line and shouldn't
// be counted as available capacity — production has to plan around them.
// Retired is terminal.
//
// No node filter — bins anywhere count, including the loader itself
// (an empty carrier sitting at the loader still represents capacity).

package service

import (
	"context"
	"fmt"
)

// PayloadSystemCount is the per-payload count returned by SystemBinCount.
// Distinct from PayloadAvailability (preflight) so callers can't confuse
// the two semantics at the type level.
type PayloadSystemCount struct {
	PayloadCode string `json:"payload_code"`
	BinCount    int    `json:"bin_count"`
}

// SystemBinCountResult carries per-payload counts. Payloads with zero
// bins are present in the result with BinCount=0 — callers should not
// assume absence means zero.
type SystemBinCountResult struct {
	Counts []PayloadSystemCount `json:"counts"`
}

// SystemBinCount counts bins per payload across the whole plant,
// excluding states that aren't part of the kanban loop. See file
// comment for the inclusion policy and reasoning.
//
// Empty payload codes in the request are rejected — silently dropping
// would let a typo become a quiet "zero bins" answer.
func (s *InventoryService) SystemBinCount(ctx context.Context, payloads []string) (SystemBinCountResult, error) {
	result := SystemBinCountResult{
		Counts: make([]PayloadSystemCount, 0, len(payloads)),
	}
	if len(payloads) == 0 {
		return result, nil
	}

	counts := make(map[string]int, len(payloads))
	for _, p := range payloads {
		if p == "" {
			return result, fmt.Errorf("system-count: empty payload code in request")
		}
		counts[p] = 0
	}

	// Parameterized IN (...) — same pattern as PreflightAvailability.
	placeholders := make([]byte, 0, len(payloads)*4)
	args := make([]any, 0, len(payloads))
	for i, p := range payloads {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '$')
		placeholders = append(placeholders, []byte(fmt.Sprintf("%d", i+1))...)
		args = append(args, p)
	}

	query := `SELECT payload_code, COUNT(*) AS n
		FROM bins
		WHERE payload_code IN (` + string(placeholders) + `)
		  AND status NOT IN ('flagged', 'maintenance', 'quality_hold', 'retired')
		GROUP BY payload_code`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return result, fmt.Errorf("system-count: query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var code string
		var n int
		if err := rows.Scan(&code, &n); err != nil {
			return result, fmt.Errorf("system-count: scan: %w", err)
		}
		counts[code] = n
	}
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("system-count: rows: %w", err)
	}

	// Preserve request order so the response is stable for callers.
	for _, p := range payloads {
		result.Counts = append(result.Counts, PayloadSystemCount{
			PayloadCode: p,
			BinCount:    counts[p],
		})
	}
	return result, nil
}

// PayloadSystemUOP is the per-payload UOP total returned by
// SystemUOPForPayload. Distinct from PayloadSystemCount (bin-count
// semantics) — callers can't confuse "I have N bins of WIDGET-A" with
// "I have N parts of WIDGET-A in the loop" at the type level.
type PayloadSystemUOP struct {
	PayloadCode string `json:"payload_code"`
	BinUOP      int    `json:"bin_uop"`    // SUM(bins.uop_remaining)
	BucketUOP   int    `json:"bucket_uop"` // SUM(lineside_buckets.qty)
	TotalUOP    int    `json:"total_uop"`  // BinUOP + BucketUOP
}

// SystemUOPForPayloadResult carries per-payload UOP totals.
type SystemUOPForPayloadResult struct {
	Counts []PayloadSystemUOP `json:"counts"`
}

// SystemUOPForPayload returns the total in-loop UOP for each payload —
// the sum of bin remaining-UOP plus the sum of lineside-bucket qty
// (1:1 BOM assumption: bucket.qty IS UOP). This is the value the
// UOP-threshold replenishment monitor compares against
// demand_registry.replenish_uop_threshold to decide whether to fire
// LoopBelowThresholdSignal.
//
// Lifecycle filter on bins mirrors SystemBinCount — bins in flagged,
// maintenance, quality_hold, or retired status are excluded
// (preserves the 2026-05-11 SNF2 fix semantics: production can't rely
// on those bins so they don't count as loop inventory).
//
// Buckets with empty payload_code are excluded — those are orphans
// (claim deleted before the capture event resolved a payload).
// Conservative undercount is preferable to attributing parts to the
// wrong payload.
//
// Empty payload codes in the request are rejected.
func (s *InventoryService) SystemUOPForPayload(ctx context.Context, payloads []string) (SystemUOPForPayloadResult, error) {
	result := SystemUOPForPayloadResult{
		Counts: make([]PayloadSystemUOP, 0, len(payloads)),
	}
	if len(payloads) == 0 {
		return result, nil
	}

	type acc struct {
		bins    int
		buckets int
	}
	counts := make(map[string]*acc, len(payloads))
	for _, p := range payloads {
		if p == "" {
			return result, fmt.Errorf("system-uop: empty payload code in request")
		}
		counts[p] = &acc{}
	}

	placeholders := make([]byte, 0, len(payloads)*4)
	args := make([]any, 0, len(payloads))
	for i, p := range payloads {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '$')
		placeholders = append(placeholders, []byte(fmt.Sprintf("%d", i+1))...)
		args = append(args, p)
	}
	in := string(placeholders)

	// Bin sum. Same lifecycle filter as SystemBinCount.
	binQuery := `SELECT payload_code, COALESCE(SUM(uop_remaining), 0) AS total
		FROM bins
		WHERE payload_code IN (` + in + `)
		  AND status NOT IN ('flagged', 'maintenance', 'quality_hold', 'retired')
		GROUP BY payload_code`
	binRows, err := s.db.QueryContext(ctx, binQuery, args...)
	if err != nil {
		return result, fmt.Errorf("system-uop: bins query: %w", err)
	}
	for binRows.Next() {
		var code string
		var n int
		if err := binRows.Scan(&code, &n); err != nil {
			binRows.Close()
			return result, fmt.Errorf("system-uop: bins scan: %w", err)
		}
		if a, ok := counts[code]; ok {
			a.bins = n
		}
	}
	binRows.Close()
	if err := binRows.Err(); err != nil {
		return result, fmt.Errorf("system-uop: bins rows: %w", err)
	}

	// Bucket sum. Excludes empty payload_code (orphans / pre-upgrade) AND stranded
	// buckets — parts captured under a PRIOR style that the node's current style no
	// longer consumes. A stranded bucket is real inventory but it isn't available to
	// the running style (it gets pulled, not consumed), so counting it toward on-hand
	// inflates the payload's total and suppresses that payload's replenishment (the
	// Springfield 74576 case: a 250-qty stranded bucket kept the total ≥ threshold so no
	// empty was ever sent). Decision (2026-07-23): stranded buckets don't count; active
	// lineside still does.
	//
	// "Stranded" is computed at query time from the plant-claims mirror
	// (process_styles.is_active + style_claims), joined on core_node_name + payload_code
	// — NOT on style_id, because the bucket carries the numeric edge style id while the
	// mirror carries the style NAME (different namespaces, unjoinable). A bucket is
	// stranded iff its node has an active style AND none of that node's active style
	// claims cover the bucket's payload. If the node isn't in the mirror (no active
	// style known — e.g. a not-yet-published or loader node), the bucket is left
	// counted: exclude only what we can POSITIVELY prove stranded, never under-count on
	// a missing mirror.
	bucketQuery := `SELECT lb.payload_code, COALESCE(SUM(lb.qty), 0) AS total
		FROM lineside_buckets lb
		WHERE lb.payload_code IN (` + in + `)
		  AND NOT (
		    EXISTS (
		      SELECT 1 FROM style_claims sc
		      JOIN process_styles ps ON ps.process_id = sc.process_id AND ps.style_id = sc.style_id
		      WHERE sc.core_node_name = lb.core_node_name AND ps.is_active
		    )
		    AND NOT EXISTS (
		      SELECT 1 FROM style_claims sc
		      JOIN process_styles ps ON ps.process_id = sc.process_id AND ps.style_id = sc.style_id
		      WHERE sc.core_node_name = lb.core_node_name AND ps.is_active
		        AND (sc.payload_code = lb.payload_code
		             OR jsonb_exists(sc.allowed_payload_codes::jsonb, lb.payload_code))
		    )
		  )
		GROUP BY lb.payload_code`
	bucketRows, err := s.db.QueryContext(ctx, bucketQuery, args...)
	if err != nil {
		return result, fmt.Errorf("system-uop: buckets query: %w", err)
	}
	for bucketRows.Next() {
		var code string
		var n int
		if err := bucketRows.Scan(&code, &n); err != nil {
			bucketRows.Close()
			return result, fmt.Errorf("system-uop: buckets scan: %w", err)
		}
		if a, ok := counts[code]; ok {
			a.buckets = n
		}
	}
	bucketRows.Close()
	if err := bucketRows.Err(); err != nil {
		return result, fmt.Errorf("system-uop: buckets rows: %w", err)
	}

	for _, p := range payloads {
		a := counts[p]
		result.Counts = append(result.Counts, PayloadSystemUOP{
			PayloadCode: p,
			BinUOP:      a.bins,
			BucketUOP:   a.buckets,
			TotalUOP:    a.bins + a.buckets,
		})
	}
	return result, nil
}
