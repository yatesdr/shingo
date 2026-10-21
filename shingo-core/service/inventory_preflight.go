// Pre-flight inventory check: the flow composer's draft asks it, through the
// preflight read-ask, whether each part has a bin free and whether it has one
// at all. A changeover start does not ask; it reads the sourcing feed.

package service

import (
	"context"
	"fmt"

	"shingocore/store/bins"
)

// PreflightResult is the result of a multi-payload preflight call, each list
// in request order.
//
// ── MISSING AND ABSENT ARE TWO QUESTIONS ─────────────────────────────────────
//
// Missing is "no bin FREE right now": every bin of the payload may be reserved,
// claimed, locked or staged for somebody else. That is the changeover preflight's
// advisory question, and it is congestion — the bins exist and will come free.
//
// Absent is "no bin of the payload AT ALL": nothing free, reserved or claimed,
// wherever it stands, including on a robot. That is the only answer the Edge's
// dry-source guard may refuse a REQUEST on, because it is the only one with
// nothing to wait for. A payload in Absent is always in Missing.
type PreflightResult struct {
	Missing []string `json:"missing"`
	Absent  []string `json:"absent"`
}

// PreflightAvailability reads, plant-wide, which payloads dispatch could source
// a bin of right now. It composes bins.BinSourceableSQL — the one sourcing
// predicate — so what this promises an operator at changeover is what
// FindSourceFIFO will actually pick, minus the FIFO ordering and limit. Two
// statements: the sourceable payloads and the present ones (presentPayloads).
func (s *InventoryService) PreflightAvailability(ctx context.Context, payloads []string) (PreflightResult, error) {
	result := PreflightResult{
		Missing: []string{},
		Absent:  []string{},
	}
	if len(payloads) == 0 {
		return result, nil
	}

	for _, p := range payloads {
		if p == "" {
			return result, fmt.Errorf("preflight: empty payload code in request")
		}
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

	sourceable, err := s.payloadsWhere(ctx, string(placeholders), args, bins.BinSourceableSQL("b.payload_code"))
	if err != nil {
		return result, fmt.Errorf("preflight: %w", err)
	}
	present, err := s.presentPayloads(ctx, string(placeholders), args)
	if err != nil {
		return result, err
	}

	// Missing and Absent in request order, so the operator UI sees a stable list.
	for _, p := range payloads {
		if !sourceable[p] {
			result.Missing = append(result.Missing, p)
		}
		if !present[p] {
			result.Absent = append(result.Absent, p)
		}
	}
	return result, nil
}

// payloadsWhere is the set of the requested payloads with at least one bin
// matching cond, one statement.
func (s *InventoryService) payloadsWhere(ctx context.Context, placeholders string, args []any, cond string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT b.payload_code
		FROM bins b
		JOIN nodes n ON n.id = b.node_id
		WHERE b.payload_code IN (`+placeholders+`)
		  AND `+cond, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out[code] = true
	}
	return out, rows.Err()
}

// presentPayloads is the set of the requested payloads with a bin that exists
// as stock at all — free, reserved, claimed, locked or staged, and wherever it
// stands, a robot included. Only bins out of service and bins on disabled
// nodes are left out. Not in it is Absent.
//
// NOT a sourcing reader, and it must not become one: it deliberately keeps the
// claimed, locked, reserved and staged bins the sourcing predicate refuses,
// because stock that exists is stock a REQUEST can wait for. It shares only the
// status rule — bins.SourceableStatusSQL, in place of the fourth hand-spelled
// reject-list, which admitted any off-spec status by default.
//
// It drops every "free right now" filter the query above applies, and the
// synthetic-node one with them: a claimed bin riding to its destination stands
// on a synthetic carrier node, and that bin is exactly the stock a REQUEST should
// wait for rather than be refused over.
func (s *InventoryService) presentPayloads(ctx context.Context, placeholders string, args []any) (map[string]bool, error) {
	present, err := s.payloadsWhere(ctx, placeholders, args, bins.BinPresentSQL)
	if err != nil {
		return nil, fmt.Errorf("preflight: presence %w", err)
	}
	return present, nil
}
