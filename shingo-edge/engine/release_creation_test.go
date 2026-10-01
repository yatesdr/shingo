package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/release"
)

func stepsString(steps []protocol.ComplexOrderStep) string {
	var out []string
	for _, s := range steps {
		w := s.Action + " " + s.Node
		if s.Action == protocol.ActionWait {
			w += "(" + s.Purpose + ")"
		}
		out = append(out, w)
	}
	return strings.Join(out, ", ")
}

// S7: every pickup at a curtained node gets a station wait in front unless one
// already stands there; a dropoff gets none. The reported ordinal is the first
// added wait's among the plan's station waits.
func TestWithCurtainWaits(t *testing.T) {
	t.Parallel()
	ready := func(n string) protocol.ComplexOrderStep { return stationWait(n, release.PurposeReady) }
	pick := func(n string) protocol.ComplexOrderStep {
		return protocol.ComplexOrderStep{Action: protocol.ActionPickup, Node: n}
	}
	drop := func(n string) protocol.ComplexOrderStep {
		return protocol.ComplexOrderStep{Action: protocol.ActionDropoff, Node: n}
	}
	curtained := map[string]bool{"SYN-FG": true}
	for _, c := range []struct {
		name  string
		steps []protocol.ComplexOrderStep
		want  string
		first int
	}{
		{"the per-position swap: one wait, before it lifts the old bin; the return drop finishes",
			[]protocol.ComplexOrderStep{pick("SYN-FG"), drop("SYN-OUT"), pick("SYN-MKT"), drop("SYN-FG")},
			"wait SYN-FG(ready), pickup SYN-FG, dropoff SYN-OUT, pickup SYN-MKT, dropoff SYN-FG", 0},
		{"a delivery: no wait in front of the drop",
			[]protocol.ComplexOrderStep{pick("SYN-STAGE"), drop("SYN-FG")},
			"pickup SYN-STAGE, dropoff SYN-FG", -1},
		{"a crossing a wait already guards is left alone",
			[]protocol.ComplexOrderStep{stationWait("SYN-FG", release.PurposeSwap), pick("SYN-FG"), drop("SYN-OUT")},
			"wait SYN-FG(swap), pickup SYN-FG, dropoff SYN-OUT", -1},
		{"an earlier station wait counts toward the ordinal",
			[]protocol.ComplexOrderStep{stationWait("SYN-STAGE", release.PurposeToolingDone), pick("SYN-STAGE"), drop("SYN-OUT"),
				pick("SYN-FG"), drop("SYN-OUT")},
			"wait SYN-STAGE(tooling_done), pickup SYN-STAGE, dropoff SYN-OUT, wait SYN-FG(ready), pickup SYN-FG, dropoff SYN-OUT", 1},
		{"no curtained node: unchanged",
			[]protocol.ComplexOrderStep{pick("SYN-A"), drop("SYN-B")},
			"pickup SYN-A, dropoff SYN-B", -1},
	} {
		got, first := withCurtainWaits(c.steps, curtained, ready)
		if s := stepsString(got); s != c.want || first != c.first {
			t.Errorf("%s:\n got  %s (first %d)\n want %s (first %d)", c.name, s, first, c.want, c.first)
		}
	}
}

// A creation's intent follows its leg through each of its curtain waits
// (Standing), and a system-created one is marked System so a live curtain
// drops it and asks for a press (Q8).
func TestCreationIntentStandsThroughItsWaits(t *testing.T) {
	t.Parallel()
	in := &release.Intent{StationWait: 0, Origin: release.OriginCreation, Standing: true, System: true, SentAt: "x"}
	next := 1
	if got := in.OnStaged(&next); got != release.StageAdvance {
		t.Errorf("a standing intent staged at its next wait: %v, want StageAdvance", got)
	}
	op := &release.Intent{StationWait: 0, SentAt: "x"}
	if got := op.OnStaged(&next); got != release.StageConsume {
		t.Errorf("an operator's press staged at its next wait: %v, want StageConsume", got)
	}
}
