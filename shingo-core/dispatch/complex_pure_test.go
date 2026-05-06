package dispatch

import (
	"strings"
	"testing"
)

func TestStepSkipSummaries(t *testing.T) {
	t.Parallel()
	skips := []pickupSkip{
		{stepIndex: 0, nodeName: "STOR-A", reason: "no bin"},
		{stepIndex: 2, nodeName: "LINE-B", reason: "claimed"},
	}
	got := stepSkipSummaries(skips)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if want := "step 0 at STOR-A: no bin"; got[0] != want {
		t.Errorf("got[0] = %q, want %q", got[0], want)
	}
	if want := "step 2 at LINE-B: claimed"; got[1] != want {
		t.Errorf("got[1] = %q, want %q", got[1], want)
	}
}

func TestStepSkipSummaries_Empty(t *testing.T) {
	t.Parallel()
	got := stepSkipSummaries(nil)
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestJoinRejects_Short(t *testing.T) {
	t.Parallel()
	rejects := []string{"r1", "r2", "r3"}
	got := joinRejects(rejects)
	if want := "r1; r2; r3"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestJoinRejects_Truncation(t *testing.T) {
	t.Parallel()
	rejects := make([]string, 10)
	for i := range rejects {
		rejects[i] = "reason"
	}
	got := joinRejects(rejects)
	if !strings.Contains(got, "... +4 more") {
		t.Errorf("expected truncation marker in %q", got)
	}
	// Should contain 6 entries + truncation marker
	parts := strings.Split(got, "; ")
	if len(parts) != 7 { // 6 entries + "... +4 more"
		t.Errorf("got %d parts, want 7", len(parts))
	}
}
