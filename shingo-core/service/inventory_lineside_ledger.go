package service

import (
	"context"
	"fmt"
)

// LinesideLedgerByNode returns Core's ledger lineside on-hand UOP per
// core_node_name for one payload:
//
//	SUM(bins.uop_remaining) for bins currently AT that node (same lifecycle
//	filter as SystemUOPForPayload) + the active lineside bucket qty at that node.
//
// It is the per-node "ledger's view of the line" the R1 shadow compares against
// Edge's per-node lineside reports.
//
// NOTE the bucket term here is the RAW active-bucket sum; it does NOT apply the
// stranded-bucket exclusion SystemUOPForPayload uses. During a changeover a
// node's ledger term can therefore differ slightly from that node's contribution
// to the total. The shadow tolerates this — a resulting disagreement is logged,
// never acted on — and keeping this query simple avoids duplicating the stranded
// subquery per node.
func (s *InventoryService) LinesideLedgerByNode(ctx context.Context, payloadCode string) (map[string]int, error) {
	out := map[string]int{}
	if payloadCode == "" {
		return out, nil
	}

	// Bin term per node (bins.node_id -> nodes.name).
	binRows, err := s.db.QueryContext(ctx, `
		SELECT n.name, COALESCE(SUM(b.uop_remaining), 0)
		FROM bins b
		JOIN nodes n ON n.id = b.node_id
		WHERE b.payload_code = $1
		  AND b.status NOT IN ('flagged', 'maintenance', 'quality_hold', 'retired')
		GROUP BY n.name`, payloadCode)
	if err != nil {
		return nil, fmt.Errorf("lineside-ledger bins query: %w", err)
	}
	for binRows.Next() {
		var node string
		var uop int
		if err := binRows.Scan(&node, &uop); err != nil {
			binRows.Close()
			return nil, fmt.Errorf("lineside-ledger bins scan: %w", err)
		}
		out[node] += uop
	}
	binRows.Close()
	if err := binRows.Err(); err != nil {
		return nil, fmt.Errorf("lineside-ledger bins rows: %w", err)
	}

	// Bucket term per node.
	bkRows, err := s.db.QueryContext(ctx, `
		SELECT core_node_name, COALESCE(SUM(qty), 0)
		FROM lineside_buckets
		WHERE payload_code = $1
		GROUP BY core_node_name`, payloadCode)
	if err != nil {
		return nil, fmt.Errorf("lineside-ledger buckets query: %w", err)
	}
	for bkRows.Next() {
		var node string
		var qty int
		if err := bkRows.Scan(&node, &qty); err != nil {
			bkRows.Close()
			return nil, fmt.Errorf("lineside-ledger buckets scan: %w", err)
		}
		out[node] += qty
	}
	bkRows.Close()
	if err := bkRows.Err(); err != nil {
		return nil, fmt.Errorf("lineside-ledger buckets rows: %w", err)
	}

	return out, nil
}
