package store

// Delegate file: sourceability verdict-change history lives in
// store/sourceability/. The compute path reads through its own Inputs struct
// and needs no delegate; this is the read/write surface for the persisted
// CHANGES (migration 56).

import (
	"time"

	"shingocore/store/sourceability"
)

// ListSourceabilityEvents returns recorded verdict changes since `since`,
// newest first. processID / payload "" mean "all".
func (db *DB) ListSourceabilityEvents(since time.Time, processID, payload string, limit int) ([]sourceability.Event, error) {
	return sourceability.ListEvents(db.DB, since, processID, payload, limit)
}
