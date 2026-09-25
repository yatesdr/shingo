package store

import "fmt"

// AllocateInventoryDeltaSeq returns the next monotonically-increasing
// SequenceID for an inventory-delta scope, and the scope's running net after
// adding netAdd to it.
//
// scopeKind is "bin" here; scopeKey is the same stable string Core's dedup
// table uses (strconv(BinID)); epoch labels the bin's load-lifecycle. A pile
// level has no net and allocates through AllocateInventoryLevelSeq.
//
// PK is (scope_kind, scope_key, epoch). Per-epoch rows mean a new bin load
// (epoch bump on Core) starts the seq stream at 1 and the net at 0.
//
// THE NET RIDES THE SEQ STATEMENT. net is the signed sum of every delta this
// Edge has flushed for the scope; the flush puts it on the message, and Core
// applies it minus what it has already applied, so a message lost, reordered or
// muted by Core's high-water mark is healed by the next one (SYNTH-round2 §3).
// Adding it here, in the UPSERT that already returns the seq, is what keeps the
// running total free on the Pi: still one statement per flushed scope.
//
// The caller passes only what this row has not already absorbed. A flush whose
// outbox INSERT fails after this statement succeeded has burned the seq and
// added its delta to net; the accumulator remembers that and adds only the new
// part on the retry (see accumulator.flushBins).
//
// Survives Edge restarts — the row is durable. Old-epoch rows linger
// after a load-lifecycle bump (cheap: a handful of bytes each), no
// retention sweep required.
func (db *DB) AllocateInventoryDeltaSeq(scopeKind, scopeKey string, epoch, netAdd int64) (seq, net int64, err error) {
	err = db.QueryRow(`
		INSERT INTO inventory_delta_seq (scope_kind, scope_key, epoch, next_seq, net, updated_at)
		VALUES (?, ?, ?, 1, ?, datetime('now'))
		ON CONFLICT (scope_kind, scope_key, epoch)
		DO UPDATE SET next_seq = next_seq + 1, net = net + excluded.net, updated_at = datetime('now')
		RETURNING next_seq, net`,
		scopeKind, scopeKey, epoch, netAdd,
	).Scan(&seq, &net)
	if err != nil {
		return 0, 0, fmt.Errorf("allocate inventory_delta_seq scope=%s/%s epoch=%d: %w",
			scopeKind, scopeKey, epoch, err)
	}
	return seq, net, nil
}

// AllocateInventoryLevelSeq returns the next SequenceID for a scope whose
// messages carry a whole level rather than a delta (a lineside pile's row), so
// there is no running net to advance: the level itself is what heals a lost or
// reordered message. One UPSERT on the same table and key shape as the bin
// scopes, at epoch 0.
func (db *DB) AllocateInventoryLevelSeq(scopeKind, scopeKey string) (int64, error) {
	var seq int64
	err := db.QueryRow(`
		INSERT INTO inventory_delta_seq (scope_kind, scope_key, epoch, next_seq, updated_at)
		VALUES (?, ?, 0, 1, datetime('now'))
		ON CONFLICT (scope_kind, scope_key, epoch)
		DO UPDATE SET next_seq = next_seq + 1, updated_at = datetime('now')
		RETURNING next_seq`,
		scopeKind, scopeKey,
	).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("allocate inventory_delta_seq scope=%s/%s: %w", scopeKind, scopeKey, err)
	}
	return seq, nil
}
