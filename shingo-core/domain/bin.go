package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// Bin is a physical container that holds a payload at a node. Its
// fields mirror the bins table plus two joined columns (BinTypeCode,
// NodeName) that every read-path SELECT in store/bins pulls through a
// JOIN — they ride along with the struct so callers don't have to look
// them up separately.
type Bin struct {
	ID                int64      `json:"id"`
	BinTypeID         int64      `json:"bin_type_id"`
	Label             string     `json:"label"`
	Description       string     `json:"description"`
	NodeID            *int64     `json:"node_id,omitempty"`
	Status            string     `json:"status"`
	ClaimedBy         *int64     `json:"claimed_by,omitempty"`
	StagedAt          *time.Time `json:"staged_at,omitempty"`
	StagedExpiresAt   *time.Time `json:"staged_expires_at,omitempty"`
	PayloadCode       string     `json:"payload_code"`
	Manifest          *string    `json:"manifest,omitempty"`
	UOPRemaining      int        `json:"uop_remaining"`
	ManifestConfirmed bool       `json:"manifest_confirmed"`
	Locked            bool       `json:"locked"`
	LockedBy          string     `json:"locked_by"`
	LockedAt          *time.Time `json:"locked_at,omitempty"`
	LastCountedAt     *time.Time `json:"last_counted_at,omitempty"`
	LastCountedBy     string     `json:"last_counted_by"`
	LoadedAt          *time.Time `json:"loaded_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	// Joined fields
	BinTypeCode string `json:"bin_type_code"`
	NodeName    string `json:"node_name"`
}

// ManifestEntry is a single line in a bin's manifest — one CatID /
// part number at a given quantity, optionally tagged with a lot code
// and free-form notes. Marshalled into the bins.manifest JSON column.
type ManifestEntry struct {
	CatID    string `json:"catid"`
	Quantity int64  `json:"qty"`
	LotCode  string `json:"lot_code,omitempty"`
	Notes    string `json:"notes,omitempty"`
}

// Manifest is the parsed form of a Bin.Manifest JSON field — a flat
// list of ManifestEntry. Stored as a single JSON string column; the
// split into items is a domain concern, not a DB-layer one.
type Manifest struct {
	Items []ManifestEntry `json:"items"`
}

// ParseManifest decodes the bin's manifest JSON into a Manifest. A
// nil or empty manifest pointer returns an empty, non-nil Manifest so
// callers can append to m.Items unconditionally.
//
// This method lives on domain.Bin because it reads only the Bin's own
// Manifest field and uses no external state — decoding is pure data,
// not persistence.
func (b *Bin) ParseManifest() (*Manifest, error) {
	if b.Manifest == nil || *b.Manifest == "" {
		return &Manifest{}, nil
	}
	var m Manifest
	if err := json.Unmarshal([]byte(*b.Manifest), &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}
