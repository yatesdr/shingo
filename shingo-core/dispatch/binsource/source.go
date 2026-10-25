// Package binsource is the pure, ranked bin selector for dedicated home loaders:
// given the bins present across a loader's slot set, pick the one to hand the cell
// for a Want{Payload, Intent}. Pure — no DB, no I/O — so it is exhaustively
// table-testable with plain values; the store-aware gather lives in the dispatch
// package (loader_source.go).
//
// Five rules, nothing more:
//
//   - Eligibility is payload-exact and intent-aware. A demand for part X is only
//     ever satisfied by a bin of X — never a partial of another part Y. Drain
//     (consume) wants a bin HOLDING X (full or partial); Fill (produce) wants a
//     CONTAINER for X (a partial of X to top up, or a fungible empty).
//
//   - The carrier must be one X may travel in. payload_bin_types is hard where
//     it has rows and silent where it has none, and the resolved set rides in on
//     Want.BinTypes rather than being read here — the package is pure, and the
//     rule is loaded once per call by the store-aware gather.
//
//   - A Fill takes only a carrier its want's height admits (Want.Height,
//     domain.Height.Admits over the candidate's carrier state): tops & bottoms
//     is nobody's ordinary empty, and a want of tops & bottoms alone takes
//     nothing else, a partial of X included. Drain is untouched: it never takes
//     a part-less carrier.
//
//   - Order is plain FIFO of part X — oldest COALESCE(loaded_at, created_at)
//     first. A partial is not special; it is just an older bin of X (its
//     loaded_at survives a return, so FIFO re-consumes a kept partial on its
//     own). The only tier is in Fill: a partial of X is taken before a fresh
//     empty (an empty is not part-X stock, so it is never age-ranked against a
//     partial).
//
//   - Empties are not aged either: a Fill ranks them on its want's height
//     ladder (rank, the Go twin of the empty finders' SQL ladder, held equal
//     by store/bins TestHeightLadder_ThePoolAgreesWithTheFinders), and those on
//     one rung, or under a want that states no layers, are fungible, picked
//     stably by id.
package binsource

import (
	"time"

	"shingo/protocol"
	"shingocore/domain"
)

// Intent is the sourcing direction. The same selector serves both; Intent only
// decides which bins are eligible (you cannot drain an empty, nor fill a full).
type Intent int

const (
	// Drain (consume): the cell needs PARTS — a full or partial of X that holds stock (Cand.Stocked).
	Drain Intent = iota
	// Fill (produce): the cell needs a CONTAINER for X — a partial of X to top
	// up, else a fungible empty.
	Fill
)

// Want is a sourcing request: the part X, the direction, and the carrier types
// X is allowed to travel in.
//
// THE RULE RIDES ON THE WANT because it is a fact about the PAYLOAD, and the
// payload is fixed for a call. The caller loads it once (dispatch/loader_source
// .go) and every candidate is judged against the same value, so the package
// stays pure and the pool scan stays one query. A zero BinTypes is
// unrestricted, which is what a caller that has no payload_bin_types rows — or
// no payload — passes.
type Want struct {
	Payload  string
	Intent   Intent
	BinTypes domain.BinTypeRule
	// Asker is the id of the order asking, 0 when nobody in particular is. A
	// candidate carrying ITS pending reservation is still eligible; one carrying
	// anybody else's is not (B1, Rule 4).
	Asker int64
	// Height is the height a Fill's carrier is wanted at: the zero value (no
	// height said) and a stack never take tops & bottoms, Layers(0) takes any,
	// TopsAndBottomsOnly takes tops & bottoms and nothing else; a Layers want
	// also ranks the empties (rank).
	Height domain.Height
}

// Cand is one candidate bin, carrying exactly what the selector reads. Populated
// by the caller from bins rows; in tests, constructed directly.
type Cand struct {
	BinID     int64
	BinTypeID int64  // bins.bin_type_id — judged against Want.BinTypes
	Payload   string // "" marks an empty bin
	UOP       int    // uop_remaining
	Cap       int    // uop_capacity
	// Stocked is whether the carrier holds stock (domain.Bin.Stocked, the one
	// spelling of "counted to nothing"): read off the row by the caller as
	// State is, so this package compares UOP only with the capacity.
	Stocked bool

	LoadedAt  *time.Time // nil for an empty; FIFO key is COALESCE(LoadedAt, CreatedAt)
	CreatedAt time.Time

	Claimed bool // claimed_by IS NOT NULL → unavailable
	Locked  bool
	// ReservedBy is the order holding a pending reservation on the bin, 0 when
	// none does. Another order's reservation makes the bin unavailable to this
	// want; the asker's own does not.
	ReservedBy int64
	// Bare is bin_types.bare: a cart with no bin on it. Never a container to
	// fill and never stock to drain, so it is rejected before any intent.
	Bare              bool
	ManifestConfirmed bool             // required only for a FULL source (see eligible)
	Status            domain.BinStatus // rejected set is statusUsable
	// State is the cart's carrier state (domain.Bin.CarrierState), judged
	// against Want.Height on a Fill. "" in a test that does not set it reads
	// as no state the height excludes.
	State protocol.CarrierState
	// TotesOnCart and TotesPerLayer are bins.totes_on_cart and its type's
	// bin_types.totes_per_layer (nil: not known, and a type that states none),
	// which place an empty on a Fill's height ladder (rank).
	TotesOnCart, TotesPerLayer *int
}

func effectiveTime(c Cand) time.Time {
	if c.LoadedAt != nil {
		return *c.LoadedAt
	}
	return c.CreatedAt
}

func isEmpty(c Cand) bool { return c.Payload == "" }

func isFullOf(c Cand, x string) bool { return c.Payload == x && c.UOP >= c.Cap }

func isPartialOf(c Cand, x string) bool { return c.Payload == x && c.Stocked && c.UOP < c.Cap }

// eligible reports whether c can satisfy want: a bin of part X only (Payload == X
// excludes empties and other parts up front), filtered by intent, in a carrier X
// is allowed to travel in. A FULL must be manifest-confirmed to be a ready
// source; a partial (a known-good returned bin) need not be. The status
// reject-set is domain.BinStatus.BlocksPickup and the bin-type rule is
// domain.BinTypeRule — the SAME two predicates
// binresolver.BinUnavailableReason uses, so the loader ranker and the
// concrete-node path can no longer drift ('staged'/'available' stay pickable).
func eligible(c Cand, w Want) bool { return RejectReason(c, w) == "" }

// RejectReason is eligible's answer with the WHY attached: "" when the candidate
// can satisfy want, otherwise a short stable tag naming the first rule it failed.
//
// eligible is defined in terms of this rather than the other way round on
// purpose. The decision and the explanation are one implementation, so a
// diagnostic that says "rejected: unconfirmed-full" can never drift from the
// selector that rejected it — the drift this package's own status-predicate
// comment was written to end.
//
// Tags are ordered as the rules are evaluated, so the tag names the FIRST
// failure, not every failure.
func RejectReason(c Cand, w Want) string {
	switch {
	case c.Claimed:
		return "claimed"
	case c.Locked:
		return "locked"
	case c.ReservedBy != 0 && (w.Asker == 0 || c.ReservedBy != w.Asker):
		// OWNER-AWARE (B1, Rule 4): the pool used to be reservation-blind and
		// handed out a bin another order had planned for; the reserve then
		// conflicted and the cell waited beside a free one.
		return "reserved"
	case c.Bare:
		// A bare cart has no payload, which Fill would read as a fungible
		// empty. It never composed bins.EmptyCarrierWhere, so the rule is
		// spelled here (TestDedicatedPool_NeverDrawsABareCart).
		return "bare"
	case c.Status.BlocksPickup():
		return "status:" + string(c.Status)
	}
	switch w.Intent {
	case Drain:
		switch {
		case c.Payload == "":
			return "empty-bin"
		case c.Payload != w.Payload:
			return "payload:" + c.Payload
		case !c.Stocked:
			return "uop<=0"
		case isFullOf(c, w.Payload) && !c.ManifestConfirmed:
			return "unconfirmed-full"
		}
		return binTypeReject(c, w)
	case Fill:
		if !w.Height.Admits(c.State) {
			return "height"
		}
		if isPartialOf(c, w.Payload) || isEmpty(c) {
			return binTypeReject(c, w)
		}
		if c.Payload == w.Payload {
			return "full-not-a-container"
		}
		return "payload:" + c.Payload
	default:
		return "unknown-intent"
	}
}

// binTypeReject is the carrier half of eligibility: this bin holds (or will
// hold) part X, so its type must be one X is allowed to travel in.
//
// IT IS ASKED LAST, at the point each intent has otherwise accepted, and that
// ordering is the reason RejectReason's tags stay readable. A bin of the wrong
// part reports "payload:PART-B", not "bin-type" — the type of a bin nobody
// wanted is not why it was skipped. Only a candidate that passed every other
// rule can be refused for its carrier.
//
// IT BINDS BOTH INTENTS, empties included. A fungible empty picked to be FILLED
// with X becomes a carrier of X the moment the produce leg lands, so a type X
// may not travel in is not a container for X either — refusing it here is the
// same rule the produce-finalize door (store/bins.binManifest) enforces at the
// other end of that trip.
func binTypeReject(c Cand, w Want) string {
	if w.BinTypes.Permits(c.BinTypeID) {
		return ""
	}
	return "bin-type"
}

// less reports whether a ranks ahead of b. Part-X bins: FIFO, oldest first. Fill
// puts a partial of X ahead of any empty; empties are not aged: they rank on the
// want's height ladder (rank), then stably by BinID.
//
// The intent asymmetry is DELIBERATE — do NOT "fix" it by adding a Drain tier (see
// IMPLEMENTATION-PLAN §3.1). The partial-first tier is mandatory only on Fill, where
// a partial competes against EMPTIES whose FIFO key falls back to an ancient,
// immutable created_at (an old tote would otherwise bury a fresh partial). On Drain
// there are no empties — candidates are only full/partial of X, all carrying a real
// loaded_at — and a kept partial keeps its original (older) loaded_at across return,
// so plain FIFO already serves it ahead of any newer bin. A Drain tier would be dead
// code at best, and at worst would consume a NEWER partial before a genuinely OLDER
// full — a FIFO violation. (Holds while consume "drains the bin out of shingo"; a
// future consume-side partial-keep that stages aged fulls would re-open this.)
func less(a, b Cand, w Want) bool {
	if w.Intent == Fill {
		ae, be := isEmpty(a), isEmpty(b)
		if ae != be {
			return be // a (partial of X) before b (empty)
		}
		if ae {
			if ra, rb := rank(w.Height, a.TotesOnCart, a.TotesPerLayer), rank(w.Height, b.TotesOnCart, b.TotesPerLayer); ra != rb {
				return ra.before(rb)
			}
			return a.BinID < b.BinID // empties on one rung: grab any, stable
		}
	}
	at, bt := effectiveTime(a), effectiveTime(b)
	if !at.Equal(bt) {
		return at.Before(bt)
	}
	return a.BinID < b.BinID
}

// rung is an empty's place on a want's height ladder: the three terms the
// finders' SQL ladder sorts by (store/bins preferredEmptyOrder), in its order,
// false and nearer first in each. The SQL's later terms (a reachable slot, the
// shallower depth) are not here: the pool's members are a loader's positions,
// and the pool ranks no accessibility.
type rung struct {
	unknown bool // no count, or a type that states no totes per layer
	short   bool // fewer totes than the want
	off     int  // how far from the want, in totes
}

// rank is where an empty with totesOnCart on it, of a type of totesPerLayer to
// a layer, stands on want's ladder: the Go twin of preferredEmptyOrder's ORDER
// BY, comparing in totes against the empty's own type. A want that states no
// layers (none, TopsAndBottomsOnly) puts every empty on one rung, as the SQL
// appends no ladder for it. At Layers(0) no count is short, so tops & bottoms
// is nearest, then the shortest stack. Held equal to the SQL by
// store/bins TestHeightLadder_ThePoolAgreesWithTheFinders.
func rank(want domain.Height, totesOnCart, totesPerLayer *int) rung {
	layers, ok := want.Stated()
	switch {
	case !ok:
		return rung{}
	case totesOnCart == nil || !protocol.IsToteType(totesPerLayer):
		return rung{unknown: true}
	}
	off := *totesOnCart - layers*(*totesPerLayer)
	if off < 0 {
		return rung{short: true, off: -off}
	}
	return rung{off: off}
}

// before reports whether r ranks ahead of o.
func (r rung) before(o rung) bool {
	switch {
	case r.unknown != o.unknown:
		return o.unknown
	case r.short != o.short:
		return o.short
	}
	return r.off < o.off
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
