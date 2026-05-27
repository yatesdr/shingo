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
// buckets); epoch labels the bin's load-lifecycle for bins (0 for
// buckets — see ApplyLinesideBucketDelta on Core).
//
// PK is (scope_kind, scope_key, epoch). Per-epoch counters mean a new
// bin load (epoch bump on Core) starts the seq stream at 1, immune
// to any prior-epoch counter drift this Edge instance carried.
//
// Atomic via UPSERT-and-return: INSERT ... ON CONFLICT
// DO UPDATE SET next_seq = next_seq + 1 RETURNING next_seq. SQLite
// serializes the table-level write so concurrent calls advance the
// counter without races.
//
// Survives Edge restarts — the row is durable. Old-epoch rows linger
// after a load-lifecycle bump (cheap: a handful of bytes each), no
// retention sweep required.
func (db *DB) AllocateInventoryDeltaSeq(scopeKind, scopeKey string, epoch int64) (int64, error) {
	var seq int64
	err := db.QueryRow(`
		INSERT INTO inventory_delta_seq (scope_kind, scope_key, epoch, next_seq, updated_at)
		VALUES (?, ?, ?, 1, datetime('now'))
		ON CONFLICT (scope_kind, scope_key, epoch)
		DO UPDATE SET next_seq = next_seq + 1, updated_at = datetime('now')
		RETURNING next_seq`,
		scopeKind, scopeKey, epoch,
	).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("allocate inventory_delta_seq scope=%s/%s epoch=%d: %w",
			scopeKind, scopeKey, epoch, err)
	}
	return seq, nil
}
