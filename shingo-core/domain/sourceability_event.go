// sourceability_event.go — the row shape for persisted sourceability verdict
// changes (migration 56).
//
// The queries live in store/sourceability/events.go, which explains why only
// CHANGES are recorded and what the table buys. The type is here so www
// handlers can name it without importing the store.

package domain

import "time"

// SourceabilityEvent is one recorded sourceability verdict change. Column vocabulary follows
// bin_uop_ledger (op / source / actor / metadata) rather than inventing a new
// one — that table is the best-designed shape in this schema and new tables
// converge on it.
type SourceabilityEvent struct {
	ID          int64  `json:"id"`
	ProcessKey  string `json:"process_key"`
	StyleID     string `json:"style_id"`
	PayloadCode string `json:"payload_code"`
	Sourceable  bool   `json:"sourceable"`
	Status      string `json:"status"`
	Reason      string `json:"reason"`
	// MissingPayload is the FIRST missing payload, denormalised out of the
	// metadata so it can be indexed and grouped. The full list is in Metadata;
	// this is the one an operator is told to go stock.
	MissingPayload string    `json:"missing_payload"`
	Op             string    `json:"op"`
	Source         string    `json:"source"`
	Actor          string    `json:"actor"`
	Metadata       string    `json:"metadata"`
	ObservedAt     time.Time `json:"observed_at"`
}
