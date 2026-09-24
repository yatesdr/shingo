package heartbeat

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata golden files")

// goldenT0 anchors the fixed fixture. Synthetic; no plant data.
var goldenT0 = time.Date(2026, 1, 15, 8, 0, 0, 0, time.UTC)

// goldenEvents is a fixed stream for one cell with two Processes (7 primary,
// 9 sub). It carries the two shapes the heartbeat math is ambiguous about:
//   - a tick with delta = 3 (three strokes inside one poll interval), and
//   - a jump (an unconfirmed PLC gap, delta 592).
//
// Plus an ordinary ~20 s rhythm, a micro-stop and a stop, so every output
// field is exercised.
func goldenEvents() []PartEvent {
	type e struct {
		sec     int
		pid     int64
		delta   int64
		anomaly string
	}
	raw := []e{
		{0, 7, 1, ""}, {20, 7, 1, ""}, {40, 7, 1, ""}, {60, 7, 1, ""},
		{80, 7, 3, ""},        // three strokes in one poll
		{100, 7, 1, ""},       //
		{120, 7, 592, "jump"}, // unconfirmed PLC gap
		{140, 7, 1, ""},
		{400, 7, 1, ""}, // 260 s gap: micro-stop at a 20 s target
		{420, 7, 1, ""},
		{440, 7, 1, ""},
		{1200, 7, 1, ""}, // 760 s gap: stopped
		{1220, 7, 1, ""},
		// Sub-process 9: a slower 45 s rhythm.
		{10, 9, 1, ""}, {55, 9, 1, ""}, {100, 9, 2, ""}, {145, 9, 1, ""}, {190, 9, 1, ""},
	}
	out := make([]PartEvent, len(raw))
	for i, r := range raw {
		out[i] = PartEvent{
			CellID:         "stn-test",
			RecordedAt:     goldenT0.Add(time.Duration(r.sec) * time.Second),
			EdgeSnapshotID: int64(i + 1),
			CountValue:     int64(1000 + i),
			Delta:          r.delta,
			Anomaly:        r.anomaly,
			ProcessID:      r.pid,
			StyleID:        3,
		}
	}
	return out
}

func primaryOnly(evs []PartEvent) []PartEvent {
	var out []PartEvent
	for _, e := range evs {
		if e.ProcessID == 7 {
			out = append(out, e)
		}
	}
	return out
}

// TestHeartbeatMath_Golden (P3) pins every pure heartbeat output over a fixed
// stream, byte for byte: ComputeCellState, ComputeStops, ComputeMetrics,
// ComputeResolvedCellState and ComputeCellHeartbeat, as JSON.
//
// In its current form it pins two things a builder could otherwise "fix"
// quietly:
//   - Parts and PartsLastHour count ROWS, not ΣDelta — the delta = 3 tick is
//     one part (heartbeat.go ComputeMetrics `Parts: len(events)`, and the
//     PartsLastHour loop).
//   - A jump counts as a part and as an ordinary gap in cycle, target and stop
//     math.
//
// INVERTS when Parts are counted by delta and jumps are excluded until
// confirmed (the "count by delta, not by row" change); the golden's diff is the
// named fix. Regenerate with -update and read the diff.
func TestHeartbeatMath_Golden(t *testing.T) {
	t.Parallel()
	evs := goldenEvents()
	prim := primaryOnly(evs)
	th := DefaultThresholds()
	now := goldenT0.Add(1230 * time.Second)
	since, until := goldenT0.Add(-time.Minute), now
	target := EstimateTarget(prim)
	cfg := CellConfig{CellID: "cell-test", Station: "stn-test", DisplayName: "Cell Test",
		PrimaryProcessID: 7, SubProcessIDs: []int64{9}}

	got := map[string]any{
		"estimate_target_ms":  target.Milliseconds(),
		"cell_state":          ComputeCellState(prim, target, now, th),
		"cell_state_mid_stop": ComputeCellState(prim[:11], target, goldenT0.Add(700*time.Second), th),
		// The last tick carries three strokes: CurrentCycleMS is its gap.
		"cell_state_after_triple": ComputeCellState(prim[:5], target, goldenT0.Add(81*time.Second), th),
		"stops":                   ComputeStops(prim, target, th),
		"metrics":                 ComputeMetrics(prim, since, until, target, th),
		"resolved_cell_state":     ComputeResolvedCellState(evs, cfg, now, th),
		"cell_heartbeat":          ComputeCellHeartbeat(evs, cfg, since, until, th),
	}
	b, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b = append(b, '\n')
	path := filepath.Join("testdata", "heartbeat_math_golden.json")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), b) {
		t.Errorf("heartbeat math changed. got:\n%s", b)
	}
}
