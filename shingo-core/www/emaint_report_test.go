package www

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"time"

	"shingocore/fleet"
)

// TestEMaintReport_ShapeIsPinned pins every key of the E-Maint report, the top
// level and one robot's entry at every depth. The downloaded file is read
// outside Core, and the report moved from the Diagnostics page to the Robots
// page's Fleet tab on the promise that the file did not change; a renamed or
// dropped key fails here rather than in someone's spreadsheet.
func TestEMaintReport_ShapeIsPinned(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	rep := eMaintReport([]fleet.RobotStatus{{VehicleID: "AMR-01", Connected: true, OdoTotal: 82824.6}}, now, "id-1")
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	keys := func(m map[string]any) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	if want := []string{"generated_at", "report_id", "robot_count", "robots", "source"}; !reflect.DeepEqual(keys(got), want) {
		t.Errorf("top-level keys = %v, want %v", keys(got), want)
	}
	if got["report_id"] != "id-1" || got["generated_at"] != "2026-10-15T14:00:00Z" || got["source"] != "shingo-core" {
		t.Errorf("header = %v", got)
	}
	robots := got["robots"].([]any)
	entry := robots[0].(map[string]any)
	if want := []string{"battery", "connected", "controller", "lifts", "odometer", "position", "runtime",
		"safety", "snapshot_at", "task", "vehicle_id"}; !reflect.DeepEqual(keys(entry), want) {
		t.Errorf("robot keys = %v, want %v", keys(entry), want)
	}
	nested := map[string][]string{
		"position":   {"angle", "current_map", "current_station", "x", "y"},
		"odometer":   {"today_m", "total_m"},
		"runtime":    {"session_ms", "total_ms"},
		"lifts":      {"current_height_mm", "error_code", "total_count"},
		"battery":    {"charging", "current_a", "level_pct", "voltage_v"},
		"controller": {"humidity_pct", "temp_c", "voltage_v"},
		"safety":     {"blocked", "emergency"},
		"task":       {"model", "status", "version"},
	}
	for k, want := range nested {
		if got := keys(entry[k].(map[string]any)); !reflect.DeepEqual(got, want) {
			t.Errorf("%s keys = %v, want %v", k, got, want)
		}
	}
	if entry["odometer"].(map[string]any)["total_m"] != 82824.6 {
		t.Errorf("odometer.total_m = %v", entry["odometer"])
	}
}
