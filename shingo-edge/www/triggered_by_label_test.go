package www

import "testing"

// TestTriggeredByLabel_EveryValue: every value a completed row carries has
// words in the history panel; none is "--", an unknown one shows as written.
func TestTriggeredByLabel_EveryValue(t *testing.T) {
	for _, v := range []string{"manual", "manual-forced", "auto-catid", "auto-catid-forced", "admin-switch",
		"admin-switch-forced", "auto-catid-flip", "restore", "operator-hmi", "auto-task-terminal"} {
		if l := triggeredByLabel(v); l == "" || l == v || l == "--" {
			t.Errorf("%s: label %q", v, l)
		}
	}
	if l := triggeredByLabel(""); l != "--" {
		t.Errorf("empty: %q, want --", l)
	}
	if l := triggeredByLabel("something-new"); l != "something-new" {
		t.Errorf("unknown: %q, want the value", l)
	}
}
