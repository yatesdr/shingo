package main

import (
	"os"
	"strings"
	"testing"
)

// main() cannot run in a test, so this holds the line that tells the feeds what
// the sourcing monitor sends; what the two ends do with it is
// engine TestSourcingFeed_AnEdgeApplyingEveryPushIsNotFlagged.
func TestMain_TheSourcingMonitorTellsTheFeedsWhatItSent(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(b), "sm.SetSentFunc(coreDataService.SourcingSent)") {
		t.Error("main.go does not hand the sourcing monitor coreDataService.SourcingSent: its sends go unrecorded")
	}
}
