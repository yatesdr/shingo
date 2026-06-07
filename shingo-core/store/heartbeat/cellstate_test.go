package heartbeat

import (
	"testing"
	"time"
)

// procEv builds a part-fire for one Process at t.
func procEv(processID int64, t time.Time) PartEvent {
	return PartEvent{ProcessID: processID, RecordedAt: t, Delta: 1, CountValue: 1}
}

// TestComputeResolvedCellState pins the Phase E primary/sub split: a cell's
// event stream is partitioned by process_id, each Process gets its own derived
// state, and events for processes outside the config are ignored.
func TestComputeResolvedCellState(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	// Primary (100) firing every ~20s up to ~1s ago → running.
	// Sub (200) last fired ~12min ago at a 20s cadence → stopped.
	// Process 999 is NOT in the config and must be dropped.
	events := []PartEvent{
		procEv(100, ago(61*time.Second)), procEv(100, ago(41*time.Second)),
		procEv(100, ago(21*time.Second)), procEv(100, ago(1*time.Second)),
		procEv(200, ago(742*time.Second)), procEv(200, ago(722*time.Second)),
		procEv(999, ago(2*time.Second)), procEv(999, ago(1*time.Second)),
	}

	cfg := CellConfig{
		CellID: "SNF2", Station: "plant-a.line-1", DisplayName: "Assembly NF 2",
		PrimaryProcessID: 100, SubProcessIDs: []int64{200},
	}
	got := ComputeResolvedCellState(events, cfg, now, DefaultThresholds())

	if got.CellID != "SNF2" || got.Station != "plant-a.line-1" || got.DisplayName != "Assembly NF 2" {
		t.Errorf("metadata not carried through: %+v", got)
	}
	if got.Primary.ProcessID != 100 {
		t.Errorf("primary process_id = %d, want 100", got.Primary.ProcessID)
	}
	if got.Primary.State != StateRunning {
		t.Errorf("primary state = %q, want %q", got.Primary.State, StateRunning)
	}
	if len(got.SubProcesses) != 1 {
		t.Fatalf("sub_processes len = %d, want 1 (process 999 must be dropped)", len(got.SubProcesses))
	}
	if got.SubProcesses[0].ProcessID != 200 {
		t.Errorf("sub process_id = %d, want 200", got.SubProcesses[0].ProcessID)
	}
	if got.SubProcesses[0].State != StateStopped {
		t.Errorf("sub state = %q, want %q", got.SubProcesses[0].State, StateStopped)
	}
}

// TestComputeResolvedCellState_SimpleCell pins the simple-cell shape: no subs →
// sub_processes is an empty (non-nil) slice so the frontend draws zero
// satellites with no special casing, and the JSON is [] not null.
func TestComputeResolvedCellState_SimpleCell(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	cfg := CellConfig{CellID: "SNF3", Station: "plant-a.line-1", PrimaryProcessID: 300}
	got := ComputeResolvedCellState([]PartEvent{procEv(300, now.Add(-time.Second))}, cfg, now, DefaultThresholds())

	if got.SubProcesses == nil {
		t.Error("sub_processes is nil, want empty slice")
	}
	if len(got.SubProcesses) != 0 {
		t.Errorf("sub_processes len = %d, want 0", len(got.SubProcesses))
	}
	if got.Primary.ProcessID != 300 {
		t.Errorf("primary process_id = %d, want 300", got.Primary.ProcessID)
	}
}

// TestComputeResolvedCellState_NoEvents pins that a configured Process with no
// events still appears (as no-data) so the tile renders a stable dot set.
func TestComputeResolvedCellState_NoEvents(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	cfg := CellConfig{CellID: "C", Station: "s", PrimaryProcessID: 1, SubProcessIDs: []int64{2}}
	got := ComputeResolvedCellState(nil, cfg, now, DefaultThresholds())

	if got.Primary.State != StateNoData {
		t.Errorf("primary state = %q, want %q", got.Primary.State, StateNoData)
	}
	if len(got.SubProcesses) != 1 || got.SubProcesses[0].State != StateNoData {
		t.Errorf("sub-process should be present as no-data, got %+v", got.SubProcesses)
	}
}

// TestComputeCellHeartbeat pins the drill payload: the cell's windowed events
// split per Process (primary first, then subs), each carrying only its own
// events; events for processes outside the config are dropped.
func TestComputeCellHeartbeat(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	since := now.Add(-time.Hour)
	events := []PartEvent{
		procEv(100, now.Add(-40*time.Minute)), procEv(100, now.Add(-20*time.Minute)), procEv(100, now.Add(-1*time.Minute)),
		procEv(200, now.Add(-10*time.Minute)),
		procEv(999, now.Add(-5*time.Minute)), // outside the cell — must be dropped
	}
	cfg := CellConfig{CellID: "SNF2", Station: "s", PrimaryProcessID: 100, SubProcessIDs: []int64{200}}
	got := ComputeCellHeartbeat(events, cfg, since, now, DefaultThresholds())

	if len(got.Processes) != 2 {
		t.Fatalf("processes len = %d, want 2 (primary + 1 sub; 999 dropped)", len(got.Processes))
	}
	if !got.Processes[0].Primary || got.Processes[0].ProcessID != 100 {
		t.Errorf("first window should be primary 100, got %+v", got.Processes[0])
	}
	if len(got.Processes[0].Events) != 3 || got.Processes[0].Metrics.Parts != 3 {
		t.Errorf("primary window should have 3 events/parts, got events=%d parts=%d",
			len(got.Processes[0].Events), got.Processes[0].Metrics.Parts)
	}
	if got.Processes[1].ProcessID != 200 || got.Processes[1].Primary {
		t.Errorf("second window should be sub 200, got %+v", got.Processes[1])
	}
	if len(got.Processes[1].Events) != 1 {
		t.Errorf("sub window should have 1 event, got %d", len(got.Processes[1].Events))
	}
}
