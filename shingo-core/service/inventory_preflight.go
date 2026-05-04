// Pre-flight inventory check.
//
// Edge calls this before a changeover starts to verify that bins for
// every required payload exist in the AMR supermarket. The same code
// path is intended to serve a planning-mode preview UI later.

package service

import (
	"context"
	"fmt"
)

// PayloadAvailability is the per-payload count returned by PreflightAvailability.
type PayloadAvailability struct {
	PayloadCode string `json:"payload_code"`
	BinCount    int    `json:"bin_count"`
}

// PreflightResult is the rolled-up result for a multi-payload preflight call.
// Missing is the subset of requested payloads with zero available bins —
// it's the "is the changeover safe to start?" signal. Available carries the
// per-payload counts (zero for missing payloads, included for completeness).
type PreflightResult struct {
	Missing   []string              `json:"missing"`
	Available []PayloadAvailability `json:"available"`
}

// PreflightAvailability counts manifest-confirmed, unclaimed bins per
// payload at enabled storage nodes — the same eligibility filter
// FindSourceFIFO uses to pick a source bin. The station argument is
// reserved for future per-station scoping (e.g. zone-restricted
// supermarkets); currently it is plant-wide.
//
// The query mirrors FindSourceFIFO's WHERE clause minus the FIFO ordering
// and limit:
//   - manifest_confirmed = true
//   - claimed_by IS NULL
//   - locked = false
//   - status NOT IN staged/maintenance/flagged/retired/quality_hold
//   - enabled, non-synthetic node
func (s *InventoryService) PreflightAvailability(ctx context.Context, station string, payloads []string) (PreflightResult, error) {
	_ = station // reserved for future per-station scoping
	result := PreflightResult{
		Missing:   []string{},
		Available: make([]PayloadAvailability, 0, len(payloads)),
	}
	if len(payloads) == 0 {
		return result, nil
	}

	// Seed the count map so payloads with zero bins still report 0
	// rather than vanishing entirely.
	counts := make(map[string]int, len(payloads))
	for _, p := range payloads {
		if p == "" {
			return result, fmt.Errorf("preflight: empty payload code in request")
		}
		counts[p] = 0
	}

	// Build a parameterized IN (...) clause. Postgres needs explicit
	// placeholders; ANY($1::text[]) would also work but per-driver
	// support varies — stick with the explicit placeholder pattern.
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

	query := `SELECT b.payload_code, COUNT(*) AS n
		FROM bins b
		JOIN nodes n ON n.id = b.node_id
		WHERE b.payload_code IN (` + string(placeholders) + `)
		  AND b.manifest_confirmed = true
		  AND b.claimed_by IS NULL
		  AND b.locked = false
		  AND b.status NOT IN ('staged', 'maintenance', 'flagged', 'retired', 'quality_hold')
		  AND n.enabled = true
		  AND n.is_synthetic = false
		GROUP BY b.payload_code`

	rows, err := s.db.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return result, fmt.Errorf("preflight: query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var code string
		var n int
		if err := rows.Scan(&code, &n); err != nil {
			return result, fmt.Errorf("preflight: scan: %w", err)
		}
		counts[code] = n
	}
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("preflight: rows: %w", err)
	}

	// Preserve the request order in Available; collect Missing in the
	// same order so the operator UI sees a stable list.
	for _, p := range payloads {
		n := counts[p]
		result.Available = append(result.Available, PayloadAvailability{
			PayloadCode: p,
			BinCount:    n,
		})
		if n == 0 {
			result.Missing = append(result.Missing, p)
		}
	}
	return result, nil
}
