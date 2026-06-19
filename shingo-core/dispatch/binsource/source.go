// Package binsource is the pure, ranked bin selector for dedicated home loaders:
// given the bins present across a loader's slot set, pick the one to hand the cell
// for a Want{Payload, Intent}. Pure — no DB, no I/O — so it is exhaustively
// table-testable with plain values; the store-aware gather lives in the dispatch
// package (loader_source.go).
//
// Two rules, nothing more:
//
//   - Eligibility is payload-exact and intent-aware. A demand for part X is only
//     ever satisfied by a bin of X — never a partial of another part Y. Drain
//     (consume) wants a bin HOLDING X (full or partial); Fill (produce) wants a
//     CONTAINER for X (a partial of X to top up, or a fungible empty).
//
//   - Order is plain FIFO of part X — oldest COALESCE(loaded_at, created_at)
//     first. A partial is not special; it is just an older bin of X (its
//     loaded_at survives a return, so FIFO re-consumes a kept partial on its
//     own). The only tier is in Fill: a partial of X is taken before a fresh
//     empty (an empty is not part-X stock, so it is never age-ranked against a
//     partial — empties are fungible, picked stably).
package binsource

import (
	"time"

	"shingocore/domain"
)

// Intent is the sourcing direction. The same selector serves both; Intent only
// decides which bins are eligible (you cannot drain an empty, nor fill a full).
type Intent int

const (
	// Drain (consume): the cell needs PARTS — a full or partial of X (uop > 0).
	Drain Intent = iota
	// Fill (produce): the cell needs a CONTAINER for X — a partial of X to top
	// up, else a fungible empty.
	Fill
)

// Want is a sourcing request: the part X and the direction.
type Want struct {
	Payload string
	Intent  Intent
}

// Cand is one candidate bin, carrying exactly what the selector reads. Populated
// by the caller from bins rows; in tests, constructed directly.
type Cand struct {
	BinID   int64
	Payload string // "" marks an empty bin
	UOP     int    // uop_remaining
	Cap     int    // uop_capacity

	LoadedAt  *time.Time // nil for an empty; FIFO key is COALESCE(LoadedAt, CreatedAt)
	CreatedAt time.Time

	Claimed           bool // claimed_by IS NOT NULL → unavailable
	Locked            bool
	ManifestConfirmed bool             // required only for a FULL source (see eligible)
	Status            domain.BinStatus // rejected set is statusUsable
}

func effectiveTime(c Cand) time.Time {
	if c.LoadedAt != nil {
		return *c.LoadedAt
	}
	return c.CreatedAt
}

func isEmpty(c Cand) bool { return c.Payload == "" }

func isFullOf(c Cand, x string) bool { return c.Payload == x && c.UOP >= c.Cap }

func isPartialOf(c Cand, x string) bool { return c.Payload == x && c.UOP > 0 && c.UOP < c.Cap }

// statusUsable rejects exactly the statuses the concrete-pickup path rejects
// (binresolver.BinUnavailableReason) — maintenance, flagged, retired,
// quality_hold. 'staged' is ACCEPTED (loader slots are sourced as concrete
// pickups), as is 'available'.
func statusUsable(s domain.BinStatus) bool {
	switch s {
	case domain.BinStatusMaintenance, domain.BinStatusFlagged, domain.BinStatusRetired, domain.BinStatusQualityHold:
		return false
	}
	return true
}

// eligible reports whether c can satisfy want: a bin of part X only (Payload == X
// excludes empties and other parts up front), filtered by intent. A FULL must be
// manifest-confirmed to be a ready source; a partial (a known-good returned bin)
// need not be.
func eligible(c Cand, w Want) bool {
	if c.Claimed || c.Locked || !statusUsable(c.Status) {
		return false
	}
	switch w.Intent {
	case Drain:
		if c.Payload != w.Payload || c.UOP <= 0 {
			return false
		}
		return !isFullOf(c, w.Payload) || c.ManifestConfirmed
	case Fill:
		return isPartialOf(c, w.Payload) || isEmpty(c)
	default:
		return false
	}
}

// less reports whether a ranks ahead of b. Part-X bins: FIFO, oldest first. Fill
// puts a partial of X ahead of any empty; empties are fungible, picked stably by
// BinID (not aged).
func less(a, b Cand, w Want) bool {
	if w.Intent == Fill {
		ae, be := isEmpty(a), isEmpty(b)
		if ae != be {
			return be // a (partial of X) before b (empty)
		}
		if ae {
			return a.BinID < b.BinID // empties: grab any, stable
		}
	}
	at, bt := effectiveTime(a), effectiveTime(b)
	if !at.Equal(bt) {
		return at.Before(bt)
	}
	return a.BinID < b.BinID
}

// Source returns the single best eligible candidate, or (zero, false) when none
// is eligible. Pure: no DB, no mutex, no I/O.
func Source(cands []Cand, want Want) (Cand, bool) {
	best, found := Cand{}, false
	for _, c := range cands {
		if !eligible(c, want) {
			continue
		}
		if !found || less(c, best, want) {
			best, found = c, true
		}
	}
	return best, found
}
