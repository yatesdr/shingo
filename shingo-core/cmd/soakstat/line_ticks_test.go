package main

import (
	"testing"
	"time"
)

// A line whose last PLC tick is more than the quiet window before the run's
// last tick anywhere stopped, and it is a finding at the top of the report
// (the orc's ruling on the D2 after, §2.3: line 2 stopped before the end, and
// the report read its tick count as a line that kept running).
func TestLineTicks_ALineThatStoppedIsAFinding(t *testing.T) {
	t.Parallel()
	end := time.Date(2026, 10, 12, 6, 15, 0, 0, time.UTC)
	edges := []*edgeReading{
		{station: "edge1.line1", lines: []lineTick{{line: "PRESS-1", ticks: 354, last: end}, {line: "WELD-3", ticks: 448, last: end.Add(-20 * time.Minute)}}},
		{station: "edge2.line1", lines: []lineTick{{line: "LINE-2", ticks: 140, last: end.Add(-83 * time.Minute)}}},
	}
	got := stoppedLines(edges)
	if len(got) != 2 || got[0].line != "LINE-2" || got[1].line != "WELD-3" {
		t.Fatalf("stopped lines %+v; want LINE-2 then WELD-3, longest stopped first", got)
	}
	if got[0].before != 83*time.Minute {
		t.Errorf("LINE-2 stopped %v before the end, want 83m", got[0].before)
	}
}
