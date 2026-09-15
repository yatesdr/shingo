package dispatch

import "testing"

// The whole rule surface, as data. decideRobotGroup is pure, so none of this
// needs a database, a fake, or a Dispatcher — which is the point of having
// gathered the facts in the caller.
func TestDecideRobotGroup(t *testing.T) {
	t.Parallel()

	const (
		heavy = "HEAVY-1500"
		light = "LIGHT-600"
		rack  = "RACK-ONLY"
	)

	// relaxable is a payload configured the way a plant actually would: heavy
	// while loaded, relaxed to the small robots at or below 20%.
	relaxable := groupFacts{
		payloadCode:    "PANEL-A",
		capacity:       2160,
		payloadGroup:   heavy,
		nearEmptyGroup: light,
		allowNearEmpty: true,
		thresholdPct:   20,
	}
	with := func(base groupFacts, f func(*groupFacts)) groupFacts {
		f(&base)
		return base
	}

	tests := []struct {
		name      string
		facts     groupFacts
		wantGroup string
		wantRule  rule
	}{
		// ── the unconfigured plant ────────────────────────────────────────
		{
			// THE REGRESSION THAT MATTERS MOST. Every plant that has not
			// touched this feature must dispatch exactly what
			// robotGroupForPayload used to: the payload's own group, which for
			// blank facts is "" — the vendor default.
			name:      "zero value decides the vendor default",
			facts:     groupFacts{},
			wantGroup: "",
			wantRule:  ruleNoPayload,
		},
		{
			name:      "configured payload, relaxation off, full bin",
			facts:     groupFacts{payloadCode: "PANEL-A", remaining: 2000, capacity: 2160, payloadGroup: heavy},
			wantGroup: heavy,
			wantRule:  ruleAboveThreshold,
		},
		{
			// Relaxation off: an empty bin still goes to the payload's group.
			// Off must mean off at every fill level, not just above threshold.
			name:      "relaxation off, empty bin, still the payload group",
			facts:     groupFacts{payloadCode: "PANEL-A", remaining: 0, capacity: 2160, payloadGroup: heavy},
			wantGroup: heavy,
			wantRule:  ruleEmpty,
		},

		// ── the feature ───────────────────────────────────────────────────
		{
			name:      "above the threshold stays heavy",
			facts:     with(relaxable, func(f *groupFacts) { f.remaining = 967 }),
			wantGroup: heavy,
			wantRule:  ruleAboveThreshold,
		},
		{
			// INCLUSIVE BOUNDARY, pinned from both sides. 432*100 == 2160*20.
			name:      "exactly at the threshold relaxes",
			facts:     with(relaxable, func(f *groupFacts) { f.remaining = 432 }),
			wantGroup: light,
			wantRule:  ruleNearEmpty,
		},
		{
			name:      "one unit above the threshold does not relax",
			facts:     with(relaxable, func(f *groupFacts) { f.remaining = 433 }),
			wantGroup: heavy,
			wantRule:  ruleAboveThreshold,
		},
		{
			name:      "below the threshold relaxes",
			facts:     with(relaxable, func(f *groupFacts) { f.remaining = 10 }),
			wantGroup: light,
			wantRule:  ruleNearEmpty,
		},
		{
			name:      "drained relaxes before the threshold is consulted",
			facts:     with(relaxable, func(f *groupFacts) { f.remaining = 0 }),
			wantGroup: light,
			wantRule:  ruleEmpty,
		},
		{
			// A blank near-empty group under an enabled flag is deliberate:
			// relax to the vendor-default pool, i.e. any robot. This is the
			// config near_empty_enabled exists to make expressible.
			name: "enabled with a blank group relaxes to the vendor default",
			facts: with(relaxable, func(f *groupFacts) {
				f.remaining, f.nearEmptyGroup = 10, ""
			}),
			wantGroup: "",
			wantRule:  ruleNearEmpty,
		},
		{
			// threshold 0 is not the off switch — it relaxes only an exactly
			// empty bin, which ruleEmpty already owns.
			name: "threshold zero does not relax a partial bin",
			facts: with(relaxable, func(f *groupFacts) {
				f.remaining, f.thresholdPct = 1, 0
			}),
			wantGroup: heavy,
			wantRule:  ruleAboveThreshold,
		},
		{
			// THE INTEGER-DIVISION TRAP. remaining/capacity truncates to 0 and
			// 0 <= 0, so the naive form relaxes here. Cross-multiplication does
			// not: 1000*100 is not <= 2160*0.
			name: "partially drained bin at threshold zero must not relax",
			facts: with(relaxable, func(f *groupFacts) {
				f.remaining, f.thresholdPct = 1000, 0
			}),
			wantGroup: heavy,
			wantRule:  ruleAboveThreshold,
		},

		// ── the carrier refusal ───────────────────────────────────────────
		{
			// The carrier replaces the relaxed outcome, so a payload that
			// permits the small robots cannot hand them this rack.
			name: "carrier refusal beats the payload relaxation",
			facts: with(relaxable, func(f *groupFacts) {
				f.remaining, f.requiredGroup = 10, rack
			}),
			wantGroup: rack,
			wantRule:  ruleNearEmpty,
		},
		{
			name: "carrier refusal covers the empty carrier",
			facts: groupFacts{
				payloadCode: "", remaining: 0, requiredGroup: rack,
			},
			wantGroup: rack,
			wantRule:  ruleNoPayload,
		},
		{
			// THE OTHER HALF, and the correction that produced this shape: a
			// LOADED bin goes to the payload's group, never the carrier's. A
			// carrier misconfigured to something lighter must not be able to
			// pull a full heavy load down to it.
			name: "a loaded bin ignores the carrier group",
			facts: with(relaxable, func(f *groupFacts) {
				f.remaining, f.requiredGroup = 2000, light
			}),
			wantGroup: heavy,
			wantRule:  ruleAboveThreshold,
		},

		// ── data this feature must not trust ──────────────────────────────
		{
			// A payload-bearing bin reading negative is not drained, it is
			// broken. Without ruleNegativeCount this falls into the near-empty
			// test at -69% and relaxes.
			name: "payload-bearing negative count does not relax",
			facts: with(relaxable, func(f *groupFacts) {
				f.remaining = -1497
			}),
			wantGroup: heavy,
			wantRule:  ruleNegativeCount,
		},
		{
			// Over capacity is live data at Hopkinsville (2161 of 2160).
			// Nothing may trip on >100%.
			name:      "over capacity is simply above the threshold",
			facts:     with(relaxable, func(f *groupFacts) { f.remaining = 2161 }),
			wantGroup: heavy,
			wantRule:  ruleAboveThreshold,
		},
		{
			name: "no capacity on a bin claiming parts does not relax",
			facts: with(relaxable, func(f *groupFacts) {
				f.remaining, f.capacity = 500, 0
			}),
			wantGroup: heavy,
			wantRule:  ruleNoCapacity,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotGroup, gotRule := decideRobotGroup(tt.facts)
			if gotGroup != tt.wantGroup || gotRule != tt.wantRule {
				t.Errorf("decideRobotGroup() = (%q, %s), want (%q, %s)",
					gotGroup, gotRule, tt.wantGroup, tt.wantRule)
			}
		})
	}
}

// The five carriers that were sitting negative at Hopkinsville when this was
// written, as a fixture rather than a remembered anecdote.
//
// All five carry payload_code=” with a count around -1500 — a state
// ClearForReuse cannot produce, since it writes the blank code and the zeroed
// count in one UPDATE. Something applies deltas after the clear. That is not
// this feature's bug, but it is the shape the rule table has to survive: every
// one of them must exit at ruleNoPayload and never reach the near-empty test,
// which would otherwise see -69% and relax.
func TestDecideRobotGroup_HopkinsvilleNegativeCarriers(t *testing.T) {
	t.Parallel()

	for _, remaining := range []int{-1497, -1503, -1500, -1498, -1499} {
		// The payload facts are blank because the BIN's are: BinJoinQuery LEFT
		// JOINs payloads on b.payload_code, so a bare carrier matches no
		// template and every payload column comes back at its COALESCE
		// default. Populating a payload group beside a blank code would be a
		// state the caller cannot produce.
		facts := groupFacts{payloadCode: "", remaining: remaining}

		group, r := decideRobotGroup(facts)
		if r != ruleNoPayload {
			t.Errorf("remaining=%d decided %s, want %s — a bare carrier must never reach the count",
				remaining, r, ruleNoPayload)
		}
		if group != "" {
			t.Errorf("remaining=%d gave group %q, want the vendor default", remaining, group)
		}

		// The same carrier once its type names a required group: still decided
		// by ruleNoPayload, never by the count, but now refused to the rack
		// group. This is the whole mechanism protecting an empty FG rack.
		facts.requiredGroup = "RACK-ONLY"
		group, r = decideRobotGroup(facts)
		if r != ruleNoPayload || group != "RACK-ONLY" {
			t.Errorf("remaining=%d with a carrier refusal = (%q, %s), want (%q, %s)",
				remaining, group, r, "RACK-ONLY", ruleNoPayload)
		}
	}
}

func TestNearEmptyPct(t *testing.T) {
	t.Parallel()

	tests := []struct {
		remaining, capacity, want int
	}{
		{432, 2160, 20},
		{2161, 2160, 100}, // over capacity, reported not clamped
		{0, 2160, 0},
		{-1497, 2160, -69},
		{500, 0, -1}, // no denominator
		{0, 0, -1},
	}
	for _, tt := range tests {
		if got := nearEmptyPct(tt.remaining, tt.capacity); got != tt.want {
			t.Errorf("nearEmptyPct(%d, %d) = %d, want %d",
				tt.remaining, tt.capacity, got, tt.want)
		}
	}
}
