package helpers

import (
	"database/sql"
	"testing"
	"time"
)

// A GENERIC SCAN HELPER READING A COLUMN ITS WRITER DID NOT WRITE FOR IT.
//
// Most of this schema stores TimeLayout, but several columns are deliberately
// RFC3339Nano — demand_origins_open.opened_at, supply_refusals, and
// style_node_claims.below_reorder_since. The first two grew a local time.Parse
// next to their own writer. The third is read by scanNodeClaim, a generic
// helper, and the mismatch had no symptom at the seam: the parse failed, the
// zero time came back, and every claim loaded from the database reported its
// falling edge as UNSET while the column plainly held a timestamp.
//
// What that cost: evaluateCellLevel stamps the falling edge only when it reads
// nil, so it re-stamped on every tick — an episode that had been open for hours
// always looked as if it had just started. And the rising edge only clears a
// flag it can see, so it never cleared one, which left CellLevelStillBreached
// answering "still breached" forever and cell episodes unable to close by
// either route.
//
// The parse is the whole fix, so the parse is what this pins.
func TestScanTimeAcceptsBothLayouts(t *testing.T) {
	want := time.Date(2026, 9, 1, 13, 44, 21, 0, time.UTC)

	if got := ScanTime("2026-09-01 13:44:21"); !got.Equal(want) {
		t.Errorf("canonical layout: got %v, want %v", got, want)
	}
	if got := ScanTime("2026-09-01T13:44:21Z"); !got.Equal(want) {
		t.Errorf("RFC3339: got %v, want %v — this is the layout below_reorder_since is written in", got, want)
	}

	// Sub-second precision survives; the falling edge is written with it.
	nano := time.Date(2026, 9, 1, 13, 44, 21, 793864200, time.UTC)
	if got := ScanTime(nano.Format(time.RFC3339Nano)); !got.Equal(nano) {
		t.Errorf("RFC3339Nano: got %v, want %v", got, nano)
	}

	// Unparseable still yields the zero time rather than an error — the
	// documented contract every existing caller is written against.
	if got := ScanTime("not a time"); !got.IsZero() {
		t.Errorf("garbage: got %v, want the zero time", got)
	}
}

func TestScanTimePtrAcceptsBothLayouts(t *testing.T) {
	want := time.Date(2026, 9, 1, 13, 44, 21, 0, time.UTC)

	for _, s := range []string{"2026-09-01 13:44:21", "2026-09-01T13:44:21Z"} {
		got := ScanTimePtr(sql.NullString{String: s, Valid: true})
		if got == nil || !got.Equal(want) {
			t.Errorf("ScanTimePtr(%q) = %v, want %v", s, got, want)
		}
	}
	if got := ScanTimePtr(sql.NullString{}); got != nil {
		t.Errorf("NULL: got %v, want nil", got)
	}
	if got := ScanTimePtr(sql.NullString{String: "not a time", Valid: true}); got != nil {
		t.Errorf("garbage: got %v, want nil", got)
	}
}
