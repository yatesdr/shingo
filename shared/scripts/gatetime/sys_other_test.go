//go:build !windows

package main

import (
	"strings"
	"testing"
	"time"
)

// TestOtherMachine_SaysWhatItCannotRead: off Windows there is no standby log
// and no job object, so the stamp reads standby=unread (never a 0 nothing
// measured) and every run carries the unsupported reason. The machine is named,
// not picked from the environment, so the test reads the same on every host.
func TestOtherMachine_SaysWhatItCannotRead(t *testing.T) {
	m := otherMachine{}
	now := time.Now()
	if got := stamps(m, nil, now.Add(-time.Minute), now, ""); !strings.Contains(got, "standby=unread") {
		t.Errorf("stamp %q, want standby=unread", got)
	}
	if l := m.limits(); len(l) == 0 || !strings.HasPrefix(l[0], "unsupported(") {
		t.Errorf("limits %v, want an unsupported(...) reason", l)
	}
}

// TestOtherMachine_RunIsNeverClean: off Windows a run whose samples are quiet
// and on mains power is still VOID, on the unsupported reason: what it cannot
// read is never taken for the absence of a problem.
func TestOtherMachine_RunIsNeverClean(t *testing.T) {
	ss := samplesEvery(time.Now(), 3*time.Second, repeat(10, [3]float64{2, 2, 0}))
	j := judge(ss, nil, 15*time.Second, nil, otherMachine{}.limits())
	if j.verdict() == "clean" || !hasReason(j, "unsupported(") {
		t.Errorf("a quiet run off Windows: %q, want VOID with unsupported(...)", j.verdict())
	}
}
