package store

import (
	"fmt"
)

// AllocateInventoryDeltaSeq returns the next monotonically-increasing
// SequenceID for an inventory-delta scope. Phase 1d of the
// bin-as-truth refactor — Edge's at-most-once guarantee at the wire
// requires per-scope monotonic sequence numbers; Core's
// inventory_delta_dedup table uses them to drop replays.
//
// scopeKind ∈ {"bin", "bucket"}; scopeKey is the same stable string
// the dedup table uses (strconv(BinID) for bins; piped composite for
// buckets).
//
// Atomic via UPSERT-and-return: INSERT ... ON CONFLICT
// DO UPDATE SET next_seq = next_seq + 1 RETURNING next_seq. SQLite
// serializes the table-level write so concurrent calls advance the
// counter without races. The returned value is the SequenceID the
// caller should put on the wire.
//
// Survives Edge restarts — the row is durable. After a crash the
// reporter resumes at the next unused id; gaps are acceptable
// (Core's dedup is monotonic-greater-than, not contiguous).
func (db *DB) AllocateInventoryDeltaSeq(scopeKind, scopeKey string) (int64, error) {
	var seq int64
	err := db.QueryRow(`
		INSERT INTO inventory_delta_seq (scope_kind, scope_key, next_seq, updated_at)
		VALUES (?, ?, 1, datetime('now'))
		ON CONFLICT (scope_kind, scope_key)
		DO UPDATE SET next_seq = next_seq + 1, updated_at = datetime('now')
		RETURNING next_seq`,
		scopeKind, scopeKey,
	).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("allocate inventory_delta_seq scope=%s/%s: %w",
			scopeKind, scopeKey, err)
	}
	return seq, nil
}
