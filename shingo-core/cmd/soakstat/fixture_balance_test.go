package main

import (
	"strings"
	"testing"
)

// TestFixtureBalance_BothDirectionsAreFlaggedOnTheSameMargin: a payload made
// 20% faster than drawn and one drawn 20% faster than made are each flagged and
// named for their direction; one within 10% either way, one short by less than
// a carrier, and one never made (a loader's) are not.
func TestFixtureBalance_BothDirectionsAreFlaggedOnTheSameMargin(t *testing.T) {
	t.Parallel()
	bal := []payloadBalance{
		{payload: "OVER", made: 1200, drawn: 1000, capacity: 100},
		{payload: "UNDER", made: 1000, drawn: 1200, capacity: 100},
		{payload: "EVEN", made: 1000, drawn: 1080, capacity: 10},
		{payload: "ONEBIN", made: 100, drawn: 150, capacity: 60},
		{payload: "LOADER", made: 0, drawn: 900, capacity: 50},
	}
	for _, b := range bal {
		want := b.payload == "OVER" || b.payload == "UNDER"
		if got := b.over() || b.under(); got != want {
			t.Errorf("%s flagged=%v, want %v (made %d, drawn %d)", b.payload, got, want, b.made, b.drawn)
		}
	}
	if !bal[1].under() || bal[1].over() {
		t.Error("UNDER was not called under-made")
	}
	got := balanceFlags(bal)
	if !strings.Contains(got, "over-makes OVER") || !strings.Contains(got, "under-makes UNDER") {
		t.Errorf("summary %q does not name each direction", got)
	}
	if !strings.Contains(bal[1].flag(), "(-17%)") {
		t.Errorf("under-make flag %q does not read as a shortfall", bal[1].flag())
	}
}
