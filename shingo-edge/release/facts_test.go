package release

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
)

func wait(node, kind string) protocol.ComplexOrderStep {
	return protocol.ComplexOrderStep{Action: protocol.ActionWait, Node: node, WaitKind: kind}
}
func pick(node string) protocol.ComplexOrderStep {
	return protocol.ComplexOrderStep{Action: protocol.ActionPickup, Node: node}
}
func drop(node string) protocol.ComplexOrderStep {
	return protocol.ComplexOrderStep{Action: protocol.ActionDropoff, Node: node}
}

// renderFacts prints a leg's facts in a fixed order.
func renderFacts(f Facts) string {
	departs := []string{}
	for _, n := range []string{"PRESS", "B", "C", "STAGE", "IN", "OUT"} {
		if i, ok := f.Departs[n]; ok {
			departs = append(departs, fmt.Sprintf("%s@%d", n, i))
		}
	}
	step := "-"
	if f.DepartingStep != nil {
		step = fmt.Sprint(*f.DepartingStep)
	}
	return fmt.Sprintf("places=[%s] departs=[%s] touches=[%s] role=%s step=%s",
		strings.Join(f.Places, ","), strings.Join(departs, ","), strings.Join(f.Touches, ","), f.Role, step)
}

// TestFactsFromSteps pins the facts for the shapes the step builders produce
// (material_orders.go): the answers the release questions read. Places is
// PlacesBinAt by construction; Departs is "the segment after the first station
// wait lifts a bin at the node"; Touches is "every pickup and dropoff after the
// first station wait" — the definitions the doors used before the facts were
// stored (segmentAfterFirstStationWaitLifts, binTouchesAfterFirstStationWait).
func TestFactsFromSteps(t *testing.T) {
	t.Parallel()
	S := protocol.WaitKindStation
	for _, c := range []struct {
		name  string
		steps []protocol.ComplexOrderStep
		want  string
	}{
		{"two_robot A (supply)", []protocol.ComplexOrderStep{pick("STAGE"), wait("STAGE", S), drop("PRESS")},
			"places=[PRESS] departs=[] touches=[PRESS] role=placing step=-"},
		{"two_robot B (evac)", []protocol.ComplexOrderStep{wait("PRESS", S), pick("PRESS"), drop("OUT")},
			"places=[OUT] departs=[PRESS@1] touches=[PRESS,OUT] role=departing step=1"},
		{"press-index R1, 3-pos unflipped", []protocol.ComplexOrderStep{wait("PRESS", S), pick("PRESS"), drop("OUT"), pick("IN"), drop("C")},
			"places=[OUT,C] departs=[PRESS@1,IN@3] touches=[PRESS,OUT,IN,C] role=departing step=1"},
		{"press-index R2, 3-pos", []protocol.ComplexOrderStep{wait("B", S), pick("B"), drop("PRESS"), pick("C"), drop("B")},
			"places=[PRESS,B] departs=[B@1,C@3] touches=[B,PRESS,C,B] role=placing step=-"},
		{"changeover R1 with a tooling wait", []protocol.ComplexOrderStep{wait("PRESS", S), pick("PRESS"), drop("OUT"), wait("STAGE", "tooling"), pick("IN"), drop("PRESS")},
			"places=[OUT,PRESS] departs=[PRESS@1] touches=[PRESS,OUT,IN,PRESS] role=departing step=1"},
		{"untagged wait is a station wait", []protocol.ComplexOrderStep{wait("PRESS", ""), pick("PRESS"), drop("OUT")},
			"places=[OUT] departs=[PRESS@1] touches=[PRESS,OUT] role=departing step=1"},
		{"a lane wait first, the station wait later", []protocol.ComplexOrderStep{pick("STAGE"), wait("LANE", protocol.WaitKindLane), drop("B"), wait("PRESS", S), pick("B"), drop("PRESS")},
			"places=[PRESS] departs=[B@4] touches=[B,PRESS] role=placing step=-"},
		{"no station wait: nothing a release lets go", []protocol.ComplexOrderStep{pick("PRESS"), drop("OUT")},
			"places=[OUT] departs=[] touches=[] role=neither step=-"},
		{"set down and taken back", []protocol.ComplexOrderStep{wait("STAGE", S), drop("PRESS"), pick("PRESS"), drop("OUT")},
			"places=[OUT] departs=[PRESS@2] touches=[PRESS,PRESS,OUT] role=departing step=2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := renderFacts(FactsFromSteps(c.steps, "PRESS")); got != c.want {
				t.Errorf("\n got  %s\n want %s", got, c.want)
			}
			// Places is PlacesBinAt for every node the steps name.
			f := FactsFromSteps(c.steps, "PRESS")
			for _, s := range c.steps {
				if f.PlacesBinAt(s.Node) != PlacesBinAt(c.steps, s.Node) {
					t.Errorf("Facts.PlacesBinAt(%s) disagrees with PlacesBinAt", s.Node)
				}
			}
		})
	}
}

// TestFactsRoundTrip: what creation stores is what the loader reads.
func TestFactsRoundTrip(t *testing.T) {
	t.Parallel()
	f := FactsFromSteps([]protocol.ComplexOrderStep{wait("PRESS", protocol.WaitKindStation), pick("PRESS"), drop("OUT")}, "PRESS")
	raw, err := f.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeFacts(raw)
	if err != nil {
		t.Fatal(err)
	}
	if renderFacts(back) != renderFacts(f) {
		t.Errorf("round trip changed the facts: %s -> %s", renderFacts(f), renderFacts(back))
	}
}
