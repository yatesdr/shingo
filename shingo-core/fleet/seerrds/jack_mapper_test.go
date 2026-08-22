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
