package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStationPull_ABaselineFlagsAStationThatFellShortAndOneThatStopped: a
// baseline written by -pull-out reads back to the same rates, a station under
// pullBelow of its baseline is named, one within it is not, and a station the
// baseline names that ticked nothing is named as silent.
func TestStationPull_ABaselineFlagsAStationThatFellShortAndOneThatStopped(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "pull.tsv")
	certified := []stationPull{
		{station: "PRESS-1", kind: "produce", perPlantHour: 40},
		{station: "PRESS-2", kind: "produce", perPlantHour: 40},
		{station: "WELD-2", kind: "consume", perPlantHour: 20},
	}
	if err := writePullBaseline(path, certified, 4); err != nil {
		t.Fatal(err)
	}
	base, err := readPullBaseline(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(base) != 3 || base["PRESS-1|produce"] != 40 {
		t.Fatalf("baseline read back as %v, want the three written rates", base)
	}
	run := []stationPull{
		{station: "PRESS-1", kind: "produce", units: 48, perPlantHour: 12, hours: 4},  // stopped early
		{station: "PRESS-2", kind: "produce", units: 148, perPlantHour: 37, hours: 4}, // within spread
	}
	run, silent := applyPullBaseline(run, base)
	got := strings.Join(pullFindings(run, silent), "\n")
	if !strings.Contains(got, "PRESS-1 produce") {
		t.Errorf("a station at 30%% of its pull was not named:\n%s", got)
	}
	if strings.Contains(got, "PRESS-2") {
		t.Errorf("a station at 92%% of its pull was named:\n%s", got)
	}
	if !strings.Contains(got, "WELD-2 consume ticked no unit at all") {
		t.Errorf("a station the baseline names that never pulled was not named:\n%s", got)
	}
}

// TestStationPull_AQuietStationsNoiseIsNotAShortfall: 12 units against an
// expected 16 is 75% of the pull but inside a Poisson count's spread at 16;
// 4 against 16 is not.
func TestStationPull_AQuietStationsNoiseIsNotAShortfall(t *testing.T) {
	t.Parallel()
	noise := stationPull{units: 12, perPlantHour: 3, hours: 4, baseline: 4}
	if noise.below() {
		t.Error("12 units against an expected 16 was called below its pull")
	}
	stopped := stationPull{units: 4, perPlantHour: 1, hours: 4, baseline: 4}
	if !stopped.below() {
		t.Error("4 units against an expected 16 was not called below its pull")
	}
}

// TestStationPull_AMalformedBaselineIsRefused: a line without the station|kind
// key or a rate is an error, not a silently empty baseline that flags nothing.
func TestStationPull_AMalformedBaselineIsRefused(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "pull.tsv")
	if err := os.WriteFile(path, []byte("# header\nPRESS-1 40\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPullBaseline(path); err == nil {
		t.Error("a baseline line with no tab and no kind was accepted")
	}
}
