package plantspec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shingo/protocol"
)

// validPlant returns a minimal spec that passes Validate; tests mutate a copy
// to exercise each failure mode.
func validPlant() *Plant {
	return &Plant{
		Namespace: "edge1",
		LineID:    "line1",
		BinTypes:  []string{"STANDARD"},
		Payloads: []Payload{
			{Code: "PART-A", UOPCapacity: 1000, BinType: "STANDARD"},
		},
		Zones: []Zone{{
			Name: "SM-A", RetrieveAlgorithm: "FIFO", StoreAlgorithm: "DPTH",
			WaitPoints: []string{"SM-A-WAIT"},
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
		// buildSwapDispatch refuses a two_robot claim with no evac dropoff
		// after the supply leg has shipped; the spec must refuse it first.
		{"two_robot missing dest", func(p *Plant) {
			p.Claims[1].SwapMode = "two_robot"
			p.Claims[1].OutboundStaging = ""
		}, "two_robot requires outbound_destination"},
		{"simple retired", func(p *Plant) { p.Claims[2].SwapMode = "simple" }, "retired"},
		{"no storage hierarchy", func(p *Plant) { p.Zones = nil }, "storage hierarchy missing"},
		{"payload unknown bin type", func(p *Plant) { p.Payloads[0].BinType = "BOGUS" }, "unknown bin_type"},
		{"bin at unknown node", func(p *Plant) { p.Bins[0].Slot = "GHOST" }, "unknown node"},
		{"demand unknown payload", func(p *Plant) { p.Demands[0].Payload = "PART-Z" }, "unknown payload"},
		{"reporting point unknown node", func(p *Plant) { p.ReportingPoints[0].Node = "GHOST" }, "unknown node"},
		{"style unknown process", func(p *Plant) { p.Styles[0].Process = "NOPE" }, "unknown process"},
		{"slot zero depth", func(p *Plant) { p.Zones[0].Lanes[0].Slots[0].Depth = 0 }, "non-positive depth"},
		// The twin of the Edge's dedicated-spot check, for the seeder's raw INSERT.
		{"keep_staged_node named by another claim", func(p *Plant) {
			p.Claims[1].KeepStagedNode = "STAGE-P1-IN"
			p.Claims[2].InboundSource = "STAGE-P1-IN"
		}, "STAGE-P1-IN is claim[1] PRESS-1/STYLE-A's kept spot"},
		{"keep_staged_node that is the claim's own outbound staging", func(p *Plant) {
			p.Claims[1].KeepStagedNode = p.Claims[1].OutboundStaging
		}, "also names it as outbound_staging"},
		// The Edge's save rules for who refills and for a tool change's marks,
		// over a mode the old press-index-only arm never reached.
		{"refill_by Robot 2 off a press-index cell", func(p *Plant) {
			p.Claims[1].RefillBy = protocol.RefillByIndexRobot
		}, "refill_by \"index_robot\" applies to two_robot_press_index only"},
		{"refill_by that names no robot", func(p *Plant) {
			p.Claims[1].RefillBy = "robot_9"
		}, "refill_by \"robot_9\" is not a robot"},
		{"marks with no inbound staging", func(p *Plant) {
			p.Claims[2].ChangeoverEvacNodes = []string{"LINE1-IN"}
		}, "changeover_evac_nodes needs inbound_staging"},
		{"marks on a loader", func(p *Plant) {
			p.Claims[0].ChangeoverEvacNodes = []string{"LOADER-1"}
		}, "a manual_swap loader has no positions to clear"},
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

// A two_robot claim with inbound staging and an evac dropoff is complete: the
// refusal above is for the missing destination, not the mode.
func TestValidate_TwoRobotWithDestinationPasses(t *testing.T) {
	p := validPlant()
	p.Claims[1].SwapMode = "two_robot"
	p.Claims[1].OutboundStaging = ""
	p.Claims[1].OutboundDestination = "SM-A"
	if err := p.Validate(); err != nil {
		t.Fatalf("a complete two_robot claim was refused: %v", err)
	}
}

// A tool change may mark the positions of any line mode a robot serves, given
// inbound staging to hold the new carrier: here a single-robot press and a
// sequential line.
func TestValidate_MarksOnAnyLineModePass(t *testing.T) {
	p := validPlant()
	p.Claims[1].ChangeoverEvacNodes = []string{"PRESS-1"}
	p.Claims[2].ChangeoverEvacNodes = []string{"LINE1-IN"}
	p.Claims[2].InboundStaging = "STAGE-P1-IN"
	if err := p.Validate(); err != nil {
		t.Fatalf("marks on a single-robot press and a sequential line were refused: %v", err)
	}
}

// A kept spare on its own staging, named by nothing else, is a valid spec: the
// claim's own inbound_staging may be its spot. So is a spot on a sequential
// claim: every swap mode may keep one.
func TestValidate_KeepStagedAlonePasses(t *testing.T) {
	p := validPlant()
	p.Claims[1].KeepStagedNode = p.Claims[1].InboundStaging
	if err := p.Validate(); err != nil {
		t.Fatalf("a lone keep-staged claim was refused: %v", err)
	}
	p = validPlant()
	p.Claims[2].KeepStagedNode = "STAGE-SEQ-SPOT"
	if err := p.Validate(); err != nil {
		t.Fatalf("a keep-staged sequential claim was refused: %v", err)
	}
}

func TestLoad_RoundTrip(t *testing.T) {
	yaml := `
namespace: edge1
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
	path := filepath.Join(dir, "plant.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Namespace != "edge1" || p.LineID != "line1" {
		t.Fatalf("header not parsed: %+v", p)
	}
	if len(p.Zones) != 1 || len(p.Zones[0].Lanes) != 1 || len(p.Zones[0].Lanes[0].Slots) != 1 {
		t.Fatalf("zone hierarchy not parsed: %+v", p.Zones)
	}
	if p.Zones[0].Lanes[0].Slots[0].Depth != 1 {
		t.Fatalf("slot depth not parsed: %+v", p.Zones[0].Lanes[0].Slots[0])
	}
}

// kanbanPlant is validPlant plus a flat zone and a kanban on it —
// the smallest spec that declares a level. Tests mutate a copy to exercise each
// refusal.
//
// Every case here MIRRORS a save-time refusal in the settings modal, and that is
// the point of testing them together: a spec that can declare a configuration the
// UI refuses would seed a plant nobody could then edit, and the first save of an
// untouched screen would come back with a reason the operator would be right to
// read as a bug.
func kanbanPlant() *Plant {
	p := validPlant()
	p.BinTypes = append(p.BinTypes, "SMALL")
	p.Zones = append(p.Zones, Zone{
		Name: "PRESS-EMPTIES", RetrieveAlgorithm: "FIFO", StoreAlgorithm: "LKND",
		Positions: []Slot{
			{Name: "PEB-01", Depth: 1}, {Name: "PEB-02", Depth: 1},
			{Name: "PEB-03", Depth: 1}, {Name: "PEB-04", Depth: 1},
		},
	})
	p.Kanbans = []Kanban{{
		Group:   "PRESS-EMPTIES",
		Station: "edge1.line1",
		Levels: []KanbanLevel{
			{BinType: "STANDARD", Want: 2},
			{BinType: "SMALL", Want: 1},
		},
		Supports: []string{"PRESS-LINE"},
	}}
	return p
}

func TestValidate_KanbanPasses(t *testing.T) {
	if err := kanbanPlant().Validate(); err != nil {
		t.Fatalf("a valid kanban should pass, got: %v", err)
	}
}

// A kanban with no supports declares what it holds and is refilled by others, so
// it needs no station; sources, cover and per-type overflow are all spec fields.
func TestValidate_AKanbanWithNoSupportsNeedsNoStation(t *testing.T) {
	p := kanbanPlant()
	p.Kanbans[0].Station = ""
	p.Kanbans[0].Supports = nil
	if err := p.Validate(); err != nil {
		t.Fatalf("a kanban with no supports and no station should pass, got: %v", err)
	}
}

func TestValidate_KanbanRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Plant)
		want   string
	}{
		{"unknown zone", func(p *Plant) { p.Kanbans[0].Group = "NOPE" }, "unknown zone"},
		{"names a station", func(p *Plant) { p.Kanbans[0].Group = "PRESS-1" }, "a kanban is a zone"},
		// Flat, because the save-time rule is flat: a lane means a carrier can be
		// buried, and a level counted over buried carriers is a number whose
		// meaning changes with what is parked in front of it.
		{"not flat", func(p *Plant) {
			p.Zones[1].Lanes = []Lane{{Name: "PEB-LANE", Slots: []Slot{{Name: "PEB-L1", Depth: 1}}}}
		}, "a kanban is flat"},
		{"no positions", func(p *Plant) { p.Zones[1].Positions = nil }, "no positions to hold a level"},
		// projectOrder no-ops on a blank StationID.
		{"supports and no station", func(p *Plant) { p.Kanbans[0].Station = "" }, "show on no board"},
		{"negative pool", func(p *Plant) { p.Kanbans[0].Pools = map[string]int{p.BinTypes[0]: -1} }, "cannot be negative"},
		{"a source that is itself", func(p *Plant) { p.Kanbans[0].Sources = []string{"PRESS-EMPTIES"} }, "itself as a source"},
		{"a source that is no zone", func(p *Plant) { p.Kanbans[0].Sources = []string{"NOPE"} }, "not a declared zone"},
		{"a source twice", func(p *Plant) { p.Kanbans[0].Sources = []string{"SYN_MARKET", "SYN_MARKET"} }, "twice"},
		{"no levels", func(p *Plant) { p.Kanbans[0].Levels = nil }, "declares no levels"},
		{"unknown bin type", func(p *Plant) { p.Kanbans[0].Levels[0].BinType = "BOGUS" }, "unknown bin_type"},
		{"duplicate bin type", func(p *Plant) { p.Kanbans[0].Levels[1].BinType = "STANDARD" }, "twice"},
		{"negative want", func(p *Plant) { p.Kanbans[0].Levels[0].Want = -1 }, "cannot be negative"},
		{"unknown process", func(p *Plant) { p.Kanbans[0].Supports = []string{"GHOST"} }, "unknown process"},
		{"duplicate group", func(p *Plant) {
			p.Kanbans = append(p.Kanbans, p.Kanbans[0])
		}, "duplicate kanban"},
		// A position hangs directly off the group, so nothing can be buried behind
		// it and its depth is 1.
		{"position not depth 1", func(p *Plant) { p.Zones[1].Positions[0].Depth = 2 }, "its depth is 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := kanbanPlant()
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
func TestValidate_KanbanDoesNotRefuseWarnings(t *testing.T) {
	p := kanbanPlant()
	// Level fills every position — the UI warns, nothing refuses.
	p.Kanbans[0].Levels = []KanbanLevel{{BinType: "STANDARD", Want: 4}}
	if err := p.Validate(); err != nil {
		t.Fatalf("a full-house level must not be a spec error: %v", err)
	}
	// want=0 declares the type and asks for none of it — a different statement
	// from leaving the line out, and a legal one.
	p.Kanbans[0].Levels = []KanbanLevel{{BinType: "STANDARD", Want: 0}}
	if err := p.Validate(); err != nil {
		t.Fatalf("want=0 must be a legal declared level: %v", err)
	}
}

// The shipped demo stages a kanban, because phase 2's soak needs the
// shape and a seed nobody exercises is a seed that rots.
func TestShippedDemoPlantHasKanban(t *testing.T) {
	p, err := Load("../../plants/master.yaml")
	if err != nil {
		t.Fatalf("load plants/master.yaml: %v", err)
	}
	if len(p.Kanbans) == 0 {
		t.Fatal("plants/master.yaml declares no kanban; phase 2's soak has no shape to run against")
	}
	mg := p.Kanbans[0]
	var zone *Zone
	for i := range p.Zones {
		if p.Zones[i].Name == mg.Group {
			zone = &p.Zones[i]
		}
	}
	if zone == nil {
		t.Fatalf("kanban %q names no declared zone", mg.Group)
	}
	if len(zone.Lanes) != 0 {
		t.Errorf("kanban zone %q has lanes; a kanban is flat", zone.Name)
	}
	// A mixed level is the real shape ("four of one, two of another"), and a
	// single-type level would not exercise the type-keyed half of anything.
	if len(mg.Levels) < 2 {
		t.Errorf("kanban %q declares %d level line(s); the soak needs a mixed level",
			mg.Group, len(mg.Levels))
	}
	// Room for a carrier coming back in.
	total := 0
	for _, l := range mg.Levels {
		total += l.Want
	}
	if total >= len(zone.Positions) {
		t.Errorf("kanban %q declares %d carriers across %d positions, leaving nothing free for a return",
			mg.Group, total, len(zone.Positions))
	}
}

// A source is a group or a loader position, never a lane, and a consumer's
// fulls never come from a kanban — the same two refusals the loader save
// check makes (service.ErrSourceIsALane / ErrFullsFromAKanban), so a spec
// cannot seed a claim the settings screen would refuse.
func TestValidate_SourceKindRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Plant)
		want   string
	}{
		{"consumer names a lane", func(p *Plant) { p.Claims[2].InboundSource = "SM-A-LANE-1" }, "a lane is not a source"},
		{"outbound names a lane", func(p *Plant) { p.Claims[0].OutboundDestination = "SM-A-LANE-1" }, "a lane is not a source"},
		{"consumer's fulls from a kanban", func(p *Plant) { p.Claims[2].InboundSource = "PRESS-EMPTIES" }, "a consumer's fulls cannot come from it"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := kanbanPlant()
			tc.mutate(p)
			err := p.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
	// A press drawing its EMPTIES from the kanban is what the kanban is for.
	p := kanbanPlant()
	p.Claims[1].InboundSource = "PRESS-EMPTIES"
	if err := p.Validate(); err != nil && strings.Contains(err.Error(), "PRESS-EMPTIES") {
		t.Errorf("a producer drawing empties from a kanban was refused: %v", err)
	}
}

// AN UNKNOWN KEY REFUSES THE PLANT, NAMED. A renamed field's old key used to
// load as if the field were unset (the 2026-10-03 sim plant's keep_staged).
func TestLoad_UnknownKeyRefusesNamed(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "plant.yaml")
	if err := os.WriteFile(path, []byte("namespace: x\nzones:\n  - name: Z\n    no_such_key: on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "no_such_key") {
		t.Fatalf("Load = %v, want a refusal naming no_such_key", err)
	}
}

// A lane is never without a mark: a zone with lanes and no wait point is
// refused at validation, as the store refuses its first lane at seed. A blank
// point is no point; a zone of positions only needs none.
func TestValidate_ALaneZoneNeedsAWaitPoint(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		points []string
		lanes  bool
		refuse bool
	}{
		{"marked", []string{"SM-A-WAIT"}, true, false},
		{"none", nil, true, true},
		{"blank", []string{" "}, true, true},
		{"positions only", nil, false, false},
	} {
		p := validPlant()
		p.Zones[0].WaitPoints = c.points
		if !c.lanes {
			p.Zones = append(p.Zones, Zone{Name: "SM-POS", RetrieveAlgorithm: "FIFO", StoreAlgorithm: "DPTH",
				Positions: []Slot{{Name: "SM-POS-01", Depth: 1}}})
			p.Zones[0].WaitPoints = []string{"SM-A-WAIT"}
		}
		err := p.Validate()
		refused := err != nil && strings.Contains(err.Error(), "no wait_points")
		if refused != c.refuse {
			t.Errorf("%s: refused = %v, want %v (%v)", c.name, refused, c.refuse, err)
		}
	}
}
