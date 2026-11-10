package messaging

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"shingo/protocol"
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

// TestBinRow_CarriesItsCountForDisplay: an occupied row whose carrier has a
// part carries its count (DisplayUOP), 0 and below zero included, for a
// screen to show; an empty carrier and an empty node carry none. The count is
// outside the digest (protocol TestNodeBinsDigest_Golden, "a count that
// moves"), so a tick pushes nothing.
func TestBinRow_CarriesItsCountForDisplay(t *testing.T) {
	t.Parallel()
	count := func(r protocol.NodeBinsRow) string {
		if r.DisplayUOP == nil {
			return "none"
		}
		return fmt.Sprint(*r.DisplayUOP)
	}
	at := time.Date(2026, 11, 10, 7, 0, 0, 0, time.UTC)
	got := []string{
		count(binRow("WIN-1", &bins.Bin{ID: 1, PayloadCode: "PART-A", UOPRemaining: 1027, UOPCapacity: 1300})),
		count(binRow("WIN-2", &bins.Bin{ID: 2, PayloadCode: "PART-A", UOPRemaining: 0, UOPCapacity: 1300})),
		count(binRow("WIN-3", &bins.Bin{ID: 3, PayloadCode: "PART-A", UOPRemaining: -2, UOPCapacity: 1300})),
		count(binRow("HOME-1", &bins.Bin{ID: 4, PayloadCode: "PART-A", UOPRemaining: 40, UOPCapacity: 100, HeldSince: &at})),
		count(binRow("WIN-4", &bins.Bin{ID: 5, UOPRemaining: 0})),
		count(binRow("WIN-5", nil)),
	}
	want := []string{"1027", "0", "-2", "40", "none", "none"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("display counts = %v, want %v", got, want)
	}
}

// TestBinRow_StateEpochAndPartial: a bin's row is occupied and carries its
// generation; with no bin it is empty. Partial is a count above zero and below
// the part's capacity, so it moves at the two crossings only, and a carrier
// with no part (no capacity) is never partial.
func TestBinRow_StateEpochAndPartial(t *testing.T) {
	t.Parallel()
	if r := binRow("LINE-0", nil); r.State != protocol.BinsStateEmpty || r.Occupied || r.Epoch != 0 {
		t.Errorf("no bin: %+v, want an empty row", r)
	}
	for _, c := range []struct {
		name             string
		part             string
		count, cap       int
		partial, stocked bool
	}{
		{"full", "PART-H", 100, 100, false, true},
		{"one drawn", "PART-H", 99, 100, true, true},
		{"one left", "PART-H", 1, 100, true, true},
		{"emptied", "PART-H", 0, 100, false, false},
		{"overpacked below zero", "PART-H", -2, 100, false, false},
		{"over capacity", "PART-H", 101, 100, false, true},
		{"no part", "", 40, 100, false, true},
	} {
		r := binRow("LINE-1", &bins.Bin{ID: 8, PayloadCode: c.part, UOPRemaining: c.count, UOPCapacity: c.cap, DeltaEpoch: 4})
		if r.State != protocol.BinsStateOccupied || !r.Occupied || r.Epoch != 4 || r.Partial != c.partial || r.Stocked != c.stocked {
			t.Errorf("%s: state %q occupied %v epoch %d partial %v stocked %v; want occupied, 4, partial %v, stocked %v",
				c.name, r.State, r.Occupied, r.Epoch, r.Partial, r.Stocked, c.partial, c.stocked)
		}
	}
}
