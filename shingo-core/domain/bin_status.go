package domain

import (
	"database/sql/driver"
	"fmt"
)

// BinStatus is the typed canonical bin status. Wraps string so it serializes
// natively over JSON / SQL while gaining compile-time distinction from raw
// strings and other enum-shaped string types (order Status, AckOutcome, etc.).
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

// String satisfies fmt.Stringer.
func (s BinStatus) String() string { return string(s) }

// Scan implements sql.Scanner. Accepts string or []byte; NULL becomes the
// empty BinStatus. Does not validate against AllBinStatuses() — historical
// rows from retired statuses must still load.
func (s *BinStatus) Scan(v any) error {
	if v == nil {
		*s = ""
		return nil
	}
	switch x := v.(type) {
	case string:
		*s = BinStatus(x)
	case []byte:
		*s = BinStatus(x)
	default:
		return fmt.Errorf("domain.BinStatus.Scan: cannot scan %T", v)
	}
	return nil
}

// Value implements driver.Valuer.
func (s BinStatus) Value() (driver.Value, error) {
	return string(s), nil
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
