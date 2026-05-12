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

// TestAllStepSkipsAreEmptyNode pins the gate that separates "skip the
// order, the work was never needed" from "fail the order, bins are
// available but unclaimable". The string match on emptyNodeSkipReason
// is the contract; this test guards against drift.
func TestAllStepSkipsAreEmptyNode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []pickupSkip
		want bool
	}{
		{"all empty", []pickupSkip{
			{0, "STOR-A", emptyNodeSkipReason},
			{1, "STOR-B", emptyNodeSkipReason},
		}, true},
		{"one empty, one rejected", []pickupSkip{
			{0, "STOR-A", emptyNodeSkipReason},
			{1, "STOR-B", "no candidate among 2 bin(s); rejects: [already claimed by order 99]"},
		}, false},
		{"none empty", []pickupSkip{
			{0, "STOR-A", "no candidate among 1 bin(s); rejects: [payload mismatch]"},
		}, false},
		{"empty input — zero pickup steps is not a skip case", nil, false},
	}
	for _, c := range cases {
		if got := allStepSkipsAreEmptyNode(c.in); got != c.want {
			t.Errorf("%s: allStepSkipsAreEmptyNode = %v, want %v", c.name, got, c.want)
		}
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
