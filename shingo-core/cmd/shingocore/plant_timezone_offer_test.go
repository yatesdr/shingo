package main

import (
	"os"
	"strings"
	"testing"
)

// TestPinPlantTimezone_MainOffersResolvedZone: main() cannot run in a test, so
// this holds the one line that matters (C9 b). Core hands its data service the
// zone www.OfferedPlantTimezone resolves (PLANT_TIMEZONE, else the yaml; pinned
// in www by TestPinPlantTimezone_OfferedToEdges), not the raw yaml value.
func TestPinPlantTimezone_MainOffersResolvedZone(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, "coreDataService.SetPlantTimezone(www.OfferedPlantTimezone(cfg.Timezone))") {
		t.Error("main.go does not offer Edges www.OfferedPlantTimezone(cfg.Timezone)")
	}
	if n := strings.Count(src, ".SetPlantTimezone("); n != 1 {
		t.Errorf("SetPlantTimezone called %d times in main.go, want 1", n)
	}
}
