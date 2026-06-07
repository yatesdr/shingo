package messaging

import (
	"testing"

	"shingo/protocol"
)

// TestIsProductionTick pins the §14 production gate (Delta > 0, real style,
// not an unconfirmed jump). The cat_id wiring is blocked (Q-024) but the gate
// itself is the part that's ready, so it's tested now.
func TestIsProductionTick(t *testing.T) {
	cases := []struct {
		name string
		snap protocol.CounterSnapshot
		want bool
	}{
		{"normal produce", protocol.CounterSnapshot{Delta: 1, StyleID: 7}, true},
		{"zero delta", protocol.CounterSnapshot{Delta: 0, StyleID: 7}, false},
		{"negative delta", protocol.CounterSnapshot{Delta: -1, StyleID: 7}, false},
		{"no style (changeover/unknown)", protocol.CounterSnapshot{Delta: 1, StyleID: 0}, false},
		{"jump anomaly excluded", protocol.CounterSnapshot{Delta: 1, StyleID: 7, Anomaly: "jump"}, false},
	}
	for _, c := range cases {
		if got := isProductionTick(&c.snap); got != c.want {
			t.Errorf("%s: isProductionTick = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestIsProductionReason pins the §14 demand-counter classifier. The counter
// is keyed by payload_code, so BOTH directions are production: produce_tick
// (a part is made) and consume_tick / ab_fallthrough (a sub is drawn down as
// it's produced into a downstream FG/WIP). Corrections and operator releases
// are NOT production throughput.
func TestIsProductionReason(t *testing.T) {
	cases := []struct {
		reason protocol.BinUOPDeltaReason
		want   bool
	}{
		{protocol.ReasonProduceTick, true},
		{protocol.ReasonConsumeTick, true},
		{protocol.ReasonABFallthrough, true},
		{protocol.ReasonCaptureReduction, false}, // operator pull-to-lineside on release
		{protocol.ReasonOperatorCorrection, false},
		{protocol.BinUOPDeltaReason("unknown_future_reason"), false},
		{protocol.BinUOPDeltaReason(""), false},
	}
	for _, c := range cases {
		if got := isProductionReason(c.reason); got != c.want {
			t.Errorf("isProductionReason(%q) = %v, want %v", c.reason, got, c.want)
		}
	}
}
