package binsource

import (
	"testing"

	"shingo/protocol"
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
		domain.BinStatusMaintenance, domain.BinStatusRetired,
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
									BinID: 1, Payload: p, UOP: u, Cap: cap10, Stocked: u > 0,
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
// a payload that is nearly right (`SYN-PART02G.06` vs `.10`) and a full bin that
// was never confirmed — both of which look like a present, healthy bin on every
// surface except this one.
func TestRejectReasonNamesTheFirstFailure(t *testing.T) {
	full := func(mut func(*Cand)) Cand {
		c := Cand{BinID: 7, Payload: X, UOP: cap10, Cap: cap10, Stocked: true, CreatedAt: tOld,
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
		{"blocking status", full(func(c *Cand) { c.Status = domain.BinStatusMaintenance }), "status:maintenance"},
		{"empty bin cannot be drained", full(func(c *Cand) { c.Payload = ""; c.UOP, c.Stocked = 0, false }), "empty-bin"},
		{"near-miss payload is named", full(func(c *Cand) { c.Payload = Y }), "payload:" + Y},
		{"drained to zero", full(func(c *Cand) { c.UOP, c.Stocked = 0, false }), "uop<=0"},
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
	partial.Payload, partial.UOP, partial.Stocked = X, cap10-1, true
	if got := RejectReason(partial, want); got != "" {
		t.Errorf("partial of X should be eligible for Fill, got %q", got)
	}

	fullOfX := base
	fullOfX.Payload, fullOfX.UOP, fullOfX.Stocked, fullOfX.ManifestConfirmed = X, cap10, true, true
	if got := RejectReason(fullOfX, want); got != "full-not-a-container" {
		t.Errorf("full of X for Fill = %q, want full-not-a-container", got)
	}

	otherPart := base
	otherPart.Payload, otherPart.UOP, otherPart.Stocked = Y, 1, true
	if got := RejectReason(otherPart, want); got != "payload:"+Y {
		t.Errorf("partial of Y for Fill = %q, want payload:%s", got, Y)
	}
}

// TestRejectReasonFillHeight: the pool never draws tops & bottoms for an
// ordinary Fill, and a want of tops & bottoms alone draws nothing else — not
// an empty, not a stack, not a partial of the part.
func TestRejectReasonFillHeight(t *testing.T) {
	base := Cand{BinID: 9, Cap: cap10, CreatedAt: tOld, Status: domain.BinStatusAvailable}
	topsAndBottoms := base
	topsAndBottoms.State = protocol.CarrierTopsAndBottoms
	empty := base
	empty.State = protocol.CarrierEmpty
	partial := base
	partial.Payload, partial.UOP, partial.Stocked, partial.State = X, cap10-1, true, protocol.CarrierLoaded
	for _, c := range []struct {
		name   string
		cand   Cand
		height domain.Height
		want   string
	}{
		{"tops & bottoms, no height said", topsAndBottoms, domain.Height{}, "height"},
		{"tops & bottoms, a stack", topsAndBottoms, domain.Layers(3), "height"},
		{"tops & bottoms, a want of 0", topsAndBottoms, domain.Layers(0), ""},
		{"tops & bottoms, tops & bottoms only", topsAndBottoms, domain.TopsAndBottomsOnly, ""},
		{"an empty, tops & bottoms only", empty, domain.TopsAndBottomsOnly, "height"},
		{"a partial of the part, tops & bottoms only", partial, domain.TopsAndBottomsOnly, "height"},
		{"an empty, no height said", empty, domain.Height{}, ""},
		{"a partial of the part, no height said", partial, domain.Height{}, ""},
	} {
		if got := RejectReason(c.cand, Want{Payload: X, Intent: Fill, Height: c.height}); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// Drain never takes a part-less carrier, whatever the height.
	if got := RejectReason(topsAndBottoms, Want{Payload: X, Intent: Drain, Height: domain.TopsAndBottomsOnly}); got != "empty-bin" {
		t.Errorf("Drain over tops & bottoms = %q, want empty-bin", got)
	}

	pool := []Cand{topsAndBottoms, empty}
	pool[0].BinID, pool[1].BinID = 1, 2
	if got, ok := Source(pool, Want{Payload: X, Intent: Fill}); !ok || got.BinID != 2 {
		t.Errorf("an ordinary Fill drew %+v (%v), want the empty", got, ok)
	}
	if got, ok := Source(pool, Want{Payload: X, Intent: Fill, Height: domain.TopsAndBottomsOnly}); !ok || got.BinID != 1 {
		t.Errorf("tops & bottoms only drew %+v (%v), want tops & bottoms", got, ok)
	}
	if _, ok := Source(pool[:1], Want{Payload: X, Intent: Fill}); ok {
		t.Error("an ordinary Fill drew from a pool holding only tops & bottoms")
	}
}
