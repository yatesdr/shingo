// Command shardplan splits the docker-tagged test packages across N CI shards
// and learns from each run how long they actually take.
//
// WHY THIS EXISTS. shingo-core's -race docker suite is the long pole of CI:
// 322s of a 395s workflow, against 137s for the same suite without the flag.
// That is the race detector's usual 5-20x on memory access, and it buys a
// guarantee worth keeping for software that drives AMRs around a plant — a
// data race cannot reach main. So it is made cheaper by running the packages
// side by side, not by running it less often.
//
// WHY NOT A HARDCODED PACKAGE LIST. It would be wrong within a month, and a
// drifted list does not fail loudly — it quietly stops testing whatever fell
// out of it. So the package list is re-derived from `go list` every run, and
// the only thing carried between runs is TIMINGS, which self-correct.
//
// WHY TIMINGS AND NOT A CHEAPER PROXY. Measured against a real run of the 49
// packages that carry tests (132s serial), splitting six ways:
//
//	round-robin (alphabetical)   slowest shard 36.6s
//	by test count (stateless)    slowest shard 38.8s
//	by measured duration         slowest shard 22.1s   (ideal 22.0s)
//
// The distribution is far too skewed for anything simpler — the top seven
// packages are ~100s of the 132s, so round-robin hands one shard 36.6s and
// another 3.5s. Only real durations balance it.
//
// THE FLOOR IS THE LONGEST PACKAGE. shingocore/engine alone is ~20s, so no
// value of N puts a shard below that; past N=8 you pay job startup for
// nothing.
//
// Usage:
//
//	go list -tags=docker ./... | go run ./scripts/shardplan plan -shards 6 -timings f.json
//	go list -tags=docker ./... | go run ./scripts/shardplan record -timings f.json out1.log out2.log
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// defaultWeight is what a package with no recorded timing is assumed to cost:
// a new package, or one whose entry aged out of the cache. Deliberately ABOVE
// the median. A new package that turns out to be slow costs one unbalanced
// run, whereas underestimating it makes it the straggler on a shard that was
// already full. Either way `record` corrects it on the very next run.
const defaultWeight = 5.0

// Shard is one element of the matrix handed to GitHub Actions. Pkgs is
// space-joined because that is how it reaches `go test`.
type Shard struct {
	ID   int    `json:"id"`
	Pkgs string `json:"pkgs"`
}

func main() {
	if len(os.Args) < 2 {
		fatal("usage: shardplan plan|record [flags] [files...]")
	}
	switch os.Args[1] {
	case "plan":
		cmdPlan(os.Args[2:])
	case "record":
		cmdRecord(os.Args[2:])
	default:
		fatal("shardplan: unknown command %q (want plan or record)", os.Args[1])
	}
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

// loadTimings reads the map written by a previous run. Missing or unreadable
// is NORMAL rather than an error — the first run ever, an expired cache, a
// half-downloaded artifact — and the caller then falls back to defaultWeight
// for everything, which costs balance and nothing else.
func loadTimings(path string) map[string]float64 {
	out := map[string]float64{}
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "shardplan: no usable timings at %s (%v); using equal weights\n", path, err)
		return out
	}
	if err := json.Unmarshal(b, &out); err != nil {
		fmt.Fprintf(os.Stderr, "shardplan: timings at %s are unreadable (%v); using equal weights\n", path, err)
		return map[string]float64{}
	}
	return out
}

func weight(timings map[string]float64, pkg string) float64 {
	if d, ok := timings[pkg]; ok {
		return d
	}
	return defaultWeight
}

// pack does greedy longest-first bin packing: repeatedly place the heaviest
// remaining package on the lightest shard. Cheap, and within a few percent of
// optimal on a distribution like this one.
//
// THE ORDER IS TOTAL, ties included (heaviest first, then by name), so the
// same inputs always produce the same partition. That matters more than it
// looks: the plan is computed once and handed to every shard, and a partition
// that could vary between readers is one that could drop a package.
func pack(packages []string, timings map[string]float64, shards int) ([][]string, []float64) {
	ordered := append([]string(nil), packages...)
	sort.Slice(ordered, func(i, j int) bool {
		wi, wj := weight(timings, ordered[i]), weight(timings, ordered[j])
		if wi != wj {
			return wi > wj
		}
		return ordered[i] < ordered[j]
	})

	bins := make([][]string, shards)
	loads := make([]float64, shards)
	for _, pkg := range ordered {
		lightest := 0
		for i := range loads {
			if loads[i] < loads[lightest] {
				lightest = i
			}
		}
		bins[lightest] = append(bins[lightest], pkg)
		loads[lightest] += weight(timings, pkg)
	}
	return bins, loads
}

// verifyPartition is THE ONE CHECK THAT IS NOT BEST-EFFORT.
//
// Everything else here degrades gracefully — lose the timings and the balance
// gets worse, nothing more. But if the packer ever drops a package, its tests
// stop running, CI stays green, and nothing says so. That is the failure class
// core.yml's fetch-depth note already calls out: a gate that goes green while
// asserting nothing is worse than no gate at all.
func verifyPartition(packages []string, bins [][]string) error {
	placed := map[string]int{}
	total := 0
	for _, b := range bins {
		for _, p := range b {
			placed[p]++
			total++
		}
	}
	var missing, dup []string
	for _, p := range packages {
		switch placed[p] {
		case 1:
		case 0:
			missing = append(missing, p)
		default:
			dup = append(dup, p)
		}
	}
	var extra []string
	want := map[string]bool{}
	for _, p := range packages {
		want[p] = true
	}
	for p := range placed {
		if !want[p] {
			extra = append(extra, p)
		}
	}
	if len(missing) > 0 || len(dup) > 0 || len(extra) > 0 || total != len(packages) {
		sort.Strings(missing)
		sort.Strings(dup)
		sort.Strings(extra)
		return fmt.Errorf("partition does not cover the package set exactly "+
			"(missing=%v duplicated=%v unknown=%v placed=%d want=%d)",
			missing, dup, extra, total, len(packages))
	}
	return nil
}

func readLines(f *os.File) []string {
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		if s := strings.TrimSpace(sc.Text()); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func cmdPlan(args []string) {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	shards := fs.Int("shards", 0, "number of shards (required)")
	timingsPath := fs.String("timings", ".shard/timings.json", "timings file from a previous run")
	_ = fs.Parse(args)
	if *shards < 1 {
		fatal("shardplan plan: -shards must be >= 1")
	}

	packages := readLines(os.Stdin)
	if len(packages) == 0 {
		fatal("shardplan plan: no packages on stdin (did `go list` fail?)")
	}

	timings := loadTimings(*timingsPath)
	bins, loads := pack(packages, timings, *shards)
	if err := verifyPartition(packages, bins); err != nil {
		fatal("shardplan plan: %v", err)
	}

	known := 0
	for _, p := range packages {
		if _, ok := timings[p]; ok {
			known++
		}
	}
	slowest, lightest := loads[0], loads[0]
	for _, l := range loads {
		if l > slowest {
			slowest = l
		}
		if l < lightest {
			lightest = l
		}
	}
	fmt.Fprintf(os.Stderr, "shardplan: %d packages (%d with recorded timings) into %d shards; "+
		"predicted slowest %.1fs, lightest %.1fs\n", len(packages), known, *shards, slowest, lightest)
	for i, b := range bins {
		fmt.Fprintf(os.Stderr, "  shard %d: %2d pkgs, ~%5.1fs\n", i, len(b), loads[i])
	}

	matrix := make([]Shard, len(bins))
	for i, b := range bins {
		matrix[i] = Shard{ID: i, Pkgs: strings.Join(b, " ")}
	}
	enc, err := json.Marshal(matrix)
	if err != nil {
		fatal("shardplan plan: marshal matrix: %v", err)
	}
	fmt.Println(string(enc))
}

// Plain `go test` output. -json is NOT used by the workflow, deliberately: a
// race report is a multi-page stack dump, and wrapping it in JSON would make
// it unreadable on the one run where somebody has to read it. Both forms are
// accepted here anyway, so the choice stays the workflow's.
//
//	ok      shingocore/store/bins   2.591s
//	FAIL    shingocore/dispatch     12.3s
//	?       shingocore/cmd/x        [no test files]
var (
	rePkgTime = regexp.MustCompile(`^(?:ok|FAIL)\s+(\S+)\s+([0-9.]+)s`)
	reNoTests = regexp.MustCompile(`^\?\s+(\S+)\s+\[no test files\]`)
)

type testEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
}

func cmdRecord(args []string) {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	timingsPath := fs.String("timings", ".shard/timings.json", "timings file to merge into and rewrite")
	_ = fs.Parse(args)

	current := map[string]bool{}
	for _, p := range readLines(os.Stdin) {
		current[p] = true
	}
	if len(current) == 0 {
		fatal("shardplan record: no packages on stdin (did `go list` fail?)")
	}

	merged := loadTimings(*timingsPath)
	seen := 0
	for _, path := range fs.Args() {
		f, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "shardplan: skipping %s (%v)\n", path, err)
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if pkg, d, ok := parseLine(line); ok {
				merged[pkg] = d
				seen++
			}
		}
		f.Close()
	}

	// PRUNED TO THE CURRENT PACKAGE LIST, which stops the file growing forever
	// and keeps deleted packages from haunting the balance.
	pruned := map[string]float64{}
	for k, v := range merged {
		if current[k] {
			pruned[k] = v
		}
	}
	if err := os.MkdirAll(filepath.Dir(*timingsPath), 0o755); err != nil {
		fatal("shardplan record: %v", err)
	}
	out, err := json.MarshalIndent(pruned, "", " ")
	if err != nil {
		fatal("shardplan record: marshal: %v", err)
	}
	if err := os.WriteFile(*timingsPath, append(out, '\n'), 0o644); err != nil {
		fatal("shardplan record: %v", err)
	}
	fmt.Fprintf(os.Stderr, "shardplan: recorded %d package results, %d timings kept, %d stale pruned\n",
		seen, len(pruned), len(merged)-len(pruned))
}

// parseLine pulls a package duration out of one line of `go test` output, in
// either the plain or the -json form.
func parseLine(line string) (string, float64, bool) {
	if strings.HasPrefix(line, "{") {
		var ev testEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			return "", 0, false
		}
		if ev.Test != "" || ev.Package == "" {
			return "", 0, false // per-test event, not the package total
		}
		// "skip" IS RECORDED, not ignored, and that is not a detail: a package
		// with no test files reports skip and never pass, so ignoring it would
		// pin every such package to defaultWeight forever. `go list ./...`
		// returns 59 packages here against 49 that run tests — that would be
		// ~50s of phantom weight the packer carries permanently.
		switch ev.Action {
		case "pass", "fail", "skip":
			return ev.Package, ev.Elapsed, true
		}
		return "", 0, false
	}
	if m := rePkgTime.FindStringSubmatch(line); m != nil {
		d, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			return "", 0, false
		}
		return m[1], d, true
	}
	if m := reNoTests.FindStringSubmatch(line); m != nil {
		return m[1], 0, true // same reasoning as "skip" above
	}
	return "", 0, false
}
