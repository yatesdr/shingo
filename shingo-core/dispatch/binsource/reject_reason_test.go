package binsource

import (
	"testing"

	"shingocore/domain"
)

// TestRejectReasonCannotDriftFromEligible is the whole point of RejectReason
// existing. The diagnostic that says WHY a bin was refused and the selector that
// refused it are one implementation, so a log line can never claim a reason the
// selector did not act on — the drift this package's status-predicate comment
// was written to end, restated for the explanation half.
//
// If someone later "optimises" eligible into its own switch, this fails.
func TestRejectReasonCannotDriftFromEligible(t *testing.T) {
	statuses := []domain.BinStatus{
		domain.BinStatusAvailable, domain.BinStatusStaged, domain.BinStatusFlagged,
		domain.BinStatusMaintenance, domain.BinStatusQualityHold, domain.BinStatusRetired,
	}
	payloads := []string{"", X, Y}
	uops := []int{-1, 0, 1, cap10 - 1, cap10, cap10 + 1}
	bools := []bool{false, true}

	for _, intent := range []Intent{Drain, Fill} {
		want := Want{Payload: X, Intent: intent}
		for _, st := range statuses {
			for _, p := range payloads {
				for _, u := range uops {
					for _, claimed := range bools {
						for _, locked := range bools {
							for _, confirmed := range bools {
								c := Cand{
									BinID: 1, Payload: p, UOP: u, Cap: cap10,
									CreatedAt: tOld, Claimed: claimed, Locked: locked,
									ManifestConfirmed: confirmed, Status: st,
								}
								gotEligible := eligible(c, want)
								reason := RejectReason(c, want)
								if gotEligible != (reason == "") {
									t.Fatalf("drift: eligible=%v but RejectReason=%q\n  intent=%v cand=%+v",
										gotEligible, reason, intent, c)
								}
							}
						}
					}
				}
			}
		}
	}
}

// TestRejectReasonNamesTheFirstFailure pins the tags a diagnostic reader will
// actually see, including the two that mattered at Springfield 2026-08-05:
// a payload that is nearly right (`63144-6TA1A.06` vs `.10`) and a full bin that
// was never confirmed — both of which look like a present, healthy bin on every
// surface except this one.
func TestRejectReasonNamesTheFirstFailure(t *testing.T) {
	full := func(mut func(*Cand)) Cand {
		c := Cand{BinID: 7, Payload: X, UOP: cap10, Cap: cap10, CreatedAt: tOld,
			ManifestConfirmed: true, Status: domain.BinStatusAvailable}
		if mut != nil {
			mut(&c)
		}
		return c
	}

	cases := []struct {
		name string
		cand Cand
		want string
	}{
		{"eligible full of X", full(nil), ""},
		{"claimed beats everything", full(func(c *Cand) { c.Claimed = true; c.Payload = Y }), "claimed"},
		{"locked", full(func(c *Cand) { c.Locked = true }), "locked"},
		{"blocking status", full(func(c *Cand) { c.Status = domain.BinStatusQualityHold }), "status:quality_hold"},
		{"empty bin cannot be drained", full(func(c *Cand) { c.Payload = ""; c.UOP = 0 }), "empty-bin"},
		{"near-miss payload is named", full(func(c *Cand) { c.Payload = Y }), "payload:" + Y},
		{"drained to zero", full(func(c *Cand) { c.UOP = 0 }), "uop<=0"},
		{"full but unconfirmed", full(func(c *Cand) { c.ManifestConfirmed = false }), "unconfirmed-full"},
		{"partial need not be confirmed", full(func(c *Cand) { c.UOP = cap10 - 1; c.ManifestConfirmed = false }), ""},
		{"over-capacity counts as full", full(func(c *Cand) { c.UOP = cap10 + 1; c.ManifestConfirmed = false }), "unconfirmed-full"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RejectReason(tc.cand, Want{Payload: X, Intent: Drain}); got != tc.want {
				t.Errorf("RejectReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRejectReasonFillTags covers the container side: Fill wants a partial of X
// or a fungible empty, so a FULL bin of X is refused — correctly, and with a tag
// that says so rather than the misleading "payload" mismatch.
func TestRejectReasonFillTags(t *testing.T) {
	base := Cand{BinID: 9, Cap: cap10, CreatedAt: tOld, Status: domain.BinStatusAvailable}
	want := Want{Payload: X, Intent: Fill}

	empty := base
	if got := RejectReason(empty, want); got != "" {
		t.Errorf("empty carrier should be eligible for Fill, got %q", got)
	}

	partial := base
	partial.Payload, partial.UOP = X, cap10-1
	if got := RejectReason(partial, want); got != "" {
		t.Errorf("partial of X should be eligible for Fill, got %q", got)
	}

	fullOfX := base
	fullOfX.Payload, fullOfX.UOP, fullOfX.ManifestConfirmed = X, cap10, true
	if got := RejectReason(fullOfX, want); got != "full-not-a-container" {
		t.Errorf("full of X for Fill = %q, want full-not-a-container", got)
	}

	otherPart := base
	otherPart.Payload, otherPart.UOP = Y, 1
	if got := RejectReason(otherPart, want); got != "payload:"+Y {
		t.Errorf("partial of Y for Fill = %q, want payload:%s", got, Y)
	}
}
