package plantspec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validPlant returns a minimal spec that passes Validate; tests mutate a copy
// to exercise each failure mode.
func validPlant() *Plant {
	return &Plant{
		Namespace: "devplant",
		LineID:    "line1",
		BinTypes:  []string{"STANDARD"},
		Payloads: []Payload{
			{Code: "PART-A", UOPCapacity: 1000, BinType: "STANDARD"},
		},
		Zones: []Zone{{
			Name: "SM-A", RetrieveAlgorithm: "FIFO", StoreAlgorithm: "DPTH",
			Lanes: []Lane{{Name: "SM-A-LANE-1", Slots: []Slot{
				{Name: "SM-A01", Depth: 1}, {Name: "SM-A02", Depth: 2},
			}}},
		}},
		Stations: []Station{
			{Name: "LINE1-IN", Kind: "line_in"},
			{Name: "LOADER-1", Kind: "loader"},
			{Name: "STAGE-P1-IN", Kind: "staging"},
			{Name: "STAGE-P1-OUT", Kind: "staging"},
			{Name: "PRESS-1", Kind: "press"},
		},
		Processes:        []Process{{Name: "PRESS-LINE", ActiveStyle: "STYLE-A"}},
		Styles:           []Style{{Name: "STYLE-A", Process: "PRESS-LINE", Payload: "PART-A"}},
		OperatorStations: []string{"PRESS-OPS"},
		Bins: []Bin{
			{Name: "BIN-1", Slot: "SM-A01", Payload: "PART-A", UOP: 1000, BinType: "STANDARD"},
			{Name: "BIN-2", Slot: "SM-A02"},
		},
		Claims: []Claim{
			{CoreNode: "LOADER-1", Style: "STYLE-A", Role: "produce", SwapMode: "manual_swap",
				Payload: "PART-A", UOPCapacity: 1000, InboundSource: "SM-A", OutboundDestination: "SM-A"},
			{CoreNode: "PRESS-1", Style: "STYLE-A", Role: "produce", SwapMode: "single_robot",
				Payload: "PART-A", UOPCapacity: 30, InboundStaging: "STAGE-P1-IN", OutboundStaging: "STAGE-P1-OUT"},
			{CoreNode: "LINE1-IN", Style: "STYLE-A", Role: "consume", SwapMode: "sequential",
				Payload: "PART-A", UOPCapacity: 1000, InboundSource: "SM-A"},
		},
		Demands:         []Demand{{Payload: "PART-A", Node: "LOADER-1"}},
		ReportingPoints: []ReportingPoint{{PLCName: "PRESS-1", TagName: "PRESS-1_COUNTER", Node: "PRESS-1"}},
		CellConfigs:     []CellConfig{{Process: "PRESS-LINE", Station: "PRESS-OPS"}},
		LinesideBuckets: []LinesideBucket{{Node: "LINE1-IN", Payload: "PART-A", Qty: 50}},
	}
}

func TestValidate_GoldenPlantPasses(t *testing.T) {
	if err := validPlant().Validate(); err != nil {
		t.Fatalf("valid plant should pass, got: %v", err)
	}
}

func TestValidate_CatchesProblems(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Plant)
		want   string
	}{
		{"unknown claim node", func(p *Plant) { p.Claims[0].CoreNode = "NOPE" }, "unknown core_node"},
		{"single_robot missing staging", func(p *Plant) { p.Claims[1].InboundStaging = "" }, "requires inbound_staging"},
		{"manual_swap missing dest", func(p *Plant) { p.Claims[0].OutboundDestination = "" }, "requires outbound_destination"},
		{"simple retired", func(p *Plant) { p.Claims[2].SwapMode = "simple" }, "retired"},
		{"no storage hierarchy", func(p *Plant) { p.Zones = nil }, "storage hierarchy missing"},
		{"payload unknown bin type", func(p *Plant) { p.Payloads[0].BinType = "BOGUS" }, "unknown bin_type"},
		{"bin at unknown node", func(p *Plant) { p.Bins[0].Slot = "GHOST" }, "unknown node"},
		{"demand unknown payload", func(p *Plant) { p.Demands[0].Payload = "PART-Z" }, "unknown payload"},
		{"reporting point unknown node", func(p *Plant) { p.ReportingPoints[0].Node = "GHOST" }, "unknown node"},
		{"style unknown process", func(p *Plant) { p.Styles[0].Process = "NOPE" }, "unknown process"},
		{"slot zero depth", func(p *Plant) { p.Zones[0].Lanes[0].Slots[0].Depth = 0 }, "non-positive depth"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validPlant()
			tc.mutate(p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("expected validation error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got: %v", tc.want, err)
			}
		})
	}
}

func TestLoad_RoundTrip(t *testing.T) {
	yaml := `
namespace: devplant
line_id: line1
bin_types: [STANDARD]
payloads:
  - {code: PART-A, uop_capacity: 1000, bin_type: STANDARD}
zones:
  - name: SM-A
    retrieve_algorithm: FIFO
    store_algorithm: DPTH
    lanes:
      - name: SM-A-LANE-1
        slots:
          - {name: SM-A01, depth: 1}
stations:
  - {name: LINE1-IN, kind: line_in}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "demo.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Namespace != "devplant" || p.LineID != "line1" {
		t.Fatalf("header not parsed: %+v", p)
	}
	if len(p.Zones) != 1 || len(p.Zones[0].Lanes) != 1 || len(p.Zones[0].Lanes[0].Slots) != 1 {
		t.Fatalf("zone hierarchy not parsed: %+v", p.Zones)
	}
	if p.Zones[0].Lanes[0].Slots[0].Depth != 1 {
		t.Fatalf("slot depth not parsed: %+v", p.Zones[0].Lanes[0].Slots[0])
	}
}
