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

// TestStyleSourcingViewFrom_NotConfigured pins that a style with no
// sourceability claims is never offered. Core reports not_configured for it
// (it used to report green, which made an unconfigured process look ready to
// change over to). The picker must block it and say why, without naming
// payloads — none are configured to name.
func TestStyleSourcingViewFrom_NotConfigured(t *testing.T) {
	v := styleSourcingViewFrom(protocol.SourcingState{Status: "not_configured"})
	if !v.Blocked {
		t.Error("an unconfigured style must not be selectable — there is no verdict for it")
	}
	if v.Status != "not set up" {
		t.Errorf("status label = %q, want the operator-facing 'not set up'", v.Status)
	}
	if v.Note != "no sourceability claims" {
		t.Errorf("note = %q, want the reason it cannot be offered", v.Note)
	}
}

// TestStyleSourcingViewFrom_UnknownStatusIsNotSelectable is the forward-compat
// gate. Status is an open string on the wire, so an Edge running against a newer
// Core can receive a verdict it does not know. The default arm must fail
// CLOSED — an unrecognised verdict is not permission to change over.
func TestStyleSourcingViewFrom_UnknownStatusIsNotSelectable(t *testing.T) {
	v := styleSourcingViewFrom(protocol.SourcingState{Status: "some_future_verdict"})
	if !v.Blocked {
		t.Errorf("unknown verdict %q must block, not fall through to selectable", v.Status)
	}
}
