package domain

import (
	"shingo/protocol/testutil"
	"testing"
)

func TestBinStatus_IsTerminal(t *testing.T) {
	t.Parallel()
	terminal := []BinStatus{BinStatusRetired}
	for _, s := range terminal {
		if !s.IsTerminal() {
			t.Errorf("%s.IsTerminal() = false, want true", s)
		}
	}
	nonTerminal := []BinStatus{BinStatusAvailable, BinStatusStaged, BinStatusFlagged, BinStatusMaintenance, BinStatusQualityHold}
	for _, s := range nonTerminal {
		if s.IsTerminal() {
			t.Errorf("%s.IsTerminal() = true, want false", s)
		}
	}
}

func TestBinStatus_CanTransitionTo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		from, to BinStatus
		want     bool
	}{
		{BinStatusAvailable, BinStatusStaged, true},
		{BinStatusAvailable, BinStatusRetired, true},
		{BinStatusStaged, BinStatusAvailable, true},
		{BinStatusStaged, BinStatusFlagged, false}, // staged must release before re-classification
		{BinStatusFlagged, BinStatusAvailable, true},
		{BinStatusMaintenance, BinStatusRetired, true},
		{BinStatusRetired, BinStatusAvailable, false}, // terminal
	}
	for _, c := range cases {
		if got := c.from.CanTransitionTo(c.to); got != c.want {
			t.Errorf("%s.CanTransitionTo(%s) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestBinStatus_ScanValue_Roundtrip(t *testing.T) {
	t.Parallel()
	for _, original := range AllBinStatuses() {
		v, err := original.Value()
		if err != nil {
			t.Fatalf("%s.Value() error: %v", original, err)
		}
		var got BinStatus
		if err := got.Scan(v); err != nil {
			t.Fatalf("%s: Scan error: %v", original, err)
		}
		if got != original {
			t.Errorf("roundtrip: got %q, want %q", got, original)
		}
	}
}

func TestBinStatus_Scan_NullEmpty(t *testing.T) {
	t.Parallel()
	var s BinStatus
	testutil.MustNoErr(t, s.Scan(nil), "Scan(nil) error")
	if s != "" {
		t.Errorf("Scan(nil) -> %q, want empty", s)
	}
	testutil.MustNoErr(t, s.Scan([]byte("staged")), "Scan([]byte) error")
	if s != BinStatusStaged {
		t.Errorf("Scan([]byte staged) -> %q, want %q", s, BinStatusStaged)
	}
}

func TestBinStatus_TerminalDerivedFromTable(t *testing.T) {
	t.Parallel()
	for status := range validBinTransitions {
		if status.IsTerminal() {
			t.Errorf("status %q has outgoing edges in validBinTransitions but IsTerminal returned true", status)
		}
	}
}
