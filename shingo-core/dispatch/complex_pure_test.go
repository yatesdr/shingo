package dispatch

import (
	"strings"
	"testing"
)

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
