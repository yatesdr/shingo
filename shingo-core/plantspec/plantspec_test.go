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

// maintainedPlant is validPlant plus a flat zone and a maintained group on it —
// the smallest spec that declares a level. Tests mutate a copy to exercise each
// refusal.
//
// Every case here MIRRORS a save-time refusal in the settings modal, and that is
// the point of testing them together: a spec that can declare a configuration the
// UI refuses would seed a plant nobody could then edit, and the first save of an
// untouched screen would come back with a reason the operator would be right to
// read as a bug.
func maintainedPlant() *Plant {
	p := validPlant()
	p.BinTypes = append(p.BinTypes, "SMALL")
	p.Zones = append(p.Zones, Zone{
		Name: "PRESS-EMPTIES", RetrieveAlgorithm: "FIFO", StoreAlgorithm: "LKND",
		Positions: []Slot{
			{Name: "PEB-01", Depth: 1}, {Name: "PEB-02", Depth: 1},
			{Name: "PEB-03", Depth: 1}, {Name: "PEB-04", Depth: 1},
		},
	})
	p.MaintainedGroups = []MaintainedGroup{{
		Group:   "PRESS-EMPTIES",
		Station: "devplant.line1",
		Levels: []MaintainLevel{
			{BinType: "STANDARD", Want: 2},
			{BinType: "SMALL", Want: 1},
		},
		Supports: []string{"PRESS-LINE"},
	}}
	return p
}

func TestValidate_MaintainedGroupPasses(t *testing.T) {
	if err := maintainedPlant().Validate(); err != nil {
		t.Fatalf("a valid maintained group should pass, got: %v", err)
	}
}

func TestValidate_MaintainedGroupRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Plant)
		want   string
	}{
		{"unknown zone", func(p *Plant) { p.MaintainedGroups[0].Group = "NOPE" }, "unknown zone"},
		{"names a station", func(p *Plant) { p.MaintainedGroups[0].Group = "PRESS-1" }, "a maintained group is a zone"},
		// Flat, because the save-time rule is flat: a lane means a carrier can be
		// buried, and a level counted over buried carriers is a number whose
		// meaning changes with what is parked in front of it.
		{"not flat", func(p *Plant) {
			p.Zones[1].Lanes = []Lane{{Name: "PEB-LANE", Slots: []Slot{{Name: "PEB-L1", Depth: 1}}}}
		}, "a maintained group is flat"},
		{"no positions", func(p *Plant) { p.Zones[1].Positions = nil }, "no positions to hold a level"},
		// projectOrder no-ops on a blank StationID.
		{"no station", func(p *Plant) { p.MaintainedGroups[0].Station = "" }, "show on no board"},
		{"no levels", func(p *Plant) { p.MaintainedGroups[0].Levels = nil }, "declares no levels"},
		{"unknown bin type", func(p *Plant) { p.MaintainedGroups[0].Levels[0].BinType = "BOGUS" }, "unknown bin_type"},
		{"duplicate bin type", func(p *Plant) { p.MaintainedGroups[0].Levels[1].BinType = "STANDARD" }, "twice"},
		{"negative want", func(p *Plant) { p.MaintainedGroups[0].Levels[0].Want = -1 }, "cannot be negative"},
		// The episode key is `mnt|<group>|<type>`.
		{"pipe in bin type", func(p *Plant) {
			p.BinTypes = append(p.BinTypes, "BIG|SMALL")
			p.MaintainedGroups[0].Levels[0].BinType = "BIG|SMALL"
		}, "episode key"},
		{"unknown process", func(p *Plant) { p.MaintainedGroups[0].Supports = []string{"GHOST"} }, "unknown process"},
		{"overflow to itself", func(p *Plant) { p.MaintainedGroups[0].Overflow = "PRESS-EMPTIES" }, "overflows to itself"},
		{"overflow not a zone", func(p *Plant) { p.MaintainedGroups[0].Overflow = "PRESS-1" }, "not a declared zone"},
		{"duplicate group", func(p *Plant) {
			p.MaintainedGroups = append(p.MaintainedGroups, p.MaintainedGroups[0])
		}, "duplicate maintained_group"},
		// A position hangs directly off the group, so nothing can be buried behind
		// it and its depth is 1.
		{"position not depth 1", func(p *Plant) { p.Zones[1].Positions[0].Depth = 2 }, "its depth is 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := maintainedPlant()
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

// The two save-time WARNINGS stay warnings there and are ABSENT here. A seed is
// allowed to be in a state a plant is allowed to be in, and a spec that refused
// what the UI merely mentions would be a second, stricter rulebook.
func TestValidate_MaintainedGroupDoesNotRefuseWarnings(t *testing.T) {
	p := maintainedPlant()
	// Level fills every position — the UI warns, nothing refuses.
	p.MaintainedGroups[0].Levels = []MaintainLevel{{BinType: "STANDARD", Want: 4}}
	if err := p.Validate(); err != nil {
		t.Fatalf("a full-house level must not be a spec error: %v", err)
	}
	// want=0 declares the type and asks for none of it — a different statement
	// from leaving the line out, and a legal one.
	p.MaintainedGroups[0].Levels = []MaintainLevel{{BinType: "STANDARD", Want: 0}}
	if err := p.Validate(); err != nil {
		t.Fatalf("want=0 must be a legal declared level: %v", err)
	}
}

// The shipped demo stages a maintained group, because phase 2's soak needs the
// shape and a seed nobody exercises is a seed that rots.
func TestShippedDemoPlantHasMaintainedGroup(t *testing.T) {
	p, err := Load("../../plants/demo.yaml")
	if err != nil {
		t.Fatalf("load plants/demo.yaml: %v", err)
	}
	if len(p.MaintainedGroups) == 0 {
		t.Fatal("plants/demo.yaml declares no maintained group; phase 2's soak has no shape to run against")
	}
	mg := p.MaintainedGroups[0]
	var zone *Zone
	for i := range p.Zones {
		if p.Zones[i].Name == mg.Group {
			zone = &p.Zones[i]
		}
	}
	if zone == nil {
		t.Fatalf("maintained group %q names no declared zone", mg.Group)
	}
	if len(zone.Lanes) != 0 {
		t.Errorf("maintained zone %q has lanes; a maintained group is flat", zone.Name)
	}
	// A mixed level is the real shape ("four of one, two of another"), and a
	// single-type level would not exercise the type-keyed half of anything.
	if len(mg.Levels) < 2 {
		t.Errorf("maintained group %q declares %d level line(s); the soak needs a mixed level",
			mg.Group, len(mg.Levels))
	}
	// Room for a carrier coming back in.
	total := 0
	for _, l := range mg.Levels {
		total += l.Want
	}
	if total >= len(zone.Positions) {
		t.Errorf("maintained group %q declares %d carriers across %d positions, leaving nothing free for a return",
			mg.Group, total, len(zone.Positions))
	}
}
