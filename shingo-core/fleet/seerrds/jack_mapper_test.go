package seerrds

import (
	"encoding/json"
	"testing"

	"shingocore/rds"
)

// The two live shapes, read off Springfield's wire
// (FINDING-seer-jackunload-vs-block-completion-2026-08-12.md §"Robot telemetry"):
// every loaded robot reports jack_state 1 / height 0.0601 / isFull true, and
// every empty one reports 3 / ≈ -0.0001 / false.
//
// These are ingested because the stranded-bin inference asks "is the bin still
// on this robot", and jack_state is the vendor's own answer. jack_height was
// already carried and stays as the fallback proxy.

const loadedRobotJSON = `{
  "vehicle_id": "AMR-04",
  "isLoaded": true,
  "rbk_report": {
    "current_station": "ALN_003.S1",
    "last_station": "ALN_003.S1",
    "jack": {"jack_emc": false, "jack_enable": true, "jack_error_code": 0,
             "jack_height": 0.0601, "jack_isFull": true, "jack_load_times": 681,
             "jack_mode": false, "jack_speed": 0, "jack_state": 1}
  }
}`

const emptyRobotJSON = `{
  "vehicle_id": "AMR-04",
  "isLoaded": false,
  "rbk_report": {
    "current_station": "",
    "last_station": "SMN_024",
    "jack": {"jack_emc": false, "jack_enable": true, "jack_error_code": 0,
             "jack_height": -0.0001, "jack_isFull": false, "jack_load_times": 681,
             "jack_mode": false, "jack_speed": 0, "jack_state": 3}
  }
}`

func TestMapRobotStatus_CarriesTheJackState(t *testing.T) {
	t.Parallel()

	var loaded rds.RobotStatus
	if err := json.Unmarshal([]byte(loadedRobotJSON), &loaded); err != nil {
		t.Fatalf("unmarshal loaded: %v", err)
	}
	got := mapRobotStatus(loaded)
	if got.JackState != 1 {
		t.Errorf("JackState = %d, want 1 (loading in place)", got.JackState)
	}
	if !got.JackIsFull {
		t.Error("JackIsFull lost")
	}
	if !got.IsLoaded {
		t.Error("IsLoaded lost — it is at the ROBOT level on the wire, not inside rbk_report")
	}
	if got.LiftHeight != 0.0601 {
		t.Errorf("LiftHeight = %v, want 0.0601", got.LiftHeight)
	}

	var empty rds.RobotStatus
	if err := json.Unmarshal([]byte(emptyRobotJSON), &empty); err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	got = mapRobotStatus(empty)
	if got.JackState != 3 {
		t.Errorf("JackState = %d, want 3 (unloading in place)", got.JackState)
	}
	if got.JackIsFull || got.IsLoaded {
		t.Error("an empty deck must not report as full")
	}
	// CurrentStation is empty while a robot wanders; LastStation is what the
	// inference falls back to.
	if got.CurrentStation != "" || got.LastStation != "SMN_024" {
		t.Errorf("stations = %q / %q", got.CurrentStation, got.LastStation)
	}
}

// A table turned under a cart, scrubbed from Hopkinsville AMR-05's
// /robotsStatus on 2026-11-10 at 14:50:51, after a #3057 to 1.57
// (turntable-bintask-2026-11-10/turn3057_145043.log): the table at 1.569, the
// body's angle unchanged, the turn's task finished (4).
const turnedTableRobotJSON = `{
  "vehicle_id": "AMR-05",
  "rbk_report": {
    "angle": 3.131, "spin": 1.569, "task_status": 4, "current_station": "AP590",
    "jack": {"jack_height": -0.0001, "jack_isFull": false, "jack_state": 3}
  }
}`

// spin is the table's angle, and it reaches fleet.RobotStatus as TableAngle,
// apart from the body's angle: the table turn reads nothing else to know it is
// done.
func TestMapRobotStatus_CarriesTheTableAngle(t *testing.T) {
	t.Parallel()
	var r rds.RobotStatus
	if err := json.Unmarshal([]byte(turnedTableRobotJSON), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := mapRobotStatus(r)
	if got.TableAngle != 1.569 {
		t.Errorf("TableAngle = %v, want 1.569 (rbk_report.spin)", got.TableAngle)
	}
	if got.Angle != 3.131 {
		t.Errorf("Angle = %v, want the body's 3.131 — the table angle must not leak into it", got.Angle)
	}
	if got.TaskStatus != 4 {
		t.Errorf("TaskStatus = %d, want 4", got.TaskStatus)
	}
}
