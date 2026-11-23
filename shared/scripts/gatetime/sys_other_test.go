//go:build !windows

package main

import (
	"strings"
	"testing"
	"time"
)

// TestOtherMachine_SaysWhatItCannotRead: off Windows there is no standby log
// and no job object, so the stamp reads standby=unread (never a 0 nothing
// measured) and every run carries the unsupported reason.
func TestOtherMachine_SaysWhatItCannotRead(t *testing.T) {
	m := newMachine()
	now := time.Now()
	if got := stamps(m, nil, now.Add(-time.Minute), now, ""); !strings.Contains(got, "standby=unread") {
		t.Errorf("stamp %q, want standby=unread", got)
	}
	if l := m.limits(); len(l) == 0 || !strings.HasPrefix(l[0], "unsupported(") {
		t.Errorf("limits %v, want an unsupported(...) reason", l)
	}
}
