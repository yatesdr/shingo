package countgroup

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"shingocore/config"
)

// recordingLog captures log messages so we can assert the watchdog's
// output without relying on the stdlib logger or timing.
type recordingLog struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordingLog) Log(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recordingLog) contains(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range r.lines {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}

func (r *recordingLog) lineCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.lines)
}

// TestNeverOccupiedWatchdogFiresWarnThenError drives the watchdog
// deterministically by rewinding the Runner's enabledAt clock past
// the warn then error thresholds. It asserts the watchdog fires
// exactly once at each threshold (not every minute), and that the
// log mentions the group name and the case-sensitivity hint.
func TestNeverOccupiedWatchdogFiresWarnThenError(t *testing.T) {
	rec := &recordingLog{}
	cfg := config.CountGroupsConfig{
		NeverOccupiedWarn:  1 * time.Second,
		NeverOccupiedError: 2 * time.Second,
		Groups:             []config.CountGroupConfig{{Name: "CrosswalkTypo", Enabled: true}},
	}
	r := NewRunner(cfg, nil, nil, rec.Log)

	// Simulate the runner having been active past the warn threshold.
	r.enabledAt = time.Now().Add(-1500 * time.Millisecond)
	r.checkNeverOccupied()
	if !rec.contains("WARN") || !rec.contains("CrosswalkTypo") || !rec.contains("case-sensitive") {
		t.Fatalf("warn-threshold log did not contain expected substrings: %v", rec.lines)
	}
	warnCount := rec.lineCount()

	// Second call within the same threshold window must NOT re-log.
	r.checkNeverOccupied()
	if rec.lineCount() != warnCount {
		t.Fatalf("watchdog logged twice at warn threshold (dedup broken): %v", rec.lines)
	}

	// Now rewind past the error threshold.
	r.enabledAt = time.Now().Add(-2500 * time.Millisecond)
	r.checkNeverOccupied()
	if !rec.contains("ERROR") {
		t.Fatalf("error-threshold log did not fire: %v", rec.lines)
	}
	errorCount := rec.lineCount()

	r.checkNeverOccupied()
	if rec.lineCount() != errorCount {
		t.Fatalf("watchdog re-logged ERROR (dedup broken): %v", rec.lines)
	}
}

// TestNeverOccupiedWatchdogSuppressedAfterNonEmpty verifies that once
// a group has returned occupied at least once, the watchdog stops
// firing — the whole point is to catch groups that NEVER become
// occupied (typo'd names), not groups that happen to be empty right
// now.
func TestNeverOccupiedWatchdogSuppressedAfterNonEmpty(t *testing.T) {
	rec := &recordingLog{}
	cfg := config.CountGroupsConfig{
		NeverOccupiedWarn:  1 * time.Second,
		NeverOccupiedError: 2 * time.Second,
		Groups:             []config.CountGroupConfig{{Name: "Crosswalk1", Enabled: true}},
	}
	r := NewRunner(cfg, nil, nil, rec.Log)
	r.recordNonEmpty("Crosswalk1")

	// Rewind clock well past both thresholds. Watchdog should remain silent.
	r.enabledAt = time.Now().Add(-10 * time.Second)
	r.checkNeverOccupied()
	if rec.lineCount() != 0 {
		t.Fatalf("watchdog fired despite prior non-empty sighting: %v", rec.lines)
	}
}

// TestNeverOccupiedWatchdogSkipsDisabledGroups ensures disabled groups
// don't generate spurious WARN/ERROR noise.
func TestNeverOccupiedWatchdogSkipsDisabledGroups(t *testing.T) {
	rec := &recordingLog{}
	cfg := config.CountGroupsConfig{
		NeverOccupiedWarn:  1 * time.Second,
		NeverOccupiedError: 2 * time.Second,
		Groups:             []config.CountGroupConfig{{Name: "DisabledZone", Enabled: false}},
	}
	r := NewRunner(cfg, nil, nil, rec.Log)
	r.enabledAt = time.Now().Add(-10 * time.Second)
	r.checkNeverOccupied()
	if rec.lineCount() != 0 {
		t.Fatalf("watchdog fired for disabled group: %v", rec.lines)
	}
}
