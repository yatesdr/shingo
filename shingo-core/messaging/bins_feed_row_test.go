package messaging

import (
	"testing"
	"time"

	"shingocore/store/bins"
)

// TestBinRow_CapacityAndHeldPartial: a row carries its part's capacity, and a
// held partial (held_since set) its arrival and its count, which does not
// tick while it waits; a carrier with no part carries no capacity, and one that
// is not held carries neither. The line a held partial waits for is
// markHeldLines' read, not the row's.
func TestBinRow_CapacityAndHeldPartial(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	held := binRow("HOME-1", &bins.Bin{ID: 7, PayloadCode: "PART-H", UOPRemaining: 40, UOPCapacity: 100, HeldSince: &at})
	if held.UOPCapacity != 100 || held.HeldSince != "2026-10-18T10:00:00Z" || held.HeldUOP != 40 || held.HeldFor != "" {
		t.Errorf("held row = capacity %d, since %q, count %d, line %q; want 100, 2026-10-18T10:00:00Z, 40, none",
			held.UOPCapacity, held.HeldSince, held.HeldUOP, held.HeldFor)
	}
	plain := binRow("LINE-1", &bins.Bin{ID: 8, PayloadCode: "PART-H", UOPRemaining: 40, UOPCapacity: 100})
	if plain.UOPCapacity != 100 || plain.HeldSince != "" || plain.HeldUOP != 0 {
		t.Errorf("unheld row = capacity %d, since %q, count %d; want 100, none, 0", plain.UOPCapacity, plain.HeldSince, plain.HeldUOP)
	}
	empty := binRow("LINE-2", &bins.Bin{ID: 9, UOPCapacity: 100})
	if empty.UOPCapacity != 0 {
		t.Errorf("a carrier with no part carries capacity %d; want 0", empty.UOPCapacity)
	}
}
