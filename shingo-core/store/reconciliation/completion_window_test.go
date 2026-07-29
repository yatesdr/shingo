package reconciliation

// Internal test: countRecent is the whole of the windowing rule and it is a
// pure function, so it is exercised here rather than behind a docker tag. The
// query that feeds it is covered in reconciliation_test.go.

import (
	"testing"
	"time"
)

func TestCountRecent_ExcludesAnomaliesOlderThanTheWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	at := func(d time.Duration) *time.Time {
		v := now.Add(d)
		return &v
	}

	// The Springfield shape that motivated the window: ten anomalies, every one
	// four months old, latching the verdict red while nothing was wrong.
	old := make([]*CompletionAnomaly, 0, 10)
	for i := range 10 {
		old = append(old, &CompletionAnomaly{OrderID: int64(i), ObservedAt: at(-120 * 24 * time.Hour)})
	}
	if got := countRecent(old, now, CompletionAnomalyWindow); got != 0 {
		t.Fatalf("ten four-month-old anomalies: got %d in window, want 0", got)
	}

	// And the case the window must NOT suppress: something wrong right now.
	fresh := []*CompletionAnomaly{
		{OrderID: 100, ObservedAt: at(-time.Hour)},
		{OrderID: 101, ObservedAt: at(-23 * time.Hour)},
	}
	if got := countRecent(append(old, fresh...), now, CompletionAnomalyWindow); got != 2 {
		t.Fatalf("two fresh among ten stale: got %d, want 2", got)
	}
}

func TestCountRecent_BoundaryAndMissingTimestamp(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time {
		v := now.Add(d)
		return &v
	}

	// Exactly at the cutoff is OUT — the window is "after the cutoff", and a
	// row sitting precisely on a 24h boundary is the older reading.
	edge := []*CompletionAnomaly{{ObservedAt: at(-CompletionAnomalyWindow)}}
	if got := countRecent(edge, now, CompletionAnomalyWindow); got != 0 {
		t.Fatalf("anomaly exactly at the cutoff: got %d, want 0", got)
	}
	if got := countRecent([]*CompletionAnomaly{{ObservedAt: at(-CompletionAnomalyWindow + time.Second)}}, now, CompletionAnomalyWindow); got != 1 {
		t.Fatalf("anomaly one second inside the cutoff: got %d, want 1", got)
	}

	// A NULL timestamp counts as recent. Dropping it would render an unknown
	// age as health, which is the failure this panel exists to catch — so the
	// unsafe direction is the one that is asserted.
	if got := countRecent([]*CompletionAnomaly{{OrderID: 1}}, now, CompletionAnomalyWindow); got != 1 {
		t.Fatalf("anomaly with no timestamp: got %d, want 1 (unknown must not read as healthy)", got)
	}
}
