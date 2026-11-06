package plantspec

import (
	"strings"
	"testing"
)

// edges_test.go — one plant file over a Core with two Edges: Core's half is
// whole on every Edge's view, and each Edge's half is its own processes'.

func twoEdgePlant() *Plant {
	return &Plant{
		Namespace: "edge1", LineID: "line1",
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
	if two.Namespace != "edge2" || two.LineID != "line1" {
		t.Errorf("edge2's view is %s.%s", two.Namespace, two.LineID)
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

func TestValidate_AnEdgeIsAStationUID(t *testing.T) {
	t.Parallel()
	p := twoEdgePlant()
	p.Processes[1].Edge = "edge2"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), `edge "edge2" is not a station uid`) {
		t.Errorf("Validate = %v, want the edge refused by name", err)
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
