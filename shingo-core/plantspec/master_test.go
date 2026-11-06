package plantspec

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// master_test.go — plants/master.yaml, the one plant. SHAPE, not tuning: what
// each section has to keep for the families that run on it.

const masterPath = "../../plants/master.yaml"

func loadMaster(t *testing.T) *Plant {
	t.Helper()
	p, err := Load(masterPath)
	if err != nil {
		t.Fatalf("load plants/master.yaml: %v", err)
	}
	return p
}

func TestMasterPlant_ValidAndCleanAtBirth(t *testing.T) {
	t.Parallel()
	p := loadMaster(t)
	if err := p.Validate(); err != nil {
		t.Fatalf("plants/master.yaml invalid: %v", err)
	}
	if p.BaselineFrozenAt != "" {
		t.Error("the master declares itself a frozen baseline; it has nothing to excuse")
	}
	if c := p.CensusAtBirth(); !c.Clean() {
		t.Errorf("census: %v", c.Findings())
	}
	if c := p.CarrierPairsAtBirth(); !c.Clean() {
		t.Errorf("carrier pairs: %v", c.Findings())
	}
}

// A reader finds a section by its tag: every section's banner is in the file.
func TestMasterPlant_EverySectionIsMarked(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(masterPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"[demo]", "[multi-cart]", "[4x2]", "[corridor]", "[kanban]", "[kanban5]", "[window]"} {
		if !strings.Contains(string(raw), "# ══ "+tag) && !strings.Contains(string(raw), "# "+tag) {
			t.Errorf("no section marked %s", tag)
		}
	}
}

// sectionTypes is the carrier types each section's processes run, by the
// processes' claims under every style.
func sectionTypes(p *Plant, procs ...string) map[string]bool {
	in := map[string]bool{}
	for _, pr := range procs {
		in[pr] = true
	}
	styleProc := map[string]string{}
	for _, s := range p.Styles {
		styleProc[s.Name] = s.Process
	}
	typeOf := map[string]string{}
	for _, pl := range p.Payloads {
		typeOf[pl.Code] = pl.BinType
	}
	out := map[string]bool{}
	for _, c := range p.Claims {
		if in[styleProc[c.Style]] {
			for _, code := range c.FlowPayloads() {
				if bt := typeOf[code]; bt != "" {
					out[bt] = true
				}
			}
		}
	}
	return out
}

// Each section is its own loop on its own carriers: a keeper or a loader in one
// never draws another's empties. [kanban] and [kanban5] are one family's loops and
// share KP-SM, so they are one section here. Every process is in a section.
func TestMasterPlant_EverySectionRidesItsOwnCarriers(t *testing.T) {
	t.Parallel()
	p := loadMaster(t)
	sections := map[string][]string{
		"demo":   {"PRESS-1", "PRESS-2", "WELD-1", "WELD-2", "WELD-3", "WELD-4", "WELD-5", "LOADER-SHIM", "LOADER-COMP", "LOADER-CLIP", "LOADER-STUD", "UNLOADER-A"},
		"4x2":    {"WELD-6", "LOADER-COMMON-1", "LOADER-COMMON-2", "LOADER-COMMON-3", "LOADER-MIRROR-L", "LOADER-MIRROR-R"},
		"kanban": {"KP-PRESS", "KW-WELD", "KP-UNLOAD", "KW-UNLOAD", "P5-PRESS-1", "P5-PRESS-2", "P5-PRESS-3", "P5-PRESS-4", "P5-PRESS-5", "P5-UNLOAD-CELL"},
		"window": {"LINE-2", "SMN-10-W", "SMN-10-X", "DRAIN-10"},
	}
	placed := map[string]bool{}
	owner := map[string]string{}
	for name, procs := range sections {
		for _, pr := range procs {
			placed[pr] = true
		}
		types := sectionTypes(p, procs...)
		if len(types) == 0 {
			t.Errorf("section %s runs no carrier type: its processes are not in the file", name)
		}
		for bt := range types {
			if prev, ok := owner[bt]; ok {
				t.Errorf("carrier %s is %s's and %s's: one Core, one pool", bt, prev, name)
			}
			owner[bt] = name
		}
	}
	for _, pr := range p.Processes {
		if !placed[pr.Name] {
			t.Errorf("process %s is in no section", pr.Name)
		}
	}
}

// THE 4x2-SHAPED CELL: five consume positions under each of two styles, the
// styles differing at exactly the two mirror positions, three common parts shared.
func TestMaster4x2_FivePositionsTwoStylesDifferAtTheMirrors(t *testing.T) {
	t.Parallel()
	p := loadMaster(t)
	at := map[string]map[string]string{} // style → node → part
	for _, c := range p.Claims {
		if c.Style == "WELD-6-RUN" || c.Style == "WELD-6-ALT" {
			if c.Role != "consume" || c.SwapMode != "two_robot" {
				t.Errorf("%s/%s is %s %s, want a two_robot consume position", c.CoreNode, c.Style, c.Role, c.SwapMode)
			}
			if at[c.Style] == nil {
				at[c.Style] = map[string]string{}
			}
			at[c.Style][c.CoreNode] = c.Payload
		}
	}
	run, alt := at["WELD-6-RUN"], at["WELD-6-ALT"]
	if len(run) != 5 || len(alt) != 5 {
		t.Fatalf("WELD-6 positions: RUN %d, ALT %d, want five each", len(run), len(alt))
	}
	var differ []string
	for node, part := range run {
		if alt[node] != part {
			differ = append(differ, node)
			if part != "MIRROR-L" || alt[node] != "MIRROR-R" {
				t.Errorf("%s runs %s under RUN and %s under ALT, want MIRROR-L / MIRROR-R", node, part, alt[node])
			}
		} else if !strings.HasPrefix(part, "COMMON-") {
			t.Errorf("%s runs %s under both styles, want a common part", node, part)
		}
	}
	if len(differ) != 2 {
		t.Errorf("the styles differ at %v, want exactly the two mirror positions", differ)
	}
	commons := map[string]bool{}
	for _, part := range run {
		if strings.HasPrefix(part, "COMMON-") {
			commons[part] = true
		}
	}
	if len(commons) != 3 {
		t.Errorf("RUN and ALT share %d common parts, want COMMON-1..3", len(commons))
	}
}

// Its counting: a pin per style, the production counter in an MES-style struct
// whose CATID slot the Edge derives, and a station counter on the load station
// that counts all five positions and is not a member of that struct.
func TestMaster4x2_PinsTheIdentityAndCountsAtTheLoadStation(t *testing.T) {
	t.Parallel()
	p := loadMaster(t)
	pins := map[string]string{}
	for _, s := range p.Styles {
		if s.Process == "WELD-6" {
			pins[s.Name] = s.ExpectedCATID
		}
	}
	if pins["WELD-6-RUN"] != "1001" || pins["WELD-6-ALT"] != "1002" {
		t.Errorf("WELD-6 pins = %v, want RUN 1001 and ALT 1002", pins)
	}
	var counter string
	for _, rp := range p.ReportingPoints {
		if rp.Style == "WELD-6-RUN" {
			counter = rp.TagName
			if rp.PLCName != "WELD-6" {
				t.Errorf("WELD-6's counter is on PLC %s, want WELD-6 (simcalc keys rates on the process)", rp.PLCName)
			}
		}
	}
	dot := strings.LastIndex(counter, ".")
	if dot < 0 || counter[dot+1:] != "Prod_Counter_01" {
		t.Fatalf("WELD-6's counter tag = %q, want <struct>.Prod_Counter_01 so its CATID_01 derives beside it", counter)
	}
	structName := counter[:dot]
	var station *StationCounter
	for i := range p.StationCounters {
		if p.StationCounters[i].Process == "WELD-6" {
			station = &p.StationCounters[i]
		}
	}
	if station == nil {
		t.Fatal("WELD-6 has no station counter")
	}
	if strings.HasPrefix(station.TagName, structName+".") {
		t.Errorf("the station counter %s is a member of the MES struct %s", station.TagName, structName)
	}
	want := []string{"ALN_061", "ALN_062", "ALN_063", "ALN_064", "ALN_065"}
	got := slices.Clone(station.Nodes)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the station counter counts %v, want all five positions %v", station.Nodes, want)
	}
}

// Fed by loaders, BRKT-style: every part the cell runs has a loader and no press
// makes it, so the demo's press and weld balance does not move.
func TestMaster4x2_EveryPartComesFromALoader(t *testing.T) {
	t.Parallel()
	p := loadMaster(t)
	for _, part := range []string{"COMMON-1", "COMMON-2", "COMMON-3", "MIRROR-L", "MIRROR-R"} {
		loaders, presses := 0, 0
		for _, c := range p.Claims {
			if c.Payload != part || c.Role != "produce" {
				continue
			}
			if c.IsLoader() {
				loaders++
			} else {
				presses++
			}
		}
		if loaders != 1 || presses != 0 {
			t.Errorf("%s: %d loader(s) and %d tick producer(s), want one loader and nothing else", part, loaders, presses)
		}
	}
}

// THE CORRIDOR: lane-stress-packed's storage, gated and packed, two lanes' worth
// of headroom per group, the two four-blocker digs seeded where they were.
func TestMasterCorridor_GatedPackedAndBuriedWhereTheDigsAre(t *testing.T) {
	t.Parallel()
	p := loadMaster(t)
	if p.Headroom.LanesFree() != 2 {
		t.Errorf("headroom = %d lanes, want 2: two digs at once in one group", p.Headroom.LanesFree())
	}
	zones := map[string]Zone{}
	for _, z := range p.Zones {
		zones[z.Name] = z
	}
	for name, lanes := range map[string]int{"SYN_STAMP": 12, "SYN_COMP": 12} {
		z, ok := zones[name]
		if !ok {
			t.Fatalf("no zone %s", name)
		}
		if len(z.WaitPoints) == 0 {
			t.Errorf("%s has no wait points; its lanes are not gated", name)
		}
		if len(z.Lanes) != lanes {
			t.Errorf("%s has %d lanes, want %d", name, len(z.Lanes), lanes)
		}
	}
	oldest := map[string]string{} // slot → bin, for the aged bins
	for _, b := range p.Bins {
		if b.AgeS > 0 {
			oldest[b.Slot] = b.Payload
		}
	}
	for slot, part := range map[string]string{"LSD_005": "COMMON-1", "LSD_015": "COMMON-1", "LSC_004": "COMMON-3"} {
		if oldest[slot] != part {
			t.Errorf("the aged bin at %s is %q, want %s at the back of its lane", slot, oldest[slot], part)
		}
	}
}

// THE OBSERVABLE-WINDOW LINE on EDGE 2, and everything else on the plant's own Edge.
func TestMasterWindow_LineTwoRunsOnTheSecondEdge(t *testing.T) {
	t.Parallel()
	p := loadMaster(t)
	if got := strings.Join(p.Edges(), ","); got != "edge1.line1,edge2.line1" {
		t.Fatalf("edges = %s, want edge1.line1,edge2.line1", got)
	}
	two := p.ForEdge("edge2.line1")
	var names []string
	for _, pr := range two.Processes {
		names = append(names, pr.Name)
	}
	slices.Sort(names)
	if got := strings.Join(names, ","); got != "DRAIN-10,LINE-2,SMN-10-W,SMN-10-X" {
		t.Errorf("EDGE 2 runs %s, want line 2's four processes", got)
	}
	for _, k := range p.Kanbans {
		if k.Station != p.Station() {
			t.Errorf("kanban %s is projected to %s, want %s", k.Group, k.Station, p.Station())
		}
	}
}
