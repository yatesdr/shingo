package domain

import "time"

// Payload is a template describing a kind of content a bin can hold:
// a unique Code (matched against Bin.PayloadCode on retrieve), a
// human-readable Description, and a UOPCapacity — the number of
// unit-of-production "slots" a fresh bin holds. The actual line items
// that make up a full bin are on PayloadManifestItem, keyed back to
// the Payload via PayloadID.
type Payload struct {
	ID          int64     `json:"id"`
	Code        string    `json:"code"`
	Description string    `json:"description"`
	UOPCapacity int       `json:"uop_capacity"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}
