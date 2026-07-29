// ledger_integrity.go — row shapes for the bins UOP ledger forensics.
//
// The queries live in store/bins/ledger_integrity.go, which explains what each
// answers and why negatives are deliberately not clamped away. The types are
// here so www handlers can name them without importing the store.

package domain

import "time"

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
