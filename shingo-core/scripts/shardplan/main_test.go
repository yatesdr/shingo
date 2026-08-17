package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// The real distribution, from a measured `-p 1` run of the docker suite. Kept
// as data rather than invented numbers because the whole design rests on this
// shape being skewed: the top handful of packages are most of the wall clock,
// which is exactly why round-robin does not work and bin packing does.
var realTimings = map[string]float64{
	"shingocore/engine":              20.234,
	"shingocore/dispatch":            17.406,
	"shingocore/internal/schemadump": 17.041,
	"shingocore/www":                 13.927,
	"shingocore/store":               12.058,
	"shingocore/service":             9.697,
	"shingocore/cmd/seeddev":         9.696,
	"shingocore/internal/testdb":     5.320,
	"shingocore/messaging":           4.281,
	"shingocore/store/bins":          2.591,
	"shingocore/store/sourceability": 2.260,
	"shingocore/store/orders":        1.556,
	"shingocore/store/nodes":         0.9,
	"shingocore/store/payloads":      0.8,
	"shingocore/store/admin":         0.4,
	"shingocore/store/audit":         0.3,
}

func keys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func loadOf(bin []string, timings map[string]float64) float64 {
	var t float64
	for _, p := range bin {
		t += weight(timings, p)
	}
	return t
}

func TestPack_CoversEveryPackageExactlyOnce(t *testing.T) {
	pkgs := keys(realTimings)
	for _, n := range []int{1, 2, 4, 6, 8, 17} {
		bins, _ := pack(pkgs, realTimings, n)
		if err := verifyPartition(pkgs, bins); err != nil {
			t.Errorf("shards=%d: %v", n, err)
		}
	}
}

func TestPack_IsDeterministic(t *testing.T) {
	pkgs := keys(realTimings)
	first, _ := pack(pkgs, realTimings, 6)
	for i := 0; i < 20; i++ {
		// Shuffle the input order: the partition must depend on the SET and
		// the timings, never on the order `go list` happened to emit. Every
		// shard is handed the same plan, so an order-sensitive packer would
		// silently run some packages twice and others never.
		shuffled := append([]string(nil), pkgs...)
		shuffled[i%len(shuffled)], shuffled[0] = shuffled[0], shuffled[i%len(shuffled)]
		got, _ := pack(shuffled, realTimings, 6)
		for b := range first {
			if strings.Join(first[b], " ") != strings.Join(got[b], " ") {
				t.Fatalf("iteration %d: shard %d differs\n first=%v\n got  =%v", i, b, first[b], got[b])
			}
		}
	}
}

func TestPack_BeatsRoundRobinOnTheRealDistribution(t *testing.T) {
	pkgs := keys(realTimings)
	const n = 6

	bins, _ := pack(pkgs, realTimings, n)
	var packed float64
	for _, b := range bins {
		if l := loadOf(b, realTimings); l > packed {
			packed = l
		}
	}

	// Round-robin over the alphabetical list — i.e. `awk NR % N == i`, the
	// obvious shell one-liner this design deliberately rejects.
	rr := make([][]string, n)
	for i, p := range pkgs {
		rr[i%n] = append(rr[i%n], p)
	}
	var roundRobin float64
	for _, b := range rr {
		if l := loadOf(b, realTimings); l > roundRobin {
			roundRobin = l
		}
	}

	var total float64
	for _, d := range realTimings {
		total += d
	}
	ideal := total / n

	t.Logf("ideal %.1fs | bin-packed %.1fs | round-robin %.1fs", ideal, packed, roundRobin)
	if packed >= roundRobin {
		t.Errorf("bin packing (%.1fs) did not beat round-robin (%.1fs)", packed, roundRobin)
	}
	// The floor is the single longest package; anything near it is as good as
	// packing can get without splitting a package.
	if packed > realTimings["shingocore/engine"]*1.15 {
		t.Errorf("slowest shard %.1fs is well above the longest package (%.1fs)",
			packed, realTimings["shingocore/engine"])
	}
}

func TestPack_UnknownPackagesGetTheDefaultAndAreStillPlaced(t *testing.T) {
	pkgs := append(keys(realTimings), "shingocore/brand/new", "shingocore/also/new")
	bins, _ := pack(pkgs, realTimings, 6)
	if err := verifyPartition(pkgs, bins); err != nil {
		t.Fatalf("a package with no recorded timing must still be scheduled: %v", err)
	}
}

func TestVerifyPartition_CatchesADroppedPackage(t *testing.T) {
	pkgs := keys(realTimings)
	bins, _ := pack(pkgs, realTimings, 4)
	bins[0] = bins[0][1:] // lose one, the way a packer bug would

	err := verifyPartition(pkgs, bins)
	if err == nil {
		t.Fatal("dropping a package must be an error — a silent drop is a suite that stops running while CI stays green")
	}
	if !strings.Contains(err.Error(), "missing=") {
		t.Errorf("error should name what went missing, got: %v", err)
	}
}

func TestVerifyPartition_CatchesADuplicatedPackage(t *testing.T) {
	pkgs := keys(realTimings)
	bins, _ := pack(pkgs, realTimings, 4)
	bins[1] = append(bins[1], bins[0][0]) // now run twice, and one shard pays for it

	if err := verifyPartition(pkgs, bins); err == nil {
		t.Fatal("a package placed on two shards must be an error")
	}
}

func TestParseLine(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		wantPkg string
		wantDur float64
		wantOK  bool
	}{
		{"plain ok", "ok  \tshingocore/store/bins\t2.591s", "shingocore/store/bins", 2.591, true},
		{"plain fail", "FAIL\tshingocore/dispatch\t12.300s", "shingocore/dispatch", 12.3, true},
		{"plain ok with suffix", "ok  \tshingocore/store/admin\t0.113s [no tests to run]", "shingocore/store/admin", 0.113, true},
		// Recorded as zero rather than ignored: these packages never report
		// "pass", so ignoring them would pin them to defaultWeight forever.
		{"no test files", "?   \tshingocore/cmd/migrateloaders\t[no test files]", "shingocore/cmd/migrateloaders", 0, true},
		{"json pass", `{"Action":"pass","Package":"shingocore/uop","Elapsed":3.5}`, "shingocore/uop", 3.5, true},
		{"json skip", `{"Action":"skip","Package":"shingocore/dispatch/eta","Elapsed":0.001}`, "shingocore/dispatch/eta", 0.001, true},
		{"json per-test event ignored", `{"Action":"pass","Package":"shingocore/uop","Test":"TestX","Elapsed":0.1}`, "", 0, false},
		{"json run event ignored", `{"Action":"run","Package":"shingocore/uop"}`, "", 0, false},
		{"noise", "=== RUN   TestSomething", "", 0, false},
		{"empty", "", "", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pkg, dur, ok := parseLine(c.line)
			if ok != c.wantOK || pkg != c.wantPkg || fmt.Sprintf("%.3f", dur) != fmt.Sprintf("%.3f", c.wantDur) {
				t.Errorf("parseLine(%q) = (%q, %v, %v), want (%q, %v, %v)",
					c.line, pkg, dur, ok, c.wantPkg, c.wantDur, c.wantOK)
			}
		})
	}
}
