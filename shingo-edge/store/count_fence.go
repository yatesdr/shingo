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
