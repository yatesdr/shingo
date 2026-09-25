package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadLog_CountsADissolveThatCancelsALegMidLift pins the finding: a dig leg
// the dissolve cancelled after its lift and before its set-down counts once;
// one that set down first, or never lifted, does not.
func TestReadLog_CountsADissolveThatCancelsALegMidLift(t *testing.T) {
	log := `core-1  | 2026-09-25T08:05:27.630Z [engine] transit: bin 59 entered _TRANSIT (order 212, block sg-212-b2 @ LSC_014, step 0)
core-1  | 2026/09/25 08:05:27 engine: order 212 cancelled: reshuffle dissolved: the dig's plan went stale; re-planning
core-1  | 2026/09/25 08:05:27 engine: order 212 cancelled: reshuffle dissolved: the dig's plan went stale; re-planning
core-1  | 2026-09-25T08:06:00.000Z [engine] transit: bin 60 entered _TRANSIT (order 300, block sg-300-b2 @ LSC_020, step 0)
core-1  | 2026-09-25T08:06:04.000Z [engine] transit: bin 60 stored at LSC_031 on dropoff (order 300, block sg-300-b3) — slot now reflects the physical bin
core-1  | 2026/09/25 08:06:05 engine: order 300 cancelled: reshuffle dissolved: the dig's plan went stale; re-planning
core-1  | 2026/09/25 08:06:06 engine: order 301 cancelled: reshuffle dissolved: a dig leg failed; the demand re-plans
`
	p := filepath.Join(t.TempDir(), "core.log")
	if err := os.WriteFile(p, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readLog(p).midLiftDissolves; got != 1 {
		t.Fatalf("mid-lift dissolves = %d, want 1 (order 212 only: 300 set down first, 301 never lifted)", got)
	}
}
