package dispatch

import (
	"testing"

	"shingo/protocol"
)

// homeAcceptsCarrier is the whole of the payload gate's decision, and every
// input except one is a reason to ALLOW. Table-driven rather than a handful of
// named tests because the failure mode that matters is a future edit flipping
// one of the fail-open arms closed: an over-eager guard here sends every
// ordinary swap return to buffer and exhausts the pool within a shift, which is
// a worse outage than the one the guard exists to prevent.
func TestHomeAcceptsCarrier(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		homePin string
		carrier string
		known   bool
		want    bool
		because string
	}{
		{
			name: "match", homePin: "PART-X", carrier: "PART-X", known: true, want: true,
			because: "the ordinary case: a carrier going back to its own home",
		},
		{
			name: "mismatch", homePin: "PART-X", carrier: "PART-Y", known: true, want: false,
			because: "THE ONE REFUSAL — SPR 2026-09-03, a 74871 carrier onto 63145's home",
		},
		{
			name: "unpinned home takes anything", homePin: "", carrier: "PART-Y", known: true, want: true,
			because: "a buffer or unassigned position has no opinion and never had one",
		},
		{
			name: "spent carrier holds its home", homePin: "PART-X", carrier: "", known: true, want: true,
			because: "payload is CLEARED on release; this is the most common return in the plant",
		},
		{
			name: "unreadable carrier fails open", homePin: "PART-X", carrier: "", known: false, want: true,
			because: "diverting on an unread fact trades a rare wrong park for a common one",
		},
		{
			name: "unreadable with a stale value fails open", homePin: "PART-X", carrier: "PART-Y", known: false, want: true,
			because: "known=false disqualifies the value entirely; it must not be compared",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := homeAcceptsCarrier(tc.homePin, tc.carrier, tc.known); got != tc.want {
				t.Errorf("homeAcceptsCarrier(%q, %q, %v) = %v, want %v — %s",
					tc.homePin, tc.carrier, tc.known, got, tc.want, tc.because)
			}
		})
	}
}

// firstPickupNode names the step that lifts the carrier this leg carries. A
// return leg opens with a station WAIT at the line and only then picks up, so
// "first step" and "first pickup" are different steps and the distinction is the
// point — reading the wait's node would name the line whether or not a pickup
// happens there.
func TestFirstPickupNode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		steps []resolvedStep
		want  string
	}{
		{
			name: "return leg skips the leading wait",
			steps: []resolvedStep{
				{Action: protocol.ActionWait, Node: "LINE"},
				{Action: protocol.ActionPickup, Node: "LINE"},
				{Action: protocol.ActionDropoff, Node: "HOME"},
			},
			want: "LINE",
		},
		{
			name: "supply leg picks at the home",
			steps: []resolvedStep{
				{Action: protocol.ActionPickup, Node: "HOME"},
				{Action: protocol.ActionDropoff, Node: "LINE"},
			},
			want: "HOME",
		},
		{
			name: "first of several pickups wins",
			steps: []resolvedStep{
				{Action: protocol.ActionPickup, Node: "HOME"},
				{Action: protocol.ActionDropoff, Node: "STAGE"},
				{Action: protocol.ActionPickup, Node: "STAGE"},
			},
			want: "HOME",
		},
		{
			name:  "no pickup at all",
			steps: []resolvedStep{{Action: protocol.ActionDropoff, Node: "HOME"}},
			want:  "",
		},
		{
			name: "a node-less pickup is not an answer",
			steps: []resolvedStep{
				{Action: protocol.ActionPickup},
				{Action: protocol.ActionPickup, Node: "HOME"},
			},
			want: "HOME",
		},
		{name: "empty plan", steps: nil, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := firstPickupNode(tc.steps); got != tc.want {
				t.Errorf("firstPickupNode() = %q, want %q", got, tc.want)
			}
		})
	}
}
