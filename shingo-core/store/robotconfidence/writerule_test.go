package robotconfidence

import (
	"testing"
	"time"
)

// The write rule is the one-way door in this design: a reading it drops is
// gone, and there is no backfill. These cases are one per clause and one per
// rejection, with the stationary-failure cases called out because they are
// the ones a plausible-looking "only sample when moving" filter destroys.

func TestWriteRule_Decide(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	r := DefaultWriteRule

	// A healthy, parked, idle baseline. Cases below vary one thing at a time.
	base := Observation{Connected: true, RelocStatus: 1, Confidence: 0.95, X: 10, Y: 10}
	stored := func(ago time.Duration) *LastStored {
		return &LastStored{X: 10, Y: 10, Confidence: 0.95, At: now.Add(-ago)}
	}

	cases := []struct {
		name   string
		obs    Observation
		last   *LastStored
		want   bool
		reason string
	}{
		// ── Gates ──────────────────────────────────────────────────────────
		{
			// AMR-11 was connection_status=0 during the survey and still
			// reporting a stale 0.660. A last-known value is not a reading.
			name: "disconnected is never stored",
			obs:  Observation{Connected: false, RelocStatus: 1, Confidence: 0.66, X: 50, Y: 50},
			last: stored(time.Minute), want: false, reason: ReasonDisconnected,
		},
		{
			name: "mid-relocation is never stored",
			obs:  Observation{Connected: true, RelocStatus: 2, Confidence: 0.30, X: 50, Y: 50},
			last: stored(time.Minute), want: false, reason: ReasonRelocating,
		},
		{
			// The gate rejects RELOCING only. COMPLETED is a settled pose and
			// must still be able to reach the clauses below it.
			name: "relocation COMPLETED is not gated out",
			obs: Observation{Connected: true, RelocStatus: 3, Confidence: 0.95,
				X: 10.5, Y: 10},
			last: stored(time.Second), want: true, reason: ReasonMoved,
		},

		// ── First sample ───────────────────────────────────────────────────
		{
			name: "no prior sample is always stored",
			obs:  base,
			last: nil, want: true, reason: ReasonFirst,
		},

		// ── Clause 1: moved ────────────────────────────────────────────────
		{
			name: "moved 0.30 m",
			obs:  Observation{Connected: true, RelocStatus: 1, Confidence: 0.95, X: 10.30, Y: 10},
			last: stored(2 * time.Second), want: true, reason: ReasonMoved,
		},

		// ── Clause 2: changed ──────────────────────────────────────────────
		{
			name: "moved 0.10 m with confidence change 0.03",
			obs:  Observation{Connected: true, RelocStatus: 1, Confidence: 0.92, X: 10.10, Y: 10},
			last: stored(2 * time.Second), want: true, reason: ReasonChanged,
		},
		{
			name: "moved 0.10 m with confidence change 0.005",
			obs:  Observation{Connected: true, RelocStatus: 1, Confidence: 0.945, X: 10.10, Y: 10},
			last: stored(2 * time.Second), want: false, reason: ReasonNoChange,
		},

		// ── Clause 3: low ──────────────────────────────────────────────────
		{
			name: "confidence 0.42 while stationary past 10 s",
			obs:  Observation{Connected: true, RelocStatus: 1, Confidence: 0.42, X: 10, Y: 10},
			last: &LastStored{X: 10, Y: 10, Confidence: 0.42, At: now.Add(-11 * time.Second)},
			want: true, reason: ReasonLow,
		},
		{
			name: "confidence 0.42 while stationary inside 10 s is rate-limited",
			obs:  Observation{Connected: true, RelocStatus: 1, Confidence: 0.42, X: 10, Y: 10},
			last: &LastStored{X: 10, Y: 10, Confidence: 0.42, At: now.Add(-4 * time.Second)},
			want: false, reason: ReasonNoChange,
		},

		// ── Clause 4: stuck ────────────────────────────────────────────────
		{
			// THE REGRESSION THAT MATTERS. A robot on a job that has stopped
			// moving looks identical to a parked robot to any velocity-based
			// filter, and is the exact shape of a mid-route localization
			// failure.
			name: "parked ON A JOB past 30 s is stored",
			obs: Observation{Connected: true, RelocStatus: 1, Confidence: 0.95,
				X: 10, Y: 10, OnTask: true},
			last: stored(31 * time.Second), want: true, reason: ReasonStuck,
		},
		{
			name: "parked on a job inside 30 s is rate-limited",
			obs: Observation{Connected: true, RelocStatus: 1, Confidence: 0.95,
				X: 10, Y: 10, OnTask: true},
			last: stored(20 * time.Second), want: false, reason: ReasonNoChange,
		},

		// ── Clause 5: failed ───────────────────────────────────────────────
		{
			// The case clauses 1–4 structurally cannot catch: localization has
			// FAILED but the robot is reporting a stale HIGH value, is not
			// moving, and is not on a job. Without this clause nothing at all
			// is stored for the one robot that is definitively lost.
			name: "FAILED relocation reporting a stale high value is stored",
			obs: Observation{Connected: true, RelocStatus: 0, Confidence: 0.66,
				X: 10, Y: 10},
			last: &LastStored{X: 10, Y: 10, Confidence: 0.66, At: now.Add(-11 * time.Second)},
			want: true, reason: ReasonFailed,
		},
		{
			name: "FAILED relocation inside 10 s is rate-limited",
			obs: Observation{Connected: true, RelocStatus: 0, Confidence: 0.66,
				X: 10, Y: 10},
			last: &LastStored{X: 10, Y: 10, Confidence: 0.66, At: now.Add(-3 * time.Second)},
			want: false, reason: ReasonNoChange,
		},

		// ── The 92% ────────────────────────────────────────────────────────
		{
			// Parked, healthy, idle, unchanged. At Hopkinsville this was 92%
			// of all samples and every one of them was byte-identical to the
			// last.
			name: "parked idle and unchanged is dropped",
			obs:  base,
			last: stored(5 * time.Minute), want: false, reason: ReasonNoChange,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := r.Decide(tc.obs, tc.last, now)
			if got != tc.want {
				t.Errorf("Decide() = %v (%s), want %v (%s)", got, reason, tc.want, tc.reason)
			}
			if reason != tc.reason {
				t.Errorf("reason = %q, want %q", reason, tc.reason)
			}
		})
	}
}

// A robot creeping below the movement dead-band on every single poll must
// still be sampled once it has covered the distance cumulatively. This is why
// LastStored tracks the last sample WRITTEN rather than the last one seen —
// comparing consecutive observations would never trip the band at all.
func TestWriteRule_DeadBandIsCumulative(t *testing.T) {
	r := DefaultWriteRule
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	last := &LastStored{X: 0, Y: 0, Confidence: 0.95, At: now}

	var stored bool
	for i := 1; i <= 5; i++ {
		// 0.06 m per poll: never enough on its own, 0.30 m by the fifth.
		obs := Observation{Connected: true, RelocStatus: 1, Confidence: 0.95,
			X: float64(i) * 0.06, Y: 0}
		now = now.Add(2 * time.Second)
		if ok, _ := r.Decide(obs, last, now); ok {
			stored = true
			if i != 5 {
				t.Fatalf("stored on poll %d; 0.25 m not covered until poll 5", i)
			}
		}
	}
	if !stored {
		t.Fatal("a robot that crept 0.30 m was never sampled")
	}
}
