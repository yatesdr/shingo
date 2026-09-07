package plantspec

import (
	"path/filepath"
	"strings"
	"testing"

	"shingo/protocol"
)

// carrier_pairs_test.go — the A/B two-container rule (carrier_pairs.go).
//
// The owner's ruling is the spec: "if a process wants to run an A/B swap it
// needs at least two containers in the system. Two nodes lineside holding the
// same part — it can't function with less than 2."

// abPlant builds a spec with one paired (style, payload) and n carriers of the
// payload's bin type, over the validPlant skeleton so nothing else refuses.
func abPlant(t *testing.T, pairedClaims int, carriers int) *Plant {
	t.Helper()
	p := validPlant()
	// Replace the golden plant's single unpaired consume claim with a paired
	// A/B pair on the line_in node: LINE1-IN active, LINE2-IN parked, naming
	// each other — the measured shape of a sequential pair, regardless of the
	// mode string the claims carry.
	p.Stations = append(p.Stations, Station{Name: "LINE2-IN", Kind: "line_in"})
	p.Claims = nil
	claims := []Claim{
		{CoreNode: "LOADER-1", Style: "STYLE-A", Role: "produce", SwapMode: "manual_swap",
			Payload: "PART-A", UOPCapacity: 1000, InboundSource: "SM-A", OutboundDestination: "SM-A"},
		{CoreNode: "PRESS-1", Style: "STYLE-A", Role: "produce", SwapMode: "single_robot",
			Payload: "PART-A", UOPCapacity: 30, InboundStaging: "STAGE-P1-IN", OutboundStaging: "STAGE-P1-OUT"},
	}
	if pairedClaims > 0 {
		claims = append(claims,
			Claim{CoreNode: "LINE1-IN", Style: "STYLE-A", Role: "consume", SwapMode: "sequential",
				Payload: "PART-A", UOPCapacity: 1000, InboundSource: "SM-A", PairedCoreNode: "LINE2-IN"},
			Claim{CoreNode: "LINE2-IN", Style: "STYLE-A", Role: "consume", SwapMode: "sequential",
				Payload: "PART-A", UOPCapacity: 1000, InboundSource: "SM-A", PairedCoreNode: "LINE1-IN",
				ActivePull: boolPtr(false)})
	} else {
		claims = append(claims,
			Claim{CoreNode: "LINE1-IN", Style: "STYLE-A", Role: "consume", SwapMode: "sequential",
				Payload: "PART-A", UOPCapacity: 1000, InboundSource: "SM-A"})
	}
	p.Claims = claims
	p.Demands = []Demand{{Payload: "PART-A", Node: "LOADER-1"}}
	p.LinesideBuckets = []LinesideBucket{{Node: "LINE1-IN", Payload: "PART-A", Qty: 50}}

	// Exactly `carriers` bins of STANDARD, the payload's bin type — one slot
	// each, distinct names, the count is the whole point.
	p.Bins = nil
	for i := 0; i < carriers; i++ {
		p.Bins = append(p.Bins, Bin{
			Name: "BIN-" + string(rune('1'+i)), Slot: "SM-A0" + string(rune('1'+i)),
			Payload: "PART-A", UOP: 1000, BinType: "STANDARD",
		})
	}
	return p
}

func boolPtr(b bool) *bool { return &b }

// TestCarrierPairs_RedOnAViolatingFixture: two paired positions, one carrier.
// The rule exists for exactly this seed — the pair cannot rotate.
func TestCarrierPairs_RedOnAViolatingFixture(t *testing.T) {
	t.Parallel()
	p := abPlant(t, 2, 1)
	err := p.Validate()
	if err == nil {
		t.Fatal("a seed with two paired positions and one carrier validated; the pair cannot rotate")
	}
	if !strings.Contains(err.Error(), "CARRIER SHORTAGE AT BIRTH") {
		t.Fatalf("refused for some other reason: %v", err)
	}
	if !strings.Contains(err.Error(), "LINE1-IN, LINE2-IN") {
		t.Errorf("the message does not name the pair: %v", err)
	}
	if !strings.Contains(err.Error(), "it can't function with less than 2") {
		t.Errorf("the message does not quote the ruling: %v", err)
	}
}

// TestCarrierPairs_ExactTwoMarginPasses: two paired positions, two carriers —
// the ruling's own minimum, and it must load.
func TestCarrierPairs_ExactTwoMarginPasses(t *testing.T) {
	t.Parallel()
	p := abPlant(t, 2, 2)
	if err := p.Validate(); err != nil {
		t.Fatalf("the ruling's own minimum (2 positions, 2 carriers) must validate: %v", err)
	}
}

// TestCarrierPairs_AnUnpairedClaimCannotTripIt: the arm counts paired
// positions, not claims — a plant with no paired_core_node anywhere has no
// pair to refuse, whatever its modes.
func TestCarrierPairs_AnUnpairedClaimCannotTripIt(t *testing.T) {
	t.Parallel()
	p := abPlant(t, 0, 2)
	if err := p.Validate(); err != nil {
		t.Fatalf("an unpaired plant refused: %v", err)
	}
	if cp := p.CarrierPairsAtBirth(); !cp.Clean() {
		t.Fatalf("an unpaired plant reported shortages: %v", cp.Findings())
	}
}

// TestCarrierPairs_TheWalkerOverConfigurableSwapModes drives EVERY
// configurable mode through the paired geometry, so a new paired mode inherits
// the check the day it is added to ConfigurableSwapModes — and a mode that
// does not pair positions cannot trip it, which is the discriminator the
// walker pins from both sides.
func TestCarrierPairs_TheWalkerOverConfigurableSwapModes(t *testing.T) {
	t.Parallel()
	for _, mode := range protocol.ConfigurableSwapModes() {
		p := abPlant(t, 2, 1)
		for i := range p.Claims {
			if p.Claims[i].SwapMode == "sequential" {
				p.Claims[i].SwapMode = mode.String()
			}
		}
		// Shape legality per mode: press_index needs paired+outbound (present),
		// single_robot needs its staging (already on PRESS-1; the LINE claims
		// carry none). Silence only the per-mode field arms that would fire
		// first, by giving the pair staging when the mode demands it.
		switch mode {
		case protocol.SwapModeSingleRobot:
			for i := range p.Claims {
				if p.Claims[i].CoreNode == "LINE1-IN" || p.Claims[i].CoreNode == "LINE2-IN" {
					p.Claims[i].InboundStaging = "STAGE-P1-IN"
					p.Claims[i].OutboundStaging = "STAGE-P1-OUT"
				}
			}
		case protocol.SwapModeTwoRobot:
			for i := range p.Claims {
				if p.Claims[i].CoreNode == "LINE1-IN" || p.Claims[i].CoreNode == "LINE2-IN" {
					p.Claims[i].InboundStaging = "STAGE-P1-IN"
				}
			}
		case protocol.SwapModeTwoRobotPressIndex:
			for i := range p.Claims {
				if p.Claims[i].CoreNode == "LINE1-IN" || p.Claims[i].CoreNode == "LINE2-IN" {
					p.Claims[i].OutboundDestination = "SM-A"
				}
			}
		}
		// THE GEOMETRY, NOT THE MODE NAME, carries the check: every
		// configurable mode driven over the same paired shape must report the
		// same shortage, because the pair is two positions holding one part
		// whatever choreography swaps them. A new mode added to
		// ConfigurableSwapModes inherits this on the day it lands.
		cp := p.CarrierPairsAtBirth()
		if cp.Clean() {
			t.Errorf("mode %s over paired geometry reported no shortage — the walker no longer "+
				"covers it and a new paired mode could ship unrefused", mode)
		}
		if len(cp.Shortages) != 1 || len(cp.Shortages[0].Positions) != 2 {
			t.Errorf("mode %s: want exactly one two-position shortage, got %+v", mode, cp.Shortages)
		}
	}
}

// TestCarrierPairs_TheCommittedSpecsReportTheirMargins: every committed spec
// loads with the arm wired, and one line per paired (style, payload) shows the
// margin. This is the dry run, kept.
func TestCarrierPairs_TheCommittedSpecsReportTheirMargins(t *testing.T) {
	t.Parallel()
	specs, err := filepath.Glob(filepath.Join("..", "..", "plants", "*.yaml"))
	if err != nil || len(specs) == 0 {
		t.Fatalf("no plant specs: %v", err)
	}
	for _, spec := range specs {
		p, err := Load(spec)
		if err != nil {
			t.Errorf("%s: load: %v", spec, err)
			continue
		}
		if err := p.Validate(); err != nil {
			t.Errorf("%s no longer validates under the two-container arm: %v", filepath.Base(spec), err)
		}
		if cp := p.CarrierPairsAtBirth(); !cp.Clean() {
			t.Errorf("%s: %v", filepath.Base(spec), cp.Findings())
		}
	}
}
