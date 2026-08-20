package dispatch

import (
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol/clock"
)

// The threshold these pin is set from plant history, not from taste. Over 120
// days of real tuples with the incident window excluded, normal operation
// produced futile runs of 5, 6, 8, 9 and one of 26 — no knee — while the rates
// separate 60x: ~4/h for the worst legitimate case (ALN_001/76683-6TA0A.06,
// 2026-06-23, 26 terminals over 6.6h) against ~242/h for the 07-21 cascade
// (484 terminals in under two hours).

type recordedAudit struct {
	entityType string
	entityID   int64
	action     string
	newValue   string
	actor      string
}

type fakeAuditor struct {
	mu      sync.Mutex
	entries []recordedAudit
}

func (f *fakeAuditor) AppendAudit(entityType string, entityID int64, action, oldValue, newValue, actor string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, recordedAudit{entityType, entityID, action, newValue, actor})
	return nil
}

func (f *fakeAuditor) all() []recordedAudit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedAudit(nil), f.entries...)
}

func testDetector(t *testing.T, cfg FutilityConfig) (*FutilityDetector, *fakeAuditor, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	aud := &fakeAuditor{}
	d := NewFutilityDetector(cfg, func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, format)
		if len(args) > 0 {
			if s, ok := args[0].(string); ok {
				lines[len(lines)-1] = s
			}
		}
	}, aud)
	return d, aud, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

func defaultCfg() FutilityConfig {
	return FutilityConfig{Enabled: true, Threshold: 20, Window: time.Hour, AlertThrottle: 15 * time.Minute}
}

var testKey = FutilityKey{StationID: "plant-a.line-1", ProcessNode: "ALN_003", PayloadCode: "74577-6SA0A.06"}

func TestFutility_DisabledReturnsNil(t *testing.T) {
	t.Parallel()

	cfg := defaultCfg()
	cfg.Enabled = false
	if d := NewFutilityDetector(cfg, nil, nil); d != nil {
		t.Fatal("disabled config must return nil so callers pay nothing")
	}
	// Every method is nil-safe — that is what lets the call site skip a branch.
	var d *FutilityDetector
	d.NoteInTransit(testKey)
	if d.NoteFutileTerminal(testKey, 1, "cancelled", "") {
		t.Fatal("nil detector must not fire")
	}
	if d.Count(testKey) != 0 {
		t.Fatal("nil detector counts nothing")
	}
}

// The worst legitimate case on record must NOT fire: 26 futile terminals
// spread over 6.6 hours is ~4/h, and the window only ever sees a fraction.
func TestFutility_LegitimateSlowRunDoesNotFire(t *testing.T) {
	t.Parallel()

	clk := clock.NewManual(time.Date(2026, 6, 23, 12, 34, 0, 0, time.UTC))

	d, aud, _ := testDetector(t, defaultCfg())
	d.SetClock(clk.Now)

	// 26 terminals evenly over 6.6h ≈ one every 15m.
	for i := range 26 {
		if d.NoteFutileTerminal(testKey, int64(i), "cancelled", "no_source_bin") {
			t.Fatalf("fired at terminal %d — a 6.6h run of 26 is business as usual", i)
		}
		clk.Advance(15 * time.Minute)
	}
	if n := len(aud.all()); n != 0 {
		t.Fatalf("no audit rows expected, got %d", n)
	}
}

// The cascade must fire: 484 terminals in under two hours is ~242/h.
func TestFutility_CascadeFires(t *testing.T) {
	t.Parallel()

	clk := clock.NewManual(time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC))

	d, aud, lines := testDetector(t, defaultCfg())
	d.SetClock(clk.Now)

	fired := 0
	for i := range 60 {
		if d.NoteFutileTerminal(testKey, int64(1000+i), "skipped", "no_source_bin") {
			fired++
		}
		clk.Advance(15 * time.Second) // 240/h
	}
	if fired == 0 {
		t.Fatal("a 240/h burst must trip the detector")
	}
	entries := aud.all()
	if len(entries) != fired {
		t.Fatalf("audit rows (%d) should match firings (%d)", len(entries), fired)
	}
	if entries[0].action != "futility_rate_exceeded" || entries[0].actor != "system" || entries[0].entityType != "order" {
		t.Fatalf("audit row shape wrong: %+v", entries[0])
	}
	// The record has to name the payload — that is the actionable part, and
	// the whole point of keying on the tuple.
	if !strings.Contains(entries[0].newValue, testKey.PayloadCode) {
		t.Fatalf("audit row does not name the payload: %s", entries[0].newValue)
	}
	if got := lines(); len(got) == 0 || !strings.Contains(got[0], "FUTILITY") {
		t.Fatalf("expected one loud log line, got %v", got)
	}
}

// The window rolls: terminals that age out stop counting, so a slow drip never
// accumulates into a trigger no matter how long it runs.
func TestFutility_WindowRolls(t *testing.T) {
	t.Parallel()

	clk := clock.NewManual(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))

	cfg := defaultCfg()
	cfg.Threshold = 5
	cfg.Window = 10 * time.Minute
	d, _, _ := testDetector(t, cfg)
	d.SetClock(clk.Now)

	for range 4 {
		d.NoteFutileTerminal(testKey, 1, "cancelled", "")
	}
	if got := d.Count(testKey); got != 4 {
		t.Fatalf("count = %d, want 4", got)
	}

	clk.Advance(11 * time.Minute) // everything ages out
	if got := d.Count(testKey); got != 0 {
		t.Fatalf("count after the window passed = %d, want 0", got)
	}
	if d.NoteFutileTerminal(testKey, 2, "cancelled", "") {
		t.Fatal("a single terminal after the window cleared must not fire")
	}
}

// One robot actually departing for this tuple proves the condition cleared.
func TestFutility_InTransitResetsTheTuple(t *testing.T) {
	t.Parallel()

	clk := clock.NewManual(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))

	cfg := defaultCfg()
	cfg.Threshold = 5
	d, _, _ := testDetector(t, cfg)
	d.SetClock(clk.Now)

	for range 4 {
		d.NoteFutileTerminal(testKey, 1, "skipped", "")
	}
	d.NoteInTransit(testKey)
	if got := d.Count(testKey); got != 0 {
		t.Fatalf("in_transit must clear the tuple, count = %d", got)
	}
	for i := range 4 {
		if d.NoteFutileTerminal(testKey, int64(i), "skipped", "") {
			t.Fatal("fired below threshold after a reset")
		}
	}
}

// Tuples are independent — one broken payload must not drag another over.
func TestFutility_TuplesAreIndependent(t *testing.T) {
	t.Parallel()

	clk := clock.NewManual(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))

	cfg := defaultCfg()
	cfg.Threshold = 3
	d, _, _ := testDetector(t, cfg)
	d.SetClock(clk.Now)

	other := FutilityKey{StationID: "plant-a.line-1", ProcessNode: "ALN_001", PayloadCode: "76683-6TA0A.06"}
	for range 2 {
		d.NoteFutileTerminal(testKey, 1, "skipped", "")
		d.NoteFutileTerminal(other, 2, "skipped", "")
	}
	d.NoteInTransit(other) // the other tuple recovers

	if !d.NoteFutileTerminal(testKey, 3, "skipped", "") {
		t.Fatal("the broken tuple should reach its threshold on its own")
	}
	if d.Count(other) != 0 {
		t.Fatal("the recovered tuple must stay clear")
	}
}

// The condition persists, so the record must not repeat per-order — otherwise
// the cascade that motivated this writes 484 audit rows and 484 log lines,
// which is the noise problem the same branch is trying to fix.
func TestFutility_ThrottlesRepeats(t *testing.T) {
	t.Parallel()

	clk := clock.NewManual(time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC))

	cfg := defaultCfg()
	cfg.Threshold = 3
	cfg.AlertThrottle = 15 * time.Minute
	d, aud, _ := testDetector(t, cfg)
	d.SetClock(clk.Now)

	for range 3 {
		d.NoteFutileTerminal(testKey, 1, "skipped", "")
	}
	// 60 more inside the throttle window.
	for i := range 60 {
		if d.NoteFutileTerminal(testKey, int64(i), "skipped", "") {
			t.Fatalf("fired again at %d — repeats inside the throttle must be suppressed", i)
		}
		clk.Advance(5 * time.Second)
	}
	if n := len(aud.all()); n != 1 {
		t.Fatalf("want exactly 1 audit row inside the throttle window, got %d", n)
	}

	clk.Advance(16 * time.Minute)
	if !d.NoteFutileTerminal(testKey, 999, "skipped", "") {
		t.Fatal("must fire again once the throttle expires and the condition persists")
	}
}

// A blank payload or node collapses unrelated work into one bucket. The
// probe's own run analysis had to discard exactly these rows — a blank
// payload produced a spurious run of 41 across orders with nothing in common.
func TestFutility_IgnoresIncompleteTuples(t *testing.T) {
	t.Parallel()

	d, aud, _ := testDetector(t, FutilityConfig{Enabled: true, Threshold: 1, Window: time.Hour, AlertThrottle: time.Minute})

	for _, k := range []FutilityKey{
		{StationID: "", ProcessNode: "ALN_003", PayloadCode: "P"},
		{StationID: "s", ProcessNode: "", PayloadCode: "P"},
		{StationID: "s", ProcessNode: "ALN_003", PayloadCode: ""},
	} {
		if d.NoteFutileTerminal(k, 1, "skipped", "") {
			t.Fatalf("fired on an incomplete tuple: %+v", k)
		}
	}
	if n := len(aud.all()); n != 0 {
		t.Fatalf("incomplete tuples must not produce records, got %d", n)
	}
}
