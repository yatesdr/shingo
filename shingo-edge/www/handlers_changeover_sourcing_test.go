package www

import (
	"testing"

	"shingo/protocol"
)

// TestStyleSourcingViewFrom pins the changeover picker's per-style annotation:
// red is shown-but-blocked with the missing payloads, yellow is selectable with
// a running-low hint, green is clean and selectable.
func TestStyleSourcingViewFrom(t *testing.T) {
	red := styleSourcingViewFrom(protocol.SourcingState{Status: "red", Missing: []string{"BIN-B", "BIN-C"}})
	if !red.Blocked {
		t.Error("red style must be blocked (shown but not selectable)")
	}
	if red.Note != "missing BIN-B, BIN-C" {
		t.Errorf("red note = %q, want the missing payloads", red.Note)
	}

	yellow := styleSourcingViewFrom(protocol.SourcingState{
		Status: "yellow",
		AtRisk: []protocol.SourcingAtRisk{{PayloadCode: "BIN-A"}},
	})
	if yellow.Blocked {
		t.Error("yellow style must stay selectable (change over, but refill)")
	}
	if yellow.Note != "low: BIN-A" {
		t.Errorf("yellow note = %q, want the at-risk payloads", yellow.Note)
	}

	green := styleSourcingViewFrom(protocol.SourcingState{Status: "green"})
	if green.Blocked || green.Note != "" {
		t.Errorf("green style = %+v, want selectable with no note", green)
	}
}
