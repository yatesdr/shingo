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
// Inclusion policy (decided 2026-05-11 with plant lead):
//   - Include  : available, staged
//   - Exclude  : flagged, maintenance, quality_hold, retired
//
// A staged bin at the consumer line still represents physical inventory
// (it may even return as a partial bin). A flagged bin is under
// investigation and shouldn't be assumed to come back. Maintenance and
// quality_hold are out of circulation. Retired is terminal.
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

	rows, err := s.db.DB.QueryContext(ctx, query, args...)
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
