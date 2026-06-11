package messaging

import (
	"reflect"
	"testing"

	"shingo/protocol"
	"shingoedge/store/counters"
)

// TestBuildCellCatalog pins the Q-034 grouping: reporting points collapse to one
// cell per PLCName, disabled points included, deterministic order so an
// unchanged point set re-registers identically.
func TestBuildCellCatalog(t *testing.T) {
	points := []counters.ReportingPoint{
		// PRESS-1 hosts two processes; deliberately out of id order + one disabled.
		{ID: 1, PLCName: "PRESS-1", TagName: "T2", StyleID: 14, ProcessID: 20, Enabled: true},
		{ID: 2, PLCName: "PRESS-1", TagName: "T1", StyleID: 14, ProcessID: 10, Enabled: false},
		{ID: 3, PLCName: "WELD-1", TagName: "W1", StyleID: 14, ProcessID: 30, Enabled: true},
		// Blank PLC is skipped — nothing to anchor a cell on.
		{ID: 4, PLCName: "", TagName: "X", StyleID: 14, ProcessID: 99, Enabled: true},
	}

	got := BuildCellCatalog(points)

	want := []protocol.CellCatalogEntry{
		{CellLabel: "PRESS-1", Processes: []protocol.CellProcessBinding{
			{ProcessID: 10, StyleID: 14, PLCName: "PRESS-1", TagName: "T1"},
			{ProcessID: 20, StyleID: 14, PLCName: "PRESS-1", TagName: "T2"},
		}},
		{CellLabel: "WELD-1", Processes: []protocol.CellProcessBinding{
			{ProcessID: 30, StyleID: 14, PLCName: "WELD-1", TagName: "W1"},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildCellCatalog mismatch:\n got %+v\nwant %+v", got, want)
	}

	// Determinism: same input → identical output.
	if !reflect.DeepEqual(BuildCellCatalog(points), got) {
		t.Error("BuildCellCatalog is not deterministic")
	}

	// Empty / all-blank input → empty (not nil-panic) catalog.
	if c := BuildCellCatalog(nil); len(c) != 0 {
		t.Errorf("nil input: want empty, got %+v", c)
	}
}
