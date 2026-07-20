package engine

import "testing"

// Pure tests for the sourcing page's chip and ordering helpers — no DB, so they
// run everywhere. These are the round-2 layout rules: the chip shows the RUNNING
// style's verdict (not the worst-across-styles roll-up), and the rail sorts by
// severity worst-first.

func TestProcessChipStatus_IsRunningStyleVerdict(t *testing.T) {
	// The running style (B) is green while another style (A) is red. The chip
	// must reflect what the process is RUNNING, not the worst option — that is
	// the change from the old roll-up, which would have shown red here.
	pv := &SourcingProcessView{
		RunningStyle: "B",
		Styles: []SourcingStyleView{
			{StyleID: "A", Status: "red"},
			{StyleID: "B", Status: "green"},
		},
	}
	if got := processChipStatus(pv); got != "green" {
		t.Errorf("chip = %q, want green (the running style's verdict, not the worst)", got)
	}
}

func TestProcessChipStatus_FallsBackToRollUpWhenNoRunningStyle(t *testing.T) {
	// Core does not always know the running style (older Edge, or none set). The
	// chip then falls back to the worst-across-styles roll-up so it still says
	// something useful rather than nothing.
	pv := &SourcingProcessView{
		Styles: []SourcingStyleView{
			{StyleID: "A", Status: "green"},
			{StyleID: "B", Status: "red"},
		},
	}
	if got := processChipStatus(pv); got != "red" {
		t.Errorf("chip = %q, want red (roll-up worst when no running style)", got)
	}
}

func TestProcessChipStatus_RunningStyleDroppedFallsBack(t *testing.T) {
	// The running style was "Default", which the builder drops when it has no
	// claims, so it has no view. The chip falls back to the roll-up of what
	// remains — here an empty style set, which is not_configured. This is the
	// honest state: the process is running a claim-less placeholder.
	pv := &SourcingProcessView{RunningStyle: "Default"}
	if got := processChipStatus(pv); got != "not_configured" {
		t.Errorf("chip = %q, want not_configured (running a dropped placeholder)", got)
	}
}

func TestStatusSeverity_WorstFirst(t *testing.T) {
	// The rail order: blocked → at-risk → sourcing → no-data → not-set-up.
	order := []string{"red", "yellow", "green", "no_data", "not_configured"}
	for i := 1; i < len(order); i++ {
		if statusSeverity(order[i-1]) >= statusSeverity(order[i]) {
			t.Errorf("severity(%q) must rank before severity(%q)", order[i-1], order[i])
		}
	}
	// An unknown status sorts after everything real, never before.
	if statusSeverity("something_new") <= statusSeverity("not_configured") {
		t.Error("an unknown status must sort last, not ahead of a known one")
	}
}
