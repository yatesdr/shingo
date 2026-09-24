package messaging

import (
	"testing"

	"shingo/protocol"
)

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
