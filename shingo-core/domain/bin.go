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
	ID              int64      `json:"id"`
	BinTypeID       int64      `json:"bin_type_id"`
	Label           string     `json:"label"`
	Description     string     `json:"description"`
	NodeID          *int64     `json:"node_id,omitempty"`
	Status          BinStatus  `json:"status"`
	ClaimedBy       *int64     `json:"claimed_by,omitempty"`
	StagedAt        *time.Time `json:"staged_at,omitempty"`
	StagedExpiresAt *time.Time `json:"staged_expires_at,omitempty"`
	PayloadCode     string     `json:"payload_code"`
	Manifest        *string    `json:"manifest,omitempty"`
	UOPRemaining    int        `json:"uop_remaining"`
	// DeltaEpoch labels the current load-lifecycle for this bin.
	// Increments in Core's bin_manifest service on every load boundary
	// (SetForProduction, ClearForReuseTx). Carried on the wire in
	// BinUOPDelta so Core's dedup table can scope replay-protection per
	// load instead of per bin identity — see the v22 migration.
	DeltaEpoch        int64      `json:"delta_epoch"`
	ManifestConfirmed bool       `json:"manifest_confirmed"`
	Locked            bool       `json:"locked"`
	LockedBy          string     `json:"locked_by"`
	LockedAt          *time.Time `json:"locked_at,omitempty"`
	LastCountedAt     *time.Time `json:"last_counted_at,omitempty"`
	LastCountedBy     string     `json:"last_counted_by"`
	LoadedAt          *time.Time `json:"loaded_at,omitempty"`
	AnomalyAt         *time.Time `json:"anomaly_at,omitempty"`
	// AnomalyNote is where the robot carrying this bin last was, recorded when
	// the bin was stranded. Free text for a human: most of finding a stranded
	// bin is the walking, and this turns the search into a map pin. Empty when
	// nothing could be inferred, and on every bin stranded before v96.
	AnomalyNote string    `json:"anomaly_note,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Joined fields
	BinTypeCode string `json:"bin_type_code"`
	NodeName    string `json:"node_name"`
	UOPCapacity int    `json:"uop_capacity,omitempty"` // JOIN from payloads.uop_capacity
	// HasPendingReservation is populated by BinJoinQuery from the reservations
	// table. True when ANY order holds a pending (pre-claim) reservation on this
	// bin — owner-blind, so it may be this order's own hold (the reserve reconcile
	// loads its own holds separately, before consulting this). BinUnavailableReason
	// checks this field so the dispatch loop never offers a reserved bin as a claim
	// candidate. Pending-ONLY is sufficient because a confirmed reservation coincides
	// with a hard claimed_by (structural, since the one-tx claim+confirm moves them
	// together), which the separate claimed_by check already covers.
	HasPendingReservation bool `json:"has_pending_reservation,omitempty"`
}

// ManifestEntry is a single line in a bin's manifest — one CatID / part
// number, optionally tagged with a lot code and free-form notes.
// Marshalled into the bins.manifest JSON column.
//
// IT CARRIES NO QUANTITY, AND THAT IS DELIBERATE. The manifest says WHICH
// parts are in the carrier; how many is bins.uop_remaining x the payload
// template's parts_per_cycle, derived wherever it is needed. It used to
// carry a `qty`, and it was never a usable count: resolveTemplateManifest
// copied the TEMPLATE's per-cycle ratio into it regardless of how full the
// bin actually was, while SyncUOPAndClaim wrote the remaining cycle count.
// Two different quantities under one name, and a reader got whichever writer
// ran last. Nothing rewrote the manifest as UOP drained during production
// either, so whichever number was there went stale on the first consumed
// part.
//
// Do not helpfully add a quantity back. If a workflow ever needs the count
// as it was at LOAD time rather than as it is now, that is template
// versioning, not a field here — a stored copy would go stale the same way.
//
// Historical bins in production still carry a `qty` key. It is ignored on
// read; nothing migrates it, because nothing reads it.
type ManifestEntry struct {
	CatID   string `json:"catid"`
	LotCode string `json:"lot_code,omitempty"`
	Notes   string `json:"notes,omitempty"`
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
