// ledger_integrity.go — row shapes for the bins UOP ledger forensics.
//
// The queries live in store/bins/ledger_integrity.go, which explains what each
// answers and why negatives are deliberately not clamped away. The types are
// here so www handlers can name them without importing the store.

package domain

import "time"

// DeltaIntegrity is one payload's delta-drop total over a window, set beside
// that payload's current plant-wide ledger total.
//
// The comparison IS the panel. If 74577-6SA0A.06 reads -443 in-loop and shows
// ~443 UOP of dropped credits over the same window, the mechanism behind the
// negative count is visible on sight rather than hypothesised.
//
// TWO FIGURES, AND THEY DO NOT MIX. UOPLost counts only the two DROP ops.
// MixedContents counts payload_rebound_with_inventory and carries NO UOP total,
// because a rebind is not a drop: the applier rebinds the payload and APPLIES
// the delta ("counting CONTINUES ... the bin is anomaly-flagged for a later
// cycle count of the mixed contents"). Summing it into UOP lost would inflate
// the number and corrupt the one comparison this panel exists to make.
type DeltaIntegrity struct {
	PayloadCode string `json:"payload_code"`

	// UOPLost is the NET effect of dropped deltas on the ledger: how much the
	// count reads BELOW reality because of them. Positive means dropped credits
	// dominate, which is the shape that produces a negative in-loop total.
	//
	// Net, not magnitude, because the sign is what makes it comparable to the
	// ledger total sitting next to it. The two directions are broken out below
	// for anyone who needs to see how it was arrived at.
	UOPLost int `json:"uop_lost"`
	// CreditsDropped is the units of INBOUND count that never landed — the
	// ledger reads low by this much.
	CreditsDropped int `json:"credits_dropped"`
	// ConsumesDropped is the units of consumption that never landed — the
	// ledger reads high by this much.
	ConsumesDropped int `json:"consumes_dropped"`
	// DropRows is how many deltas were dropped, across both ops.
	DropRows int `json:"drop_rows"`
	// StaleEpochRows / PayloadMismatchRows split DropRows by cause. Two
	// different bugs; a panel that says only "42 drops" cannot tell you which.
	StaleEpochRows      int `json:"stale_epoch_rows"`
	PayloadMismatchRows int `json:"payload_mismatch_rows"`

	// MixedContents is how many bins had their payload rebound while holding
	// units under the old label. A COUNT ONLY — no UOP total, deliberately.
	// Each needs a cycle count; none is a loss.
	MixedContents int `json:"mixed_contents"`

	// LedgerTotal is the payload's current plant-wide in-loop total, negative
	// when the ledger is broken. The number UOPLost is meant to be read
	// against.
	LedgerTotal int `json:"ledger_total"`
	// Bins is how many distinct bins are involved in the drops.
	Bins int `json:"bins"`
	// FirstAt / LastAt bracket the drops in the window.
	FirstAt *time.Time `json:"first_at,omitempty"`
	LastAt  *time.Time `json:"last_at,omitempty"`
}

// DeltaDay is one plant-day of dropped deltas: the net effect on the ledger,
// and how it was arrived at.
//
// WHY A DAILY SERIES AND NOT A WINDOW TOTAL. The drops are EPISODIC, not a
// rate. Springfield, 30 days to 2026-07-29: nine days with any drops at all,
// and 2026-07-21 alone carries -27,011 of the -35,432 total — 76% of it, from
// 2,875 rows averaging ~9.4 UoP each against roughly 1 on every other day.
// That day is the already-root-caused zero-system-stock cascade. A single
// 7-day total renders that spike and a quiet Tuesday as one number, which is
// the reading that makes an incident look like a trend and a trend look like
// an incident.
//
// SIGN. NetDelta is the sum of the dropped deltas themselves, so it is
// NEGATIVE when consumption failed to land (the count reads HIGH) and POSITIVE
// when credit failed to land (the count reads LOW). It is not an absolute
// value: the direction is the finding, and flattening it is how a panel ends
// up asserting that dropped consumes explain a negative ledger.
type DeltaDay struct {
	// Day is midnight local plant time. Local, not UTC, because a shift is the
	// unit a reader is comparing against and a UTC day splits one in two.
	Day time.Time `json:"day"`
	// NetDelta is the signed sum. See the sign note above.
	NetDelta int `json:"net_delta"`
	// DropRows is how many deltas were dropped that day.
	DropRows int `json:"drop_rows"`
	// StaleEpochRows / PayloadMismatchRows split DropRows by cause. The mix
	// moves: Springfield's 07-20 was 953 mismatch against 12 stale, its 07-23
	// was 2,414 stale against 328 mismatch. One number cannot show that.
	StaleEpochRows      int `json:"stale_epoch_rows"`
	PayloadMismatchRows int `json:"payload_mismatch_rows"`
	// Payloads / Bins is how wide the day's damage was. One payload at 438 rows
	// is a different event from seven payloads at 60 each.
	Payloads int `json:"payloads"`
	Bins     int `json:"bins"`
}

type NegativeExcursion struct {
	BinID       int64  `json:"bin_id"`
	PayloadCode string `json:"payload_code"`
	Label       string `json:"label"`
	NodeName    string `json:"node_name"`
	// CrossedAt is the instant of the delta that took the bin below zero.
	CrossedAt time.Time `json:"crossed_at"`
	// BeforeUOP / AfterUOP bracket that delta. before >= 0 > after is what
	// makes it a crossing rather than a continuation.
	BeforeUOP int `json:"before_uop"`
	AfterUOP  int `json:"after_uop"`
	// Deepest is the most negative value reached before recovery.
	Deepest int `json:"deepest"`
	// RecoveredAt is when the bin next read >= 0. Zero means STILL NEGATIVE —
	// the exception-list case.
	RecoveredAt *time.Time `json:"recovered_at,omitempty"`
	// Op / Source / Actor / Metadata describe the delta that crossed. Metadata
	// carries the reason and sequence_id the applier writes.
	Op       string `json:"op"`
	Source   string `json:"source"`
	Actor    string `json:"actor"`
	Metadata string `json:"metadata"`
	// PrecededByRelease is the hypothesis flag. See NegativeExcursions.
	PrecededByRelease bool `json:"preceded_by_release"`
}

type OpenNegativeBin struct {
	BinID        int64  `json:"bin_id"`
	Label        string `json:"label"`
	PayloadCode  string `json:"payload_code"`
	NodeName     string `json:"node_name"`
	UOPRemaining int    `json:"uop_remaining"`
	// NegativeSince is when it last crossed zero, from bin_uop_ledger. Nil when
	// the crossing predates the audit window.
	NegativeSince *time.Time `json:"negative_since,omitempty"`
	LastCountedAt *time.Time `json:"last_counted_at,omitempty"`
}

// CarrierBinding is one carrier and the binding ShinGo currently believes about
// it: which payload, since when, and what the ledger reads against it.
//
// A BINDING IS THE GRAIN 5.11's LEDGER HALF IS MEASURED ON, and it is not the
// same thing as a negative excursion. A binding starts when an epoch-bumping op
// retires the previous one (audit.EpochBumpOps) and runs until the next; the
// count ShinGo accepts during it is counted against one belief about one carrier.
// Its AGE is how long that belief has stood unrefreshed, which is the axis that
// says "this count has had time to drift" — a claim about ShinGo's knowledge,
// not about the shelf.
//
// EVERY ABSENCE HERE IS A POINTER, NEVER A ZERO. Four separate things can be
// unknown on one row and they mean four different things: no payload bound at
// all, a bound payload whose bind predates the audit trail, a payload with no
// capacity recorded, and a carrier nobody has ever counted. A plain int or a
// zero time would flatten all four into "0", and the number doctrine's rule 4
// says the fix belongs at the type — so it is here and not in the renderer.
type CarrierBinding struct {
	BinID int64  `json:"bin_id"`
	Label string `json:"label"`

	// PayloadCode is "" when the carrier holds no payload. That is NOT a missing
	// reading: an empty carrier has no binding, so it has no binding age, and it
	// is not a stale-binding candidate however long it has sat there.
	PayloadCode string `json:"payload_code"`
	NodeName    string `json:"node_name"`

	// UOPRemaining is the live ledger. Always a real measured number INCLUDING
	// ZERO and including negatives, which are deliberately never clamped — see
	// store/bins/ledger_integrity.go.
	UOPRemaining int `json:"uop_remaining"`

	// UOPCapacity is the bound payload's nominal units per carrier, nil when the
	// payload has none recorded (payloads.uop_capacity <= 0, which
	// BinService.RecordCount already guards against as un-countable). Without it
	// a negative cannot be sized in binloads at all, and the surface says so
	// rather than dividing by zero or by one.
	UOPCapacity *int `json:"uop_capacity,omitempty"`

	// BoundAt is when the current binding started — the newest EpochBumpOps row
	// for this carrier. Nil means the audit trail holds no boundary for it: the
	// bind predates retention, or the carrier has never been through one. NOT
	// "just now", and NOT "forever ago"; unknown.
	BoundAt *time.Time `json:"bound_at,omitempty"`

	// LastCountedAt is the last physical cycle count. Nil means never counted,
	// which is a measured fact about the carrier and not a read failure.
	LastCountedAt *time.Time `json:"last_counted_at,omitempty"`

	// AnomalyAt is the applier's visibility flag: this carrier has had deltas
	// refused or a payload rebound over inventory. Nil means no anomaly recorded.
	// It gates nothing (see uop/applier.go) and it is shown here as corroboration
	// only.
	AnomalyAt *time.Time `json:"anomaly_at,omitempty"`
}

// BindingAge returns how long the current binding has stood, and whether that is
// knowable at all.
//
// The bool is the whole point of the signature: a (0, false) that a caller can
// ignore would let "we have no boundary row for this carrier" render as a
// freshly-bound carrier, which is the reassuring reading and the wrong one.
func (b CarrierBinding) BindingAge(now time.Time) (time.Duration, bool) {
	if b.PayloadCode == "" || b.BoundAt == nil {
		return 0, false
	}
	d := now.Sub(*b.BoundAt)
	if d < 0 {
		// Clock skew between the audit writer and the reader. Clamp for
		// arithmetic; the caller says so in the title rather than the number
		// lying about the direction.
		d = 0
	}
	return d, true
}

type RecordAccuracy struct {
	// BinsWithCount / BinsNeverCounted split the population.
	BinsWithCount    int `json:"bins_with_count"`
	BinsNeverCounted int `json:"bins_never_counted"`
	// StaleBins is how many carry a count older than the caller's threshold.
	StaleBins int `json:"stale_bins"`
	// OldestCountDays is the age of the oldest count still in service.
	OldestCountDays int `json:"oldest_count_days"`
	// Corrections over the window, and the absolute size of the average one.
	// A large mean correction means the ledger and the shelf disagree by a lot
	// whenever anyone checks — which is the accuracy number, not the count.
	Corrections       int     `json:"corrections"`
	MeanAbsCorrection float64 `json:"mean_abs_correction"`
	LargestCorrection int     `json:"largest_correction"`
}

// Duration returns how long the bin stayed under zero, or the time since
// crossing when it never recovered.
func (e NegativeExcursion) Duration(now time.Time) time.Duration {
	if e.RecoveredAt != nil {
		return e.RecoveredAt.Sub(e.CrossedAt)
	}
	return now.Sub(e.CrossedAt)
}
