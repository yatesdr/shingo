package plantspec

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// edges_test.go — one plant file over a Core with two Edges: Core's half is
// whole on every Edge's view, and each Edge's half is its own processes'.

func twoEdgePlant() *Plant {
	return &Plant{
		StationUID: "edge1.line1",
		Processes: []Process{
			{Name: "P1", ActiveStyle: "P1-RUN"},
			{Name: "P2", ActiveStyle: "P2-RUN", Edge: "edge2.line1"},
		},
		Styles: []Style{
			{Name: "P1-RUN", Process: "P1", Payload: "A"},
			{Name: "P2-RUN", Process: "P2", Payload: "B"},
		},
		Claims: []Claim{
			{CoreNode: "N1", Style: "P1-RUN"},
			{CoreNode: "N2", Style: "P2-RUN", OperatorStation: "N2-OPS"},
		},
		ReportingPoints: []ReportingPoint{
			{PLCName: "P1", TagName: "P1_C", Node: "N1", Style: "P1-RUN"},
			{PLCName: "P2", TagName: "P2_C", Node: "N2", Style: "P2-RUN"},
			{PLCName: "X", TagName: "X_C", Node: "N1"},
		},
		StationCounters:  []StationCounter{{Process: "P2", TagName: "S2", Nodes: []string{"N2"}}},
		CellConfigs:      []CellConfig{{Process: "P1", Station: "P1-OPS"}, {Process: "P2", Station: "P2-OPS"}},
		OperatorStations: []string{"P1-OPS", "P2-OPS", "N2-OPS"},
		Zones:            []Zone{{Name: "Z"}},
		Bins:             []Bin{{Name: "B1", Slot: "N1"}, {Name: "B2", Slot: "N2"}},
	}
}

func TestEdges_ThePlantsOwnFirstThenEachNamed(t *testing.T) {
	t.Parallel()
	p := twoEdgePlant()
	if got := strings.Join(p.Edges(), ","); got != "edge1.line1,edge2.line1" {
		t.Errorf("edges = %s, want edge1.line1,edge2.line1", got)
	}
	if p.EdgeOf("P1") != "edge1.line1" || p.EdgeOf("P2") != "edge2.line1" || p.EdgeOf("NONE") != "" {
		t.Errorf("EdgeOf = %q %q %q", p.EdgeOf("P1"), p.EdgeOf("P2"), p.EdgeOf("NONE"))
	}
}

// Each Edge's view carries its own processes and everything hanging off them,
// and Core's half whole: a bin at the other Edge's node is still a bin of the plant.
func TestForEdge_SplitsTheEdgesHalfAndKeepsCoresWhole(t *testing.T) {
	t.Parallel()
	p := twoEdgePlant()
	two := p.ForEdge("edge2.line1")
	if two.StationUID != "edge2.line1" || two.Station() != "edge2.line1" {
		t.Errorf("edge2's view is %s", two.StationUID)
	}
	if len(two.Processes) != 1 || two.Processes[0].Name != "P2" || len(two.Styles) != 1 || len(two.Claims) != 1 ||
		len(two.ReportingPoints) != 1 || len(two.StationCounters) != 1 || len(two.CellConfigs) != 1 {
		t.Errorf("edge2's view = %+v", two)
	}
	if got := strings.Join(two.OperatorStations, ","); got != "N2-OPS,P2-OPS" && got != "P2-OPS,N2-OPS" {
		t.Errorf("edge2's operator stations = %s, want P2-OPS and N2-OPS", got)
	}
	if len(two.Bins) != 2 || len(two.Zones) != 1 {
		t.Errorf("edge2's view dropped Core's half: %d bins, %d zones", len(two.Bins), len(two.Zones))
	}
	one := p.ForEdge("edge1.line1")
	if len(one.Processes) != 1 || one.Processes[0].Name != "P1" || len(one.ReportingPoints) != 2 || len(one.StationCounters) != 0 {
		t.Errorf("edge1's view = %+v (the styleless point stays with the plant's own Edge)", one)
	}
	if len(p.Processes) != 2 || len(p.ReportingPoints) != 3 {
		t.Error("ForEdge changed the plant it was asked about")
	}
}

// A station uid is opaque: a process's edge may be a minted stn-… uid, which
// has no dot, as well as a dev one; the plant's own must be set.
func TestValidate_AnEdgeIsAStationUID(t *testing.T) {
	t.Parallel()
	p := twoEdgePlant()
	p.Processes[1].Edge = "stn-0123456789abcdef"
	if err := p.Validate(); err != nil && strings.Contains(err.Error(), "stn-0123456789abcdef") {
		t.Errorf("Validate = %v, want a minted uid accepted as a process's edge", err)
	}
	if got := p.ForEdge("stn-0123456789abcdef"); got.Station() != "stn-0123456789abcdef" || len(got.Processes) != 1 {
		t.Errorf("the minted uid's view is %s with %d processes, want its own and P2", got.Station(), len(got.Processes))
	}
	p.StationUID = ""
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "station_uid is required") {
		t.Errorf("Validate = %v, want a plant with no station_uid refused", err)
	}
}

func TestValidate_AnExpectedCATIDIsIntegers(t *testing.T) {
	t.Parallel()
	p := twoEdgePlant()
	p.Styles[0].ExpectedCATID = "1001, 1003"
	if err := p.Validate(); err != nil && strings.Contains(err.Error(), "expected_catid") {
		t.Errorf("a list of integers was refused: %v", err)
	}
	p.Styles[0].ExpectedCATID = "RUN"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), `expected_catid "RUN"`) {
		t.Errorf("Validate = %v, want a pin that is no integer refused", err)
	}
}

var updateEdgesGolden = flag.Bool("update-edges-golden", false,
	"rewrite testdata/edges-*.golden from the committed plant files as they split now")

// TestEdges_TheCommittedSpecsSplitAsTheyDo pins how the two committed plant
// files split into Edges: the station list (Edges), each Edge's view
// (ForEdge): the station it names, every process, style, claim, reporting
// point, station counter, cell config and operator station it keeps, and the
// size of Core's half it carries whole. The goldens are today's split.
//
// The station uid is one key in the file (station_uid); how it is spelled
// does not move the split: a diff here is an Edge seeded with something other
// than what it was.
func TestEdges_TheCommittedSpecsSplitAsTheyDo(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ golden, spec string }{
		{"edges-master.golden", masterPath},
		{"edges-seed-fixture.golden", "../cmd/seeddev/testdata/seed-fixture.yaml"},
	} {
		t.Run(c.golden, func(t *testing.T) {
			t.Parallel()
			got := renderEdgeSplit(t, c.spec)
			if again := renderEdgeSplit(t, c.spec); again != got {
				t.Fatalf("two loads of %s split differently: the rendering is not deterministic", c.spec)
			}
			path := filepath.Join("testdata", c.golden)
			if *updateEdgesGolden {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatalf("write %s: %v", path, err)
				}
				return
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			want := string(raw)
			if got == want {
				return
			}
			gl, wl := strings.Split(got, "\n"), strings.Split(want, "\n")
			for i := 0; i < max(len(gl), len(wl)); i++ {
				g, w := "<end>", "<end>"
				if i < len(gl) {
					g = gl[i]
				}
				if i < len(wl) {
					w = wl[i]
				}
				if g != w {
					t.Fatalf("%s splits differently from %s at line %d:\n  got  %s\n  want %s\n"+
						"A change to how a station is named must not move a process, claim or point between Edges. "+
						"If the plant file itself changed on purpose, rerun with -update-edges-golden and review the diff.",
						c.spec, path, i+1, g, w)
				}
			}
		})
	}
}

// renderEdgeSplit loads spec and writes its split one item per line, in the
// plant's own order, so a diff names the item that moved.
func renderEdgeSplit(t *testing.T, spec string) string {
	t.Helper()
	p, err := Load(spec)
	if err != nil {
		t.Fatalf("load %s: %v", spec, err)
	}
	var b strings.Builder
	edges := p.Edges()
	if len(edges) == 0 {
		t.Fatalf("%s names no Edge", spec)
	}
	fmt.Fprintf(&b, "edges %s\n", strings.Join(edges, " "))
	fmt.Fprintf(&b, "plant station %s\n", p.Station())
	for _, station := range edges {
		v := p.ForEdge(station)
		fmt.Fprintf(&b, "\nedge %s\n", station)
		fmt.Fprintf(&b, "  view station %s\n", v.Station())
		for _, pr := range v.Processes {
			fmt.Fprintf(&b, "  process %s on %s\n", pr.Name, p.EdgeOf(pr.Name))
		}
		for _, s := range v.Styles {
			fmt.Fprintf(&b, "  style %s of %s\n", s.Name, s.Process)
		}
		for _, c := range v.Claims {
			fmt.Fprintf(&b, "  claim %s %s operator=%s\n", c.CoreNode, c.Style, c.OperatorStation)
		}
		for _, rp := range v.ReportingPoints {
			fmt.Fprintf(&b, "  point %s %s %s style=%s\n", rp.PLCName, rp.TagName, rp.Node, rp.Style)
		}
		for _, sc := range v.StationCounters {
			fmt.Fprintf(&b, "  counter %s %s %s\n", sc.Process, sc.TagName, strings.Join(sc.Nodes, ","))
		}
		for _, cc := range v.CellConfigs {
			fmt.Fprintf(&b, "  cell %s %s\n", cc.Process, cc.Station)
		}
		for _, s := range v.OperatorStations {
			fmt.Fprintf(&b, "  operator %s\n", s)
		}
		fmt.Fprintf(&b, "  core bin_types=%d payloads=%d zones=%d stations=%d bins=%d demands=%d kanbans=%d loader_settings=%d\n",
			len(v.BinTypes), len(v.Payloads), len(v.Zones), len(v.Stations), len(v.Bins), len(v.Demands),
			len(v.Kanbans), len(v.LoaderSettings))
	}
	return b.String()
}
