package domain

import "time"

// PayloadManifestItem is one line of a payload template — a part
// number and how many of it one production cycle consumes, optionally
// tagged with a free-form description. A Payload's full manifest is the
// ordered list of its items (ordered by ID, which is insertion order).
//
// PartsPerCycle is a RATIO, not a count. The number of this part
// physically in a bin is bins.uop_remaining x PartsPerCycle, derived
// wherever it is needed and stored nowhere — see domain.ManifestEntry,
// which deliberately carries no quantity of its own. Very often the
// ratio is 1; it is >1 for a part an assembly uses several of per cycle.
//
// It was called Quantity until v99 renamed it. The value did not change,
// because the value was always the ratio — measured at both plants, 138
// of 144 rows read 1 against capacities up to 18000. What the old name
// invited was reading it as a COUNT, which it never was.
//
// In the store/payloads sub-package this type is aliased as
// payloads.ManifestItem. The domain name is the fully-qualified
// one so it doesn't collide with the bin-side ManifestEntry.
type PayloadManifestItem struct {
	ID            int64     `json:"id"`
	PayloadID     int64     `json:"payload_id"`
	PartNumber    string    `json:"part_number"`
	PartsPerCycle int64     `json:"parts_per_cycle"`
	Description   string    `json:"description"`
	CreatedAt     time.Time `json:"created_at"`
}
