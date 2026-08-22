package protocol

import (
	"strings"
	"testing"
	"time"
)

// The sentences this file pins are read on the floor. Changing one is a change
// to what an operator is told about a live order, not a copy edit — same
// standing as TestFormatQueueSentence_Snapshot in dispatch.

var faultBase = time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)

func TestFormatFaultSentence(t *testing.T) {
	vendor := TermRef{Node: "ALN_003", VendorCode: 60011, VendorDesc: "cannot replan"}
	bare := TermRef{Node: "ALN_003"}

	tests := []struct {
		name   string
		phase  FaultPhase
		ref    TermRef
		since  time.Time
		now    time.Time
		notice bool
		want   string
	}{
		{
			// The 97% case. Under the threshold the order is replanning, and
			// the fleet's reason is withheld with the word — "cannot replan"
			// beside "Replanning" reads as a contradiction at 14 seconds.
			name:  "live under threshold is a replan, vendor reason withheld",
			phase: FaultPhaseLive, ref: vendor,
			since: faultBase, now: faultBase.Add(14 * time.Second), notice: false,
			want: "Replanning",
		},
		{
			name:  "live over threshold with no fleet reason",
			phase: FaultPhaseLive, ref: bare,
			since: faultBase, now: faultBase.Add(3*time.Minute + 12*time.Second), notice: true,
			want: "Fault",
		},
		{
			name:  "live over threshold quotes the fleet, code in parentheses",
			phase: FaultPhaseLive, ref: vendor,
			since: faultBase, now: faultBase.Add(3*time.Minute + 12*time.Second), notice: true,
			want: "Fault · cannot replan (60011)",
		},
		{
			name:  "recovery row carries the dwell it recovered after",
			phase: FaultPhaseRecovered, ref: vendor,
			since: faultBase, now: faultBase.Add(18 * time.Second),
			want: "Recovered after 18 s",
		},
		{
			name:  "gave-up row carries the dwell and the reason it faulted for",
			phase: FaultPhaseGaveUp, ref: vendor,
			since: faultBase, now: faultBase.Add(45 * time.Minute),
			want: "Gave up after 45m · cannot replan (60011)",
		},

		// ── The edges ────────────────────────────────────────────────────
		{
			// MarkFaulted renders at the instant of faulting: since == now.
			name:  "at the instant of faulting the elapsed is zero, not negative",
			phase: FaultPhaseLive, ref: vendor,
			since: faultBase, now: faultBase, notice: false,
			want: "Replanning",
		},
		{
			name:  "an empty ref over threshold still says what it knows",
			phase: FaultPhaseLive, ref: TermRef{},
			since: faultBase, now: faultBase.Add(2 * time.Minute), notice: true,
			want: "Fault",
		},
		{
			// A zero `since` means the faulted row could not be read. The
			// sentence degrades to the phase rather than printing a duration
			// measured from the zero time, which would read as 2026 years.
			name:  "a zero since renders zero, not the age of the epoch",
			phase: FaultPhaseRecovered, ref: bare,
			since: time.Time{}, now: faultBase,
			want: "Recovered after 0 s",
		},
		{
			// Clock skew between Core and the DB must not print "-3 s".
			name:  "a now before the since floors at zero",
			phase: FaultPhaseGaveUp, ref: bare,
			since: faultBase, now: faultBase.Add(-3 * time.Second),
			want: "Gave up after 0 s",
		},
		{
			// A code with no text still names the thing to quote to the vendor.
			name:  "a fleet code with no description still names itself",
			phase: FaultPhaseLive, ref: TermRef{VendorCode: 54018},
			since: faultBase, now: faultBase.Add(2 * time.Minute), notice: true,
			want: "Fault · fleet code 54018",
		},
		{
			name:  "fleet text is lower-cased as received, never translated",
			phase: FaultPhaseLive, ref: TermRef{VendorCode: 60011, VendorDesc: "Robot Suspended"},
			since: faultBase, now: faultBase.Add(2 * time.Minute), notice: true,
			want: "Fault · robot suspended (60011)",
		},
		{
			name:  "an unknown phase renders nothing rather than guessing",
			phase: FaultPhase("dunno"), ref: vendor,
			since: faultBase, now: faultBase.Add(time.Minute), notice: true,
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatFaultSentence(tc.phase, tc.ref, tc.since, tc.now, tc.notice)
			if got != tc.want {
				t.Errorf("FormatFaultSentence(%s, …) = %q, want %q", tc.phase, got, tc.want)
			}
		})
	}
}

// TestFormatFaultSentence_NeverSaysFailAboutALiveOrder is the wording rule, and
// it is a rule rather than a preference.
//
// A faulted order is still live: the robot can recover it (706 of 730 did, with
// a median of 20 seconds), an operator can finish or cancel it. "Failing"
// printed on all 730 is how a floor learns to ignore the word by the time it
// appears on one of the 24 that mattered. The word belongs to the `failed`
// badge of an order that is actually terminal.
//
// Recovered is live-adjacent and covered too — an order that recovered did the
// opposite of failing. Only FaultPhaseGaveUp describes an order that has ended,
// and it says "gave up", which is what Core did, not what the robot did.
func TestFormatFaultSentence_NeverSaysFailAboutALiveOrder(t *testing.T) {
	refs := []TermRef{
		{},
		{Node: "ALN_003"},
		{VendorCode: 60011, VendorDesc: "cannot replan"},
		// The nightmare input: a fleet that hands us the word itself.
		{VendorCode: 999, VendorDesc: "Task Failed"},
	}
	elapsed := []time.Duration{0, 14 * time.Second, time.Minute, 45 * time.Minute}

	for _, phase := range []FaultPhase{FaultPhaseLive, FaultPhaseRecovered} {
		for _, ref := range refs {
			for _, d := range elapsed {
				for _, notice := range []bool{false, true} {
					got := FormatFaultSentence(phase, ref, faultBase, faultBase.Add(d), notice)
					if ref.VendorDesc == "Task Failed" && phase == FaultPhaseLive && notice {
						// The fleet's own words are quoted verbatim; that is
						// the one place the substring may legitimately appear,
						// and it is attributed to the fleet by the code beside
						// it. Assert the shape rather than exempting it.
						if !strings.HasPrefix(got, "Fault · task failed (999)") {
							t.Errorf("quoted fleet text changed shape: %q", got)
						}
						continue
					}
					if strings.Contains(strings.ToLower(got), "fail") {
						t.Errorf("FormatFaultSentence(%s, %+v, notice=%v) = %q — "+
							"a live order must never be described with \"fail\"",
							phase, ref, notice, got)
					}
				}
			}
		}
	}
}

// The ladder is protocol's now, and www.FormatDuration delegates to it. If this
// drifts, every duration on every page drifts with it.
func TestFormatDuration_Ladder(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{-5 * time.Second, "0 s"},
		{0, "0 s"},
		{18 * time.Second, "18 s"},
		{59 * time.Second, "59 s"},
		{time.Minute, "1m 00s"},
		{3*time.Minute + 12*time.Second, "3m 12s"},
		{4*time.Minute + 7*time.Second, "4m 07s"},
		{45 * time.Minute, "45m"},
		{90 * time.Minute, "1h 30m"},
		{25 * time.Hour, "1d 01h"},
	}
	for _, tc := range tests {
		if got := FormatDuration(tc.d); got != tc.want {
			t.Errorf("FormatDuration(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
