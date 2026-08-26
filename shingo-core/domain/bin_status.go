package domain

import (
	"database/sql/driver"

	"shingo/protocol"
)

// BinStatus is the typed canonical bin status. Wraps string so it serializes
// natively over JSON / SQL while gaining compile-time distinction from raw
// strings and other enum-shaped string types (order Status, etc.).
//
// Mirrors the protocol.Status pattern (shingo/protocol/status.go) — same
// Scanner/Valuer shape, same advisory CanTransitionTo / IsTerminal helpers
// derived from a single transition table.
type BinStatus string

// Canonical bin status constants. These are the only values the domain
// recognises; SQL CHECK constraints and write-time validation are deferred
// (see also: BinService.ChangeStatus has no validation today, by design —
// operators sometimes need to set off-spec states during incident recovery).
const (
	BinStatusAvailable   BinStatus = "available"
	BinStatusStaged      BinStatus = "staged"
	BinStatusFlagged     BinStatus = "flagged"
	BinStatusMaintenance BinStatus = "maintenance"
	BinStatusQualityHold BinStatus = "quality_hold"
	BinStatusRetired     BinStatus = "retired"
)

// validBinTransitions defines the canonical bin state machine. Advisory
// today — ChangeStatus does not enforce this. The table exists so callers
// that want a guard (e.g. UI confirming a destructive transition, future
// recovery flows) have a single source of truth instead of re-deriving it.
//
// IsTerminal is derived from this table: a status is terminal iff it has
// no key in the map.
var validBinTransitions = map[BinStatus][]BinStatus{
	BinStatusAvailable: {
		BinStatusStaged,
		BinStatusFlagged,
		BinStatusMaintenance,
		BinStatusQualityHold,
		BinStatusRetired,
	},
	BinStatusStaged: {
		BinStatusAvailable,
	},
	BinStatusFlagged: {
		BinStatusAvailable,
		BinStatusRetired,
	},
	BinStatusMaintenance: {
		BinStatusAvailable,
		BinStatusRetired,
	},
	BinStatusQualityHold: {
		BinStatusAvailable,
		BinStatusRetired,
	},
	// BinStatusRetired is terminal — no key in the map.
}

// IsTerminal reports whether the bin status has no outgoing transitions.
func (s BinStatus) IsTerminal() bool {
	_, hasOutgoing := validBinTransitions[s]
	return !hasOutgoing
}

// CanTransitionTo reports whether (s, to) is allowed by the canonical bin
// state machine. Advisory only — the service layer does not enforce this.
func (s BinStatus) CanTransitionTo(to BinStatus) bool {
	allowed, ok := validBinTransitions[s]
	if !ok {
		return false
	}
	for _, t := range allowed {
		if t == to {
			return true
		}
	}
	return false
}

// Sourceable reports whether a bin in this status MAY be taken for a
// pickup/retrieve. 'available' and 'staged' are pickable — loader and lineside
// slots source as concrete pickups, and ApplyBinArrival stages every non-storage
// slot, so refusing 'staged' would refuse every lineside bin.
//
// ALLOW-LIST, DELIBERATELY. The status column carries no CHECK constraint and
// write-time validation is deferred (see the constant block above), so a value
// outside this enum is representable. A reject-list would admit such a value by
// default — a bin in an unrecognised state would stay sourceable, which is the
// wrong default for "should a robot drive to this bin". Anything not named here
// is refused, known or not.
//
// This is the SINGLE membership test behind every sourcing predicate. Its SQL
// twin is bins.SourceableStatusSQL; TestSourceableStatus_GoSQLAgree fails if the
// two ever part. A reader that needs to be STRICTER composes on top of it rather
// than restating the whole rule (e.g. `SourceableStatusSQL AND status <> 'staged'`).
func (s BinStatus) Sourceable() bool {
	switch s {
	case BinStatusAvailable, BinStatusStaged:
		return true
	}
	return false
}

// BlocksPickup is Sourceable's inverse, kept for the call sites that read better
// in the negative (binsource.RejectReason, binresolver.BinUnavailableReason).
func (s BinStatus) BlocksPickup() bool { return !s.Sourceable() }

// String satisfies fmt.Stringer.
func (s BinStatus) String() string { return string(s) }

// Scan implements sql.Scanner. Accepts string or []byte; NULL becomes the
// empty BinStatus. Does not validate against AllBinStatuses() — historical
// rows from retired statuses must still load.
func (s *BinStatus) Scan(v any) error {
	return protocol.ScanEnumNamed(s, v, "domain.BinStatus.Scan")
}

// Value implements driver.Valuer.
func (s BinStatus) Value() (driver.Value, error) {
	return protocol.ValueEnum(s)
}

// AllBinStatuses returns every canonical bin status defined in this
// module, used by table-driven tests that exhaustively cover the
// (from, to) matrix.
func AllBinStatuses() []BinStatus {
	return []BinStatus{
		BinStatusAvailable,
		BinStatusStaged,
		BinStatusFlagged,
		BinStatusMaintenance,
		BinStatusQualityHold,
		BinStatusRetired,
	}
}
