package protocol

import (
	"testing"
)

func TestStatusIsTerminal(t *testing.T) {
	terminals := []Status{StatusConfirmed, StatusCancelled, StatusFailed}
	for _, s := range terminals {
		if !s.IsTerminal() {
			t.Errorf("%s.IsTerminal() = false, want true", s)
		}
	}
	nonTerminals := []Status{StatusPending, StatusDelivered, StatusInTransit, StatusStaged, StatusQueued}
	for _, s := range nonTerminals {
		if s.IsTerminal() {
			t.Errorf("%s.IsTerminal() = true, want false", s)
		}
	}
}

func TestStatusFaultedIsNonTerminal(t *testing.T) {
	if StatusFaulted.IsTerminal() {
		t.Error("StatusFaulted.IsTerminal() = true, want false")
	}
}

func TestFaultedTransitions(t *testing.T) {
	accepted := []struct{ from, to Status }{
		{StatusDispatched, StatusFaulted},
		{StatusAcknowledged, StatusFaulted},
		{StatusInTransit, StatusFaulted},
		{StatusStaged, StatusFaulted},
		{StatusFaulted, StatusInTransit},
		{StatusFaulted, StatusDelivered},
		{StatusFaulted, StatusFailed},
		{StatusFaulted, StatusCancelled},
	}
	for _, c := range accepted {
		if !c.from.CanTransitionTo(c.to) {
			t.Errorf("%s.CanTransitionTo(%s) = false, want true", c.from, c.to)
		}
	}

	rejected := []struct{ from, to Status }{
		{StatusQueued, StatusFaulted},
		{StatusPending, StatusFaulted},
		{StatusDelivered, StatusFaulted},
		{StatusConfirmed, StatusFaulted},
		{StatusFaulted, StatusQueued},
		{StatusFaulted, StatusDispatched},
		{StatusFaulted, StatusPending},
		{StatusFaulted, StatusConfirmed},
	}
	for _, c := range rejected {
		if c.from.CanTransitionTo(c.to) {
			t.Errorf("%s.CanTransitionTo(%s) = true, want false", c.from, c.to)
		}
	}
}
func TestStatusCanTransitionTo(t *testing.T) {
	cases := []struct {
		from, to Status
		want     bool
	}{
		{StatusPending, StatusSourcing, true},
		{StatusSourcing, StatusDispatched, true},
		{StatusDelivered, StatusConfirmed, true},
		{StatusConfirmed, StatusPending, false}, // terminal
		{StatusPending, StatusConfirmed, false}, // skip
	}
	for _, c := range cases {
		got := c.from.CanTransitionTo(c.to)
		if got != c.want {
			t.Errorf("%s.CanTransitionTo(%s) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestStatusString(t *testing.T) {
	if got := StatusPending.String(); got != "pending" {
		t.Errorf("StatusPending.String() = %q, want %q", got, "pending")
	}
}

func TestStatusScanValue(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want Status
	}{
		{"string", "pending", StatusPending},
		{"bytes", []byte("delivered"), StatusDelivered},
		{"nil", nil, Status("")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var s Status
			if err := s.Scan(c.in); err != nil {
				t.Fatalf("Scan(%v): %v", c.in, err)
			}
			if s != c.want {
				t.Errorf("Scan(%v) = %q, want %q", c.in, s, c.want)
			}
		})
	}

	// Round-trip through Value
	for _, want := range AllStatuses() {
		v, err := want.Value()
		if err != nil {
			t.Fatalf("%s.Value(): %v", want, err)
		}
		var got Status
		if err := got.Scan(v); err != nil {
			t.Fatalf("Scan(%v): %v", v, err)
		}
		if got != want {
			t.Errorf("round-trip %s: got %s", want, got)
		}
	}
}

func TestStatusScanRejectsUnsupportedType(t *testing.T) {
	var s Status
	if err := s.Scan(42); err == nil {
		t.Error("Scan(int) returned nil error, want failure")
	}
}
