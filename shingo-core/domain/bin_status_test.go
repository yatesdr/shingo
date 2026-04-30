package domain

import "testing"

func TestBinStatus_IsTerminal(t *testing.T) {
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
	var s BinStatus
	if err := s.Scan(nil); err != nil {
		t.Fatalf("Scan(nil) error: %v", err)
	}
	if s != "" {
		t.Errorf("Scan(nil) -> %q, want empty", s)
	}
	if err := s.Scan([]byte("staged")); err != nil {
		t.Fatalf("Scan([]byte) error: %v", err)
	}
	if s != BinStatusStaged {
		t.Errorf("Scan([]byte staged) -> %q, want %q", s, BinStatusStaged)
	}
}

func TestBinStatus_TerminalDerivedFromTable(t *testing.T) {
	for status := range validBinTransitions {
		if status.IsTerminal() {
			t.Errorf("status %q has outgoing edges in validBinTransitions but IsTerminal returned true", status)
		}
	}
}
