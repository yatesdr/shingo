package engine

import (
	"strings"
	"testing"

	"shingocore/domain"
	"shingocore/fleet"
)

// TestFleetStopReason pins the sentence a fleet stop writes onto the order row,
// its history, the Edge's OrderCancelled and the swap partner's reason.
//
// The first case is HK core order 3166 (2026-11-04) as /orderDetails returned
// it: one error, the code and text that explained a morning of cancelled swaps
// on the 400t press, and which the old literal "fleet order stopped" dropped.
func TestFleetStopReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		snap *fleet.OrderSnapshot
		want string
	}{
		{
			name: "HK 3166: no path to the next block",
			snap: &fleet.OrderSnapshot{State: "STOPPED", Errors: []fleet.OrderMessage{
				{Code: 60009, Desc: "Cannot find path to new block target [PLN_05]", Times: 1, Timestamp: 1793802602},
			}},
			want: "fleet order stopped — RDS error 60009: Cannot find path to new block target [PLN_05]",
		},
		{
			name: "no snapshot: the bare detail",
			snap: nil,
			want: "fleet order stopped",
		},
		{
			// A hand terminate in RoboShop carries no errors[]. The station
			// toasts this text, so an absent reason stays absent (owner, 2026-11-04).
			name: "snapshot with no errors: the bare detail, no suffix",
			snap: &fleet.OrderSnapshot{State: "STOPPED"},
			want: "fleet order stopped",
		},
		{
			name: "empty entries are not a reason",
			snap: &fleet.OrderSnapshot{Errors: []fleet.OrderMessage{{Desc: "  "}}},
			want: "fleet order stopped",
		},
		{
			name: "the newest error only, the rest counted; exact repeats once",
			snap: &fleet.OrderSnapshot{Errors: []fleet.OrderMessage{
				{Code: 60011, Desc: "cannot replan", Timestamp: 100},
				{Code: 60009, Desc: "Cannot find path to new block target [PLN_02]", Timestamp: 200},
				{Code: 60011, Desc: "cannot replan", Timestamp: 100},
			}},
			want: "fleet order stopped — RDS error 60009: Cannot find path to new block target [PLN_02] (+1 more)",
		},
		{
			name: "a code with no text still names the thing to look up",
			snap: &fleet.OrderSnapshot{Errors: []fleet.OrderMessage{{Code: 60009}}},
			want: "fleet order stopped — RDS error 60009",
		},
		{
			name: "text with no code",
			snap: &fleet.OrderSnapshot{Errors: []fleet.OrderMessage{{Desc: "Task Failed"}}},
			want: "fleet order stopped — RDS: Task Failed",
		},
		{
			name: "many distinct errors: one shown, the rest counted",
			snap: &fleet.OrderSnapshot{Errors: []fleet.OrderMessage{
				{Code: 1, Desc: "a", Timestamp: 5},
				{Code: 2, Desc: "b", Timestamp: 4},
				{Code: 3, Desc: "c", Timestamp: 3},
				{Code: 4, Desc: "d", Timestamp: 2},
				{Code: 5, Desc: "e", Timestamp: 1},
			}},
			want: "fleet order stopped — RDS error 1: a (+4 more)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := fleetStopReason(c.snap); got != c.want {
				t.Errorf("fleetStopReason:\n got %q\nwant %q", got, c.want)
			}
		})
	}
}

// TestFleetStopReason_StaysRDSOrigin pins the property the prefix exists for:
// every sentence this writes still classifies as an RDS-origin CANCEL, so
// neither the Cancelled tile's split nor the failure rate moves when the
// fleet's words are added (Q-030 keeps fleet stops as cancels).
func TestFleetStopReason_StaysRDSOrigin(t *testing.T) {
	t.Parallel()
	snaps := []*fleet.OrderSnapshot{
		nil,
		{},
		{Errors: []fleet.OrderMessage{{Code: 60009, Desc: "Cannot find path to new block target [PLN_05]"}}},
		// Fleet text that happens to read like a person did it must not turn
		// an RDS stop into a shingo one.
		{Errors: []fleet.OrderMessage{{Code: 7, Desc: "cancelled by dispatcher"}}},
		// Nor may fleet text with a failure word turn the cancel into a failure.
		{Errors: []fleet.OrderMessage{{Code: 8, Desc: "wait timeout, task abandoned"}}},
	}
	for _, s := range snaps {
		reason := fleetStopReason(s)
		if got := domain.ClassifyCancelOrigin("", reason); got != domain.CancelOriginRDS {
			t.Errorf("ClassifyCancelOrigin(%q) = %q, want %q", reason, got, domain.CancelOriginRDS)
		}
		if got := domain.ClassifyTermination("STOPPED", reason); got != domain.OutcomeCancelled {
			t.Errorf("ClassifyTermination(STOPPED, %q) = %q, want %q", reason, got, domain.OutcomeCancelled)
		}
	}
}

// TestFleetStopReason_BoundsFleetText pins that a vendor description cannot grow
// the reason without limit.
func TestFleetStopReason_BoundsFleetText(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 1000)
	got := fleetStopReason(&fleet.OrderSnapshot{Errors: []fleet.OrderMessage{{Code: 1, Desc: long}}})
	if n := len([]rune(got)); n > len(fleetStopDetail)+3+len("RDS error 1: ")+fleet.VendorTextMax+1 {
		t.Errorf("reason is %d runes; the fleet's text was not bounded: %q", n, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated text should end with an ellipsis: %q", got)
	}
}
