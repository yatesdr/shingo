package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// provenance_cycle_test.go — enforcement for the 5.10 constants.
//
// Kept in its own file rather than appended to provenance_test.go so the two
// rounds' enforcement can be read (and merged) separately. The bijection,
// vocabulary, watch-list and SME-exemption tests in that file already cover
// these four constants generically; what is here is the part that is specific
// to them.

// TestCycleFlushIntervalMatchesEdge turns the one STRUCTURAL claim among the
// cycle constants into a check.
//
// display.cycle_flush_interval is not a measurement and not a judgement — it is
// a number that already exists in shingo-edge and is restated in Core because
// Core cannot import it. That makes it exactly the kind of claim the STRUCTURAL
// provenance describes: checkable against the structure it follows from, and off
// the deploy watch list because a plant has nothing to measure.
//
// If Edge retunes its flush cadence and this does not follow, the page silently
// stops flagging the rows whose median is the transport rather than the cell —
// which is the "a naive median measures the flush" error arriving through a
// config file. Same shape as TestRampStepsMatchesTokenSet.
//
// VERIFIED RED BY: changing the default to 4s — the test named both values and
// pointed at the Edge file.
func TestCycleFlushIntervalMatchesEdge(t *testing.T) {
	// shingo-edge is a sibling module; the file is read as DATA, not imported.
	// Core must not depend on Edge, which is the whole reason the number is
	// restated here rather than referenced.
	path := filepath.Join("..", "..", "shingo-edge", "uop", "accumulator.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (this test is the only thing holding cycle_flush_interval to "+
			"the cadence it claims to mirror; if the file moved, repoint it rather than "+
			"deleting the test)", path, err)
	}

	re := regexp.MustCompile(`defaultInventoryDeltaInterval\s*=\s*(\d+)\s*\*\s*time\.(Second|Millisecond|Minute)`)
	m := re.FindSubmatch(src)
	if m == nil {
		t.Fatalf("no defaultInventoryDeltaInterval declaration found in %s — the regex is "+
			"stale, and a silent miss here would leave this test passing on nothing", path)
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("parse %q: %v", m[1], err)
	}
	unit := map[string]time.Duration{
		"Millisecond": time.Millisecond,
		"Second":      time.Second,
		"Minute":      time.Minute,
	}[string(m[2])]
	want := time.Duration(n) * unit

	if got := DisplayDefaults().CycleFlushInterval; got != want {
		t.Errorf("display.cycle_flush_interval is %s but %s declares %s.\n"+
			"These are the same fact written twice and they have diverged. Edge owns the "+
			"cadence; this follows it. A stale value here stops the cycle-time page "+
			"flagging the rows whose median is the transport rather than the cell.",
			got, path, want)
	}
}

// TestValidateRejectsADegenerateSpreadMultiple covers the second load-bearing
// relationship in DisplayConfig.
//
// At k = 1 the tail cut IS the p90, so the tail count becomes 10% of every key's
// sample size by construction — the same number on a healthy line and a stopped
// one. That is the quantile failure mode the spread-based cut exists to avoid,
// and it is reachable by typing a smaller number into a YAML file.
//
// VERIFIED RED BY: dropping the CycleSpreadMultiple clause from Validate — the
// "degenerate" and "below one" cases were both accepted.
func TestValidateRejectsADegenerateSpreadMultiple(t *testing.T) {
	if err := DisplayDefaults().Validate(); err != nil {
		t.Fatalf("shipped defaults do not validate: %v", err)
	}
	for _, tc := range []struct {
		name         string
		k            float64
		wantRejected bool
	}{
		{"degenerate — the cut is the p90", 1, true},
		{"below one — the cut is inside the body", 0.5, true},
		{"zero", 0, true},
		{"just above one", 1.01, false},
		{"shipped", 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := DisplayDefaults()
			d.CycleSpreadMultiple = tc.k
			err := d.Validate()
			if tc.wantRejected && err == nil {
				t.Errorf("k=%g was accepted; at or below 1 the tail count is a fixed share of "+
					"n and says the same thing on every key", tc.k)
			}
			if !tc.wantRejected && err != nil {
				t.Errorf("k=%g was rejected: %v", tc.k, err)
			}
		})
	}
}

// TestCycleConstantsFallBackPerField. YAML unmarshals onto the defaults struct,
// so a display block naming one cycle key and omitting three is the normal case.
//
// VERIFIED RED BY: removing the four fallback clauses from DisplayConstants —
// every omitted field came back zero, and a zero band width renders no
// distribution at all while a zero sample floor withholds nothing.
func TestCycleConstantsFallBackPerField(t *testing.T) {
	def := DisplayDefaults()

	c := Defaults()
	c.Display = DisplayConfig{CycleMinSamples: 40}
	got := c.DisplayConstants()
	if got.CycleMinSamples != 40 {
		t.Errorf("explicit cycle_min_samples was overwritten: got %d, want 40", got.CycleMinSamples)
	}
	if got.CycleSpreadMultiple != def.CycleSpreadMultiple ||
		got.CycleBandWidth != def.CycleBandWidth ||
		got.CycleFlushInterval != def.CycleFlushInterval {
		t.Errorf("omitted cycle fields did not fall back to the shipped defaults: %+v", got)
	}

	// Negative is unset, not clamped. A negative band width would produce no
	// bands at all — "nothing changed" is a visible failure; an empty histogram
	// on every row reads as a distribution nobody has.
	c.Display = DisplayConfig{CycleBandWidth: -1, CycleSpreadMultiple: -2, CycleFlushInterval: -time.Second}
	got = c.DisplayConstants()
	if got.CycleBandWidth != def.CycleBandWidth ||
		got.CycleSpreadMultiple != def.CycleSpreadMultiple ||
		got.CycleFlushInterval != def.CycleFlushInterval {
		t.Errorf("negative values were honoured rather than treated as unset: %+v", got)
	}
}
