package sourceability

import (
	"reflect"
	"testing"
	"time"

	"shingocore/store/plantclaims"
)

// pure fixtures — no DB, so this file has no build tag and runs everywhere.

// sampleAt indexes samples by (process, style, node) for order-independent
// assertions.
func sampleAt(samples []TTESample, process, style, node string) (TTESample, bool) {
	for _, s := range samples {
		if s.ProcessID == process && s.StyleID == style && s.Line.NodeName == node {
			return s, true
		}
	}
	return TTESample{}, false
}

// activeFixture builds three styles on three processes — one RED, one that
// WOULD be yellow if the tier were on, one plainly GREEN — each the running
// style of its own process.
func activeFixture() (Inputs, plantclaims.ProcessKey, plantclaims.ProcessKey, plantclaims.ProcessKey) {
	red := key("PRESS-1", "A")
	atRisk := key("PRESS-2", "A")
	green := key("PRESS-3", "A")
	in := Inputs{
		Styles: []plantclaims.ProcessKey{red, atRisk, green},
		Claims: map[plantclaims.ProcessKey][]plantclaims.ClaimRow{
			red:    {claim("RED-N1", "BIN-MISSING", 0)},
			atRisk: {claim("RISK-N1", "BIN-RISK", 0)},
			green:  {claim("GRN-N1", "BIN-OK", 0)},
		},
		Pool: map[string]int{"BIN-RISK": 2, "BIN-OK": 2}, // BIN-MISSING absent → RED
		// Every line has stock staged and a live consumption rate, so all three
		// have a KNOWN projection — the RED one included. That is the case the
		// table exists for.
		LineUOP:    map[string]int{"RED-N1": 50, "RISK-N1": 10, "GRN-N1": 5000},
		RatePerSec: map[string]float64{"BIN-MISSING": 1, "BIN-RISK": 1, "BIN-OK": 1},
		ActiveStyles: map[string]string{
			"PRESS-1": "A", "PRESS-2": "A", "PRESS-3": "A",
		},
	}
	return in, red, atRisk, green
}

// THE PIN THE WHOLE CHANGE EXISTS FOR. Compute returns early for a RED style,
// above the TTE block, so samples built from what Compute surfaced would cover
// only the lines that were already fine. A line that ran dry is the one the
// forecast has to be graded on.
func TestComputeWithSamples_RedStyleIsSampled(t *testing.T) {
	in, _, _, _ := activeFixture()

	states, samples := ComputeWithSamples(in, Config{}, now)

	if got := byKey(states)[key("PRESS-1", "A")]; got.Status != StatusRed {
		t.Fatalf("PRESS-1 status = %q, want red", got.Status)
	}
	s, ok := sampleAt(samples, "PRESS-1", "A", "RED-N1")
	if !ok {
		t.Fatalf("no sample for the RED style's line; samples = %+v", samples)
	}
	if !s.Line.Known {
		t.Errorf("RED line projection Known = false, want true (50 UOP at 1/sec)")
	}
	if want := 50 * time.Second; s.Line.TimeToEmpty != want {
		t.Errorf("RED line TTE = %v, want %v", s.Line.TimeToEmpty, want)
	}
	if s.StyleStatus != StatusRed {
		t.Errorf("sample StyleStatus = %q, want red — the sample carries the verdict its own pass reached", s.StyleStatus)
	}
}

// Every claim of every running style is sampled, whatever the verdict, and the
// yellow gate does not decide who gets recorded.
func TestComputeWithSamples_AllThreeVerdictsSampled(t *testing.T) {
	in, _, _, _ := activeFixture()

	_, samples := ComputeWithSamples(in, Config{}, now)

	if len(samples) != 3 {
		t.Fatalf("samples = %d, want 3 (one per running style's single claim): %+v", len(samples), samples)
	}
	for _, c := range []struct {
		process, node string
		want          Status
	}{
		{"PRESS-1", "RED-N1", StatusRed},
		{"PRESS-2", "RISK-N1", StatusGreen}, // yellow tier off → green
		{"PRESS-3", "GRN-N1", StatusGreen},
	} {
		s, ok := sampleAt(samples, c.process, "A", c.node)
		if !ok {
			t.Errorf("no sample for %s/%s", c.process, c.node)
			continue
		}
		if s.StyleStatus != c.want {
			t.Errorf("%s sample status = %q, want %q", c.process, s.StyleStatus, c.want)
		}
	}
}

// WITH THE GATE OFF, NOTHING ON THE WIRE MOVES. The samples are a side channel:
// the identical StyleState set a reader consumed before the change is what it
// consumes after, byte for byte.
func TestComputeWithSamples_WireIsByteIdenticalToCompute(t *testing.T) {
	in, _, _, _ := activeFixture()

	for _, cfg := range []Config{
		{}, // yellow off, as both plants ship
		{YellowEnabled: true, Horizon: 60 * time.Second}, // and on, so the gate is covered too
	} {
		viaCompute := Compute(in, cfg, now)
		viaSibling, _ := ComputeWithSamples(in, cfg, now)
		if !reflect.DeepEqual(viaCompute, viaSibling) {
			t.Errorf("cfg %+v: Compute and ComputeWithSamples disagree on the wire\n compute = %+v\n sibling = %+v",
				cfg, viaCompute, viaSibling)
		}
	}
}

// The RED style's Missing list is untouched by sampling.
func TestComputeWithSamples_RedMissingUnchanged(t *testing.T) {
	in, red, _, _ := activeFixture()

	states, _ := ComputeWithSamples(in, Config{}, now)

	got := byKey(states)[red]
	if !reflect.DeepEqual(got.Missing, []string{"BIN-MISSING"}) {
		t.Errorf("missing = %v, want [BIN-MISSING]", got.Missing)
	}
}

// ACTIVE STYLE ONLY. in.Styles carries every configured style; sampling all of
// them would multiply the rows by styles-per-process without adding a line
// anyone is running dry on.
func TestComputeWithSamples_OnlyTheRunningStyleIsSampled(t *testing.T) {
	running, idle := key("PRESS-1", "A"), key("PRESS-1", "B")
	in := Inputs{
		Styles: []plantclaims.ProcessKey{running, idle},
		Claims: map[plantclaims.ProcessKey][]plantclaims.ClaimRow{
			running: {claim("N-RUN", "BIN-A", 0)},
			idle:    {claim("N-IDLE", "BIN-A", 0)},
		},
		Pool:         map[string]int{"BIN-A": 9},
		LineUOP:      map[string]int{"N-RUN": 100, "N-IDLE": 100},
		RatePerSec:   map[string]float64{"BIN-A": 1},
		ActiveStyles: map[string]string{"PRESS-1": "A"},
	}

	states, samples := ComputeWithSamples(in, Config{}, now)

	// Both styles still get a verdict — only the sampling is scoped.
	if len(states) != 2 {
		t.Fatalf("states = %d, want 2 — every configured style keeps its verdict", len(states))
	}
	if len(samples) != 1 {
		t.Fatalf("samples = %d, want 1 (the running style only): %+v", len(samples), samples)
	}
	if samples[0].StyleID != "A" || samples[0].Line.NodeName != "N-RUN" {
		t.Errorf("sampled %s/%s, want style A at N-RUN", samples[0].StyleID, samples[0].Line.NodeName)
	}
}

// A process with no active style contributes nothing. Core must not guess at a
// running style, and a missing sample is a visible blind spot in the score
// where a guessed one is a wrong number nothing flags.
func TestComputeWithSamples_NoActiveStyleSamplesNothing(t *testing.T) {
	k := key("PRESS-1", "A")
	in := Inputs{
		Styles:     []plantclaims.ProcessKey{k},
		Claims:     map[plantclaims.ProcessKey][]plantclaims.ClaimRow{k: {claim("N1", "BIN-A", 0)}},
		Pool:       map[string]int{"BIN-A": 1},
		LineUOP:    map[string]int{"N1": 10},
		RatePerSec: map[string]float64{"BIN-A": 1},
		// ActiveStyles deliberately nil.
	}

	states, samples := ComputeWithSamples(in, Config{}, now)

	if len(states) != 1 {
		t.Fatalf("states = %d, want 1 — the verdict does not depend on the active flag", len(states))
	}
	if len(samples) != 0 {
		t.Errorf("samples = %+v, want none when no style is flagged active", samples)
	}
}

// An unconfigured style has no claims and so no lines to project.
func TestComputeWithSamples_NotConfiguredSamplesNothing(t *testing.T) {
	k := key("PRESS-1", "A")
	in := Inputs{
		Styles:       []plantclaims.ProcessKey{k},
		Claims:       map[plantclaims.ProcessKey][]plantclaims.ClaimRow{},
		ActiveStyles: map[string]string{"PRESS-1": "A"},
	}

	states, samples := ComputeWithSamples(in, Config{}, now)

	if got := byKey(states)[k]; got.Status != StatusNotConfigured {
		t.Fatalf("status = %q, want not_configured", got.Status)
	}
	if len(samples) != 0 {
		t.Errorf("samples = %+v, want none for a style with no claims", samples)
	}
}

// AN UNKNOWN PROJECTION IS STILL A SAMPLE. A line with nothing staged, or a
// payload with no consumption in the rate window, has no time-to-empty — and
// recording that fact is what lets the score report coverage blind spots
// instead of quietly scoring only the lines that happened to have one.
func TestComputeWithSamples_UnknownProjectionStillRecorded(t *testing.T) {
	k := key("PRESS-1", "A")
	in := Inputs{
		Styles: []plantclaims.ProcessKey{k},
		Claims: map[plantclaims.ProcessKey][]plantclaims.ClaimRow{
			k: {claim("N-NOSTOCK", "BIN-A", 0), claim("N-NORATE", "BIN-B", 1)},
		},
		Pool: map[string]int{"BIN-A": 1, "BIN-B": 1},
		// N-NOSTOCK absent from LineUOP → nothing staged.
		LineUOP: map[string]int{"N-NORATE": 100},
		// BIN-B absent from RatePerSec → no consumption in the window.
		RatePerSec:   map[string]float64{"BIN-A": 1},
		ActiveStyles: map[string]string{"PRESS-1": "A"},
	}

	_, samples := ComputeWithSamples(in, Config{}, now)

	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2 — an unknown projection is still a sample", len(samples))
	}
	for _, node := range []string{"N-NOSTOCK", "N-NORATE"} {
		s, ok := sampleAt(samples, "PRESS-1", "A", node)
		if !ok {
			t.Fatalf("no sample at %s", node)
		}
		if s.Line.Known {
			t.Errorf("%s Known = true, want false", node)
		}
		if s.Line.TimeToEmpty != 0 {
			t.Errorf("%s TTE = %v, want zero value (it stores as NULL)", node, s.Line.TimeToEmpty)
		}
	}
}

// reorder_point rides along from the claim, so the score can ask whether the
// reorder point was set where the forecast said it should be.
func TestComputeWithSamples_ReorderPointCarried(t *testing.T) {
	k := key("PRESS-1", "A")
	c := claim("N1", "BIN-A", 0)
	c.ReorderPoint = 17
	in := Inputs{
		Styles:       []plantclaims.ProcessKey{k},
		Claims:       map[plantclaims.ProcessKey][]plantclaims.ClaimRow{k: {c}},
		Pool:         map[string]int{"BIN-A": 1},
		LineUOP:      map[string]int{"N1": 100},
		RatePerSec:   map[string]float64{"BIN-A": 1},
		ActiveStyles: map[string]string{"PRESS-1": "A"},
	}

	_, samples := ComputeWithSamples(in, Config{}, now)

	if len(samples) != 1 {
		t.Fatalf("samples = %d, want 1", len(samples))
	}
	if samples[0].ReorderPoint != 17 {
		t.Errorf("reorder point = %d, want 17", samples[0].ReorderPoint)
	}
}
