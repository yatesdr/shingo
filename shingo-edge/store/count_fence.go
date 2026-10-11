package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// InventoryDeltaNet returns the running net this station has flushed for one
// count scope (inventory_delta_seq.net): the signed sum of every window it has
// put in the outbox for (scopeKind, scopeKey, epoch). Zero when the scope has
// no row (nothing flushed yet). One primary-key read.
//
// Read by the record-count fence (engine fencedCount), inside
// uop.Mutator.WithPending, for the station's side of the same number Core
// reports as AsOfNet.
func (db *DB) InventoryDeltaNet(scopeKind, scopeKey string, epoch int64) (int64, error) {
	var net int64
	err := db.QueryRow(`SELECT net FROM inventory_delta_seq
		WHERE scope_kind = ? AND scope_key = ? AND epoch = ?`,
		scopeKind, scopeKey, epoch).Scan(&net)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read inventory_delta_seq net scope=%s/%s epoch=%d: %w", scopeKind, scopeKey, epoch, err)
	}
	return net, nil
}

// ReleaseCountWithCursor writes the outgoing carrier's count at its release
// (the statement processes.SetRuntimeReleaseCount runs: the count, and
// released_empty when it is 0) and reads the carrier's stream position in the
// same statement: the last seq allocated (next_seq holds the last value
// AllocateInventoryDeltaSeq returned) and the running net of every window
// flushed for (scopeKind, scopeKey, epoch); zero for a scope that has flushed
// nothing. The release's stream fence (Lane H, DECISIONS D11 (i)) is the pair,
// taken in one instant with the write it is as of.
//
// One statement: the cursor rides the count write's RETURNING as two
// primary-key subqueries, so a release that fences costs no statement beyond
// the count write. found is false when the node has no runtime row (nothing
// written, no fence).
func (db *DB) ReleaseCountWithCursor(processNodeID int64, count int, scopeKind, scopeKey string, epoch int64) (seq, net int64, found bool, err error) {
	err = db.QueryRow(`UPDATE process_node_runtime_states SET
		remaining_uop_cached=?, released_empty=?, updated_at=datetime('now')
		WHERE process_node_id=?
		RETURNING
			COALESCE((SELECT next_seq FROM inventory_delta_seq WHERE scope_kind = ? AND scope_key = ? AND epoch = ?), 0),
			COALESCE((SELECT net FROM inventory_delta_seq WHERE scope_kind = ? AND scope_key = ? AND epoch = ?), 0)`,
		count, count == 0, processNodeID,
		scopeKind, scopeKey, epoch, scopeKind, scopeKey, epoch).Scan(&seq, &net)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("release count and cursor node=%d scope=%s/%s epoch=%d: %w", processNodeID, scopeKind, scopeKey, epoch, err)
	}
	return seq, net, true, nil
}
