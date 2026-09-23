package plantspec

import (
	"strings"
	"testing"
)

// stage2_pin_test.go — the second stage of a two-stage unloader as a spec: a
// consume shared-window loader with NO payload. Stage 1's U2 feeds it an empty
// carrier directly, so it has nothing to pull and no part to name.

// withStageTwo adds a zero-payload consume window (S2-W1, window_of
// S2-UNLOADER) to the golden plant.
func withStageTwo(p *Plant) {
	p.Stations = append(p.Stations, Station{Name: "S2-W1", Kind: "unloader"})
	p.Claims = append(p.Claims, Claim{CoreNode: "S2-W1", Style: "STYLE-A", Role: "consume",
		SwapMode: "manual_swap", WindowOf: "S2-UNLOADER", OutboundDestination: "SM-A"})
}

// TestPinValidate_AZeroPayloadUnloaderIsRefusedByParity pins today's refusal:
// the zero-payload window counts as a consumer of payload "", which nothing
// produces, and that is the only finding.
func TestPinValidate_AZeroPayloadUnloaderIsRefusedByParity(t *testing.T) {
	p := validPlant()
	withStageTwo(p)
	err := p.Validate()
	if err == nil {
		t.Fatal("a zero-payload unloader validated")
	}
	msg := err.Error()
	if !strings.Contains(msg, `payload "" has 1 consumer(s) but no producer`) {
		t.Fatalf("err = %v, want the parity refusal for payload \"\"", err)
	}
	if !strings.Contains(msg, "(1 problem(s))") {
		t.Errorf("want the parity line as the only finding, got: %v", err)
	}
}

// TestPinValidate_AZeroPayloadLoaderIsRefused pins the produce side: a
// zero-payload produce loader is refused (today by the same parity check,
// payload "" produced but never consumed).
func TestPinValidate_AZeroPayloadLoaderIsRefused(t *testing.T) {
	p := validPlant()
	p.Stations = append(p.Stations, Station{Name: "L0-W1", Kind: "loader"})
	p.Claims = append(p.Claims, Claim{CoreNode: "L0-W1", Style: "STYLE-A", Role: "produce",
		SwapMode: "manual_swap", WindowOf: "L0-LOADER", InboundSource: "SM-A", OutboundDestination: "SM-A"})
	err := p.Validate()
	if err == nil {
		t.Fatal("a zero-payload loader validated")
	}
	if !strings.Contains(err.Error(), `payload "" has 1 producer(s) but no consumer`) {
		t.Fatalf("err = %v, want the parity refusal for payload \"\"", err)
	}
}
