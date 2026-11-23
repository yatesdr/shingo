package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestRecordReport reads recorder directories (RecordEnv runs) and prints, with
// -v:
//
//  1. per directory, the cells whose normalised transcripts are equal (one
//     sha256, two or more cells): the removal-candidate evidence;
//  2. across directories (runs), each cell whose transcript differs from run
//     to run, with the first differing line of the first two that differ;
//  3. per test, summed over its cells and runs: bins pushes by caller and the
//     ticker's share, the waits' passes, how many satisfying passes followed a
//     Core robot poll, and the notes (TriggeredBy).
//
// Skipped unless SHINGO_SCENARIO_RECORD_REPORT names the directories, comma
// separated:
//
//	SHINGO_SCENARIO_RECORD_REPORT=/tmp/rec/run-1,/tmp/rec/run-2 \
//	  go test -v -count=1 -run TestRecordReport ./harness/
func TestRecordReport(t *testing.T) {
	spec := os.Getenv("SHINGO_SCENARIO_RECORD_REPORT")
	if spec == "" {
		t.Skip("SHINGO_SCENARIO_RECORD_REPORT names no recorder directories")
	}
	var runs []recordRun
	for _, dir := range strings.Split(spec, ",") {
		run, err := readRecordRun(strings.TrimSpace(dir))
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
	}
	var b strings.Builder
	writeEqualCells(&b, runs)
	writeRunDiffs(&b, runs)
	writeCounts(&b, runs)
	fmt.Print(b.String())
}

// TestRecordRenormalise rebuilds each cell's transcript, and the digest and
// message count in its counts, from its .raw file, with this tree's
// normaliser: a record taken before a normaliser change reads as one taken
// after it. Skipped unless SHINGO_SCENARIO_RECORD_RENORM names the
// directories, comma separated.
func TestRecordRenormalise(t *testing.T) {
	spec := os.Getenv("SHINGO_SCENARIO_RECORD_RENORM")
	if spec == "" {
		t.Skip("SHINGO_SCENARIO_RECORD_RENORM names no recorder directories")
	}
	for _, dir := range strings.Split(spec, ",") {
		raws, err := filepath.Glob(filepath.Join(strings.TrimSpace(dir), "*.raw"))
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range raws {
			if err := renormalise(strings.TrimSuffix(raw, ".raw")); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("%s: %d cells renormalised", dir, len(raws))
	}
}

func renormalise(base string) error {
	body, err := os.ReadFile(base + ".raw")
	if err != nil {
		return err
	}
	n := newNormaliser()
	var lines []string
	for _, l := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
		if l == "" {
			continue
		}
		bus, rest, _ := strings.Cut(l, " ")
		dir, payload, _ := strings.Cut(rest, " ")
		lines = append(lines, fmt.Sprintf("%s %s %s", bus, dir, n.envelope([]byte(payload))))
	}
	transcript := strings.Join(lines, "\n")
	if len(lines) > 0 {
		transcript += "\n"
	}
	cb, err := os.ReadFile(base + ".counts.json")
	if err != nil {
		return err
	}
	var c recordCounts
	if err := json.Unmarshal(cb, &c); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(transcript))
	c.Transcript, c.Messages = hex.EncodeToString(sum[:]), len(lines)
	out, err := json.MarshalIndent(c, "", " ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(base+".transcript", []byte(transcript), 0o644); err != nil {
		return err
	}
	return os.WriteFile(base+".counts.json", append(out, '\n'), 0o644)
}

// recordRun is one recorder directory: its cells by name.
type recordRun struct {
	dir   string
	cells map[string]*recordCell
}

type recordCell struct {
	file   string // the cell's file base, in its directory
	counts recordCounts
}

func readRecordRun(dir string) (recordRun, error) {
	run := recordRun{dir: dir, cells: map[string]*recordCell{}}
	files, err := filepath.Glob(filepath.Join(dir, "*.counts.json"))
	if err != nil {
		return run, err
	}
	if len(files) == 0 {
		return run, fmt.Errorf("%s: no *.counts.json: not a recorder directory, or the run recorded nothing", dir)
	}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			return run, err
		}
		c := &recordCell{file: strings.TrimSuffix(f, ".counts.json")}
		if err := json.Unmarshal(body, &c.counts); err != nil {
			return run, fmt.Errorf("%s: %w", f, err)
		}
		name := c.counts.Cell
		if _, dup := run.cells[name]; dup {
			// A second record of one cell in one run (-count=2): its own key.
			name = filepath.Base(c.file)
		}
		run.cells[name] = c
	}
	return run, nil
}

func sortedCells(run recordRun) []string {
	names := make([]string, 0, len(run.cells))
	for n := range run.cells {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// writeEqualCells: per run, the groups of two or more cells with one
// transcript. A cell with an empty transcript (it never pumped a message) is
// grouped too, and says so.
func writeEqualCells(b *strings.Builder, runs []recordRun) {
	fmt.Fprintf(b, "## Cells with equal transcripts\n\n")
	for _, run := range runs {
		groups := map[string][]string{}
		for _, n := range sortedCells(run) {
			sha := run.cells[n].counts.Transcript
			groups[sha] = append(groups[sha], n)
		}
		var shas []string
		for sha, g := range groups {
			if len(g) > 1 {
				shas = append(shas, sha)
			}
		}
		sort.Slice(shas, func(i, j int) bool { return groups[shas[i]][0] < groups[shas[j]][0] })
		fmt.Fprintf(b, "%s: %d cells, %d groups of equal transcripts\n", run.dir, len(run.cells), len(shas))
		for _, sha := range shas {
			c := run.cells[groups[sha][0]].counts
			fmt.Fprintf(b, "  %s (%d messages):\n", sha[:12], c.Messages)
			for _, n := range groups[sha] {
				fmt.Fprintf(b, "    %s\n", n)
			}
		}
	}
	fmt.Fprintln(b)
}

// writeRunDiffs: each cell recorded in two or more runs, and whether its
// transcripts agree.
func writeRunDiffs(b *strings.Builder, runs []recordRun) {
	if len(runs) < 2 {
		return
	}
	fmt.Fprintf(b, "## Same cell, run to run (%d runs)\n\n", len(runs))
	all := map[string]bool{}
	for _, run := range runs {
		for n := range run.cells {
			all[n] = true
		}
	}
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	same, differ := 0, 0
	for _, n := range names {
		shas := map[string][]int{}
		var in []int
		for i, run := range runs {
			if c, ok := run.cells[n]; ok {
				shas[c.counts.Transcript] = append(shas[c.counts.Transcript], i)
				in = append(in, i)
			}
		}
		if len(in) < 2 {
			continue
		}
		if len(shas) == 1 {
			same++
			continue
		}
		differ++
		var firsts []int
		for _, idx := range shas {
			firsts = append(firsts, idx[0])
		}
		sort.Ints(firsts)
		fmt.Fprintf(b, "DIFFERS %s: %d transcripts over %d runs\n", n, len(shas), len(in))
		a, z := runs[firsts[0]].cells[n], runs[firsts[1]].cells[n]
		fmt.Fprintf(b, "  %s\n", firstDiff(a.file+".transcript", z.file+".transcript"))
	}
	fmt.Fprintf(b, "\n%d cells identical in every run, %d differ\n\n", same, differ)
}

// firstDiff names the first line two transcript files differ at.
func firstDiff(a, z string) string {
	ab, err1 := os.ReadFile(a)
	zb, err2 := os.ReadFile(z)
	if err1 != nil || err2 != nil {
		return fmt.Sprintf("unreadable: %v %v", err1, err2)
	}
	al, zl := strings.Split(string(ab), "\n"), strings.Split(string(zb), "\n")
	for i := 0; i < len(al) || i < len(zl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(zl) {
			y = zl[i]
		}
		if x != y {
			return fmt.Sprintf("line %d (of %d / %d):\n    %s: %s\n    %s: %s", i+1, len(al), len(zl),
				filepath.Base(filepath.Dir(a)), clip(x), filepath.Base(filepath.Dir(z)), clip(y))
		}
	}
	return "equal bytes (the digests disagree with the files)"
}

func clip(s string) string {
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}

// writeCounts: per test (the part of a cell's name before its first "/"),
// summed over its cells and every run.
func writeCounts(b *strings.Builder, runs []recordRun) {
	type agg struct {
		cells, failed int
		pushes        map[string]*pushCount
		waits         map[string]*waitAgg
		notes         map[string]map[string]int
		counts        map[string]int64
	}
	tests := map[string]*agg{}
	for _, run := range runs {
		for _, n := range sortedCells(run) {
			c := run.cells[n].counts
			test, _, _ := strings.Cut(c.Cell, "/")
			a := tests[test]
			if a == nil {
				a = &agg{pushes: map[string]*pushCount{}, waits: map[string]*waitAgg{},
					notes: map[string]map[string]int{}, counts: map[string]int64{}}
				tests[test] = a
			}
			a.cells++
			if c.Failed {
				a.failed++
			}
			for caller, p := range c.Pushes {
				q := a.pushes[caller]
				if q == nil {
					q = &pushCount{}
					a.pushes[caller] = q
				}
				q.N += p.N
				q.Changed += p.Changed
				q.WallNS += p.WallNS
			}
			for _, w := range c.Waits {
				wa := a.waits[w.Kind]
				if wa == nil {
					wa = &waitAgg{hist: map[int]int{}}
					a.waits[w.Kind] = wa
				}
				wa.add(w)
			}
			for k, vs := range c.Notes {
				if a.notes[k] == nil {
					a.notes[k] = map[string]int{}
				}
				for _, v := range vs {
					a.notes[k][fmt.Sprintf("%q", v)]++
				}
			}
			for k, v := range c.Counts {
				a.counts[k] += v
			}
		}
	}
	names := make([]string, 0, len(tests))
	for n := range tests {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintf(b, "## Counts by test, summed over cells and runs\n\n")
	for _, n := range names {
		a := tests[n]
		var total, changed int64
		for _, p := range a.pushes {
			total += p.N
			changed += p.Changed
		}
		fmt.Fprintf(b, "### %s: %d cell-runs (%d failed)\n\n", n, a.cells, a.failed)
		fmt.Fprintf(b, "| push caller | pushes | share | changed the copy | push wall s |\n|---|---|---|---|---|\n")
		callers := make([]string, 0, len(a.pushes))
		for c := range a.pushes {
			callers = append(callers, c)
		}
		sort.Strings(callers)
		for _, c := range callers {
			p := a.pushes[c]
			fmt.Fprintf(b, "| %s | %d | %.1f%% | %d | %.1f |\n", c, p.N, pct(p.N, total), p.Changed,
				time.Duration(p.WallNS).Seconds())
		}
		fmt.Fprintf(b, "| all | %d | | %d | |\n\n", total, changed)
		fmt.Fprintf(b, "| wait | waits | met | passes | mean | 1 pass | 2 | 3-5 | 6+ | met after a robot poll (observed / inferred) |\n")
		fmt.Fprintf(b, "|---|---|---|---|---|---|---|---|---|---|\n")
		kinds := make([]string, 0, len(a.waits))
		for k := range a.waits {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		for _, k := range kinds {
			w := a.waits[k]
			fmt.Fprintf(b, "| %s | %d | %d | %d | %.2f | %d | %d | %d | %d | %d / %d |\n", k, w.n, w.met, w.passes,
				float64(w.passes)/float64(max(w.n, 1)), w.hist[1], w.hist[2], w.hist[3], w.hist[6],
				w.afterObserved, w.afterInferred)
		}
		fmt.Fprintln(b)
		if len(a.counts) > 0 {
			keys := make([]string, 0, len(a.counts))
			for k := range a.counts {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			fmt.Fprintf(b, "counters:")
			for _, k := range keys {
				fmt.Fprintf(b, " %s=%d", k, a.counts[k])
			}
			fmt.Fprintln(b)
		}
		for k, vs := range a.notes {
			fmt.Fprintf(b, "note %s: %v\n", k, vs)
		}
		fmt.Fprintln(b)
	}
}

type waitAgg struct {
	n, met, passes               int
	hist                         map[int]int // 1, 2, 3 (3-5), 6 (6+)
	afterObserved, afterInferred int
}

func (w *waitAgg) add(x *Wait) {
	w.n++
	w.passes += x.Passes
	switch {
	case x.Passes <= 1:
		w.hist[1]++
	case x.Passes == 2:
		w.hist[2]++
	case x.Passes <= 5:
		w.hist[3]++
	default:
		w.hist[6]++
	}
	if !x.Met {
		return
	}
	w.met++
	if x.AfterPoll {
		switch x.PollSource {
		case "observed":
			w.afterObserved++
		case "inferred":
			w.afterInferred++
		}
	}
}

func pct(n, total int64) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(n) / float64(total)
}
