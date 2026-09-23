package plantspec

import (
	"strings"
	"testing"
)

// stage2_test.go — the second stage of a two-stage unloader as a spec: a
// consume shared-window loader with NO payload. Stage 1's U2 feeds it an empty
// carrier directly, so it has nothing to pull and no part to name.

// withStageTwo adds a zero-payload consume window (S2-W1, window_of
// S2-UNLOADER) to the golden plant.
func withStageTwo(p *Plant) {
	p.Stations = append(p.Stations, Station{Name: "S2-W1", Kind: "unloader"})
	p.Claims = append(p.Claims, Claim{CoreNode: "S2-W1", Style: "STYLE-A", Role: "consume",
		SwapMode: "manual_swap", WindowOf: "S2-UNLOADER", OutboundDestination: "SM-A"})
}

// TestValidate_AZeroPayloadUnloaderIsAllowed: stage 2 validates. It used to be
// refused as the only consumer of payload "" (`payload "" has 1 consumer(s)
// but no producer`), a shape the Edge allows and the seeder already builds.
func TestValidate_AZeroPayloadUnloaderIsAllowed(t *testing.T) {
	p := validPlant()
	withStageTwo(p)
	if err := p.Validate(); err != nil {
		t.Fatalf("a zero-payload unloader window was refused: %v", err)
	}
}

// TestValidate_AZeroPayloadDedicatedUnloaderIsStillRefused: a dedicated
// position is one payload, so a home_of unloader naming none keeps today's
// parity refusal.
func TestValidate_AZeroPayloadDedicatedUnloaderIsStillRefused(t *testing.T) {
	p := validPlant()
	p.Stations = append(p.Stations, Station{Name: "D0-P1", Kind: "unloader"})
	p.Claims = append(p.Claims, Claim{CoreNode: "D0-P1", Style: "STYLE-A", Role: "consume",
		SwapMode: "manual_swap", HomeOf: "D0-UNLOADER", OutboundDestination: "SM-A"})
	err := p.Validate()
	if err == nil || !strings.Contains(err.Error(), `payload "" has 1 consumer(s) but no producer`) {
		t.Fatalf("err = %v, want the parity refusal", err)
	}
}

// TestValidate_AZeroPayloadLoaderIsRefused: a zero-payload produce loader
// is refused, now by its own finding (it used to be parity on payload "",
// produced but never consumed).
func TestValidate_AZeroPayloadLoaderIsRefused(t *testing.T) {
	p := validPlant()
	p.Stations = append(p.Stations, Station{Name: "L0-W1", Kind: "loader"})
	p.Claims = append(p.Claims, Claim{CoreNode: "L0-W1", Style: "STYLE-A", Role: "produce",
		SwapMode: "manual_swap", WindowOf: "L0-LOADER", InboundSource: "SM-A", OutboundDestination: "SM-A"})
	err := p.Validate()
	if err == nil {
		t.Fatal("a zero-payload loader validated")
	}
	if !strings.Contains(err.Error(), "a loader needs at least one payload") {
		t.Fatalf("err = %v, want the loader-needs-a-payload finding", err)
	}
	if strings.Contains(err.Error(), `payload ""`) {
		t.Errorf("parity still reports payload \"\": %v", err)
	}
}
