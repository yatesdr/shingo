package domain

import "time"

// PayloadManifestItem is one line of a payload template — a part
// number at a given quantity, optionally tagged with a free-form
// description. A Payload's full manifest is the ordered list of its
// items (ordered by ID, which is insertion order).
//
// In the store/payloads sub-package this type is aliased as
// payloads.ManifestItem; the outer store/ package re-exports it as
// store.PayloadManifestItem. The domain name is the fully-qualified
// one so it doesn't collide with the bin-side ManifestEntry.
type PayloadManifestItem struct {
	ID          int64     `json:"id"`
	PayloadID   int64     `json:"payload_id"`
	PartNumber  string    `json:"part_number"`
	Quantity    int64     `json:"quantity"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}
