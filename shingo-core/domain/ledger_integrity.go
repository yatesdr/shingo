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
	// NegativeSince is when it last crossed zero, from bin_uop_audit. Nil when
	// the crossing predates the audit window.
	NegativeSince *time.Time `json:"negative_since,omitempty"`
	LastCountedAt *time.Time `json:"last_counted_at,omitempty"`
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
