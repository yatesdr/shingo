package harness

import (
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
	for _, v := range recordViewKinds {
		writeEqualCells(&b, runs, v)
		writeRunDiffs(&b, runs, v)
	}
	writeCounts(&b, runs)
	fmt.Print(b.String())
}

// TestRecordCompare compares two bands of runs, A (before a change) and B
// (after), cell by cell, and prints with -v:
//
//   - each cell with a B transcript that no A run of it had (or the reverse),
//     with the first line it differs from A's at: a change the transcript sees;
//   - each cell whose A runs or B runs disagree among themselves (noise, named
//     so it is not read as the change);
//   - the waits, matched by their order in the cell and their label: ticks
//     summed per kind in A and in B, and how many waits B met with no tick (0
//     passes: the condition held at entry);
//   - verdicts that differ (a cell failed in one band and not the other).
//
// Skipped unless SHINGO_SCENARIO_RECORD_COMPARE is "a1,a2,...|b1,b2,...".
func TestRecordCompare(t *testing.T) {
	spec := os.Getenv("SHINGO_SCENARIO_RECORD_COMPARE")
	if spec == "" {
		t.Skip("SHINGO_SCENARIO_RECORD_COMPARE names no bands")
	}
	as, bs, ok := strings.Cut(spec, "|")
	if !ok {
		t.Fatal(`SHINGO_SCENARIO_RECORD_COMPARE is "a1,a2,...|b1,b2,..."`)
	}
	read := func(list string) []recordRun {
		var runs []recordRun
		for _, d := range strings.Split(list, ",") {
			run, err := readRecordRun(strings.TrimSpace(d))
			if err != nil {
				t.Fatal(err)
			}
			runs = append(runs, run)
		}
		return runs
	}
	var b strings.Builder
	ra, rb := read(as), read(bs)
	for i, v := range recordViewKinds {
		compareBands(t, &b, ra, rb, v, i == 0)
	}
	fmt.Print(b.String())
}

func compareBands(t *testing.T, b *strings.Builder, a, z []recordRun, v recordView, withWaits bool) {
	band := func(runs []recordRun, cell string) (shas map[string]*recordCell, cells []*recordCell) {
		shas = map[string]*recordCell{}
		for _, r := range runs {
			if c, ok := r.cells[cell]; ok {
				if _, seen := shas[v.sha(c.counts)]; !seen {
					shas[v.sha(c.counts)] = c
				}
				cells = append(cells, c)
			}
		}
		return shas, cells
	}
	all := map[string]bool{}
	for _, r := range append(append([]recordRun{}, a...), z...) {
		for n := range r.cells {
			all[n] = true
		}
	}
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	type tally struct{ waits, ticksA, ticksB, zeroB, lessB, moreB, sameB int }
	kinds := map[string]*tally{}
	var changed, noisyA, noisyB, verdicts, missing, waitShape int
	fmt.Fprintf(b, "## A (%d runs) against B (%d runs): %s\n\n", len(a), len(z), v.name)
	for _, n := range names {
		sa, ca := band(a, n)
		sb, cb := band(z, n)
		if len(ca) == 0 || len(cb) == 0 {
			missing++
			fmt.Fprintf(b, "ONLY IN ONE BAND %s (A %d runs, B %d)\n", n, len(ca), len(cb))
			continue
		}
		if len(sa) > 1 {
			noisyA++
			fmt.Fprintf(b, "NOISE A %s: %d transcripts over %d runs\n", n, len(sa), len(ca))
		}
		if len(sb) > 1 {
			noisyB++
			fmt.Fprintf(b, "NOISE B %s: %d transcripts over %d runs\n", n, len(sb), len(cb))
		}
		for sha, c := range sb {
			if _, ok := sa[sha]; ok {
				continue
			}
			changed++
			// Against the A view it is nearest to: the lines one has more
			// of than the other (a multiset difference).
			var ref *recordCell
			var onlyA, onlyB []string
			for _, r := range sa {
				oa, ob := lineDiff(t, r.file+v.ext, c.file+v.ext)
				if ref == nil || len(oa)+len(ob) < len(onlyA)+len(onlyB) {
					ref, onlyA, onlyB = r, oa, ob
				}
			}
			fmt.Fprintf(b, "B NOT IN A %s\n  %s\n", n, firstDiff(ref.file+v.ext, c.file+v.ext))
			fmt.Fprintf(b, "  nearest A view: %d lines only in A, %d only in B\n", len(onlyA), len(onlyB))
			for _, l := range onlyA[:min(len(onlyA), 4)] {
				fmt.Fprintf(b, "    A only: %s\n", clip(l))
			}
			for _, l := range onlyB[:min(len(onlyB), 4)] {
				fmt.Fprintf(b, "    B only: %s\n", clip(l))
			}
		}
		failA, failB := 0, 0
		for _, c := range ca {
			if c.counts.Failed {
				failA++
			}
		}
		for _, c := range cb {
			if c.counts.Failed {
				failB++
			}
		}
		if (failA > 0) != (failB > 0) {
			verdicts++
			fmt.Fprintf(b, "VERDICT %s: failed in %d of %d A runs, %d of %d B runs\n", n, failA, len(ca), failB, len(cb))
		}
		if !withWaits {
			continue
		}
		// Waits: the first run of each band gives the shape; every run's
		// passes are summed per kind.
		shape := func(c *recordCell) []string {
			var s []string
			for _, w := range c.counts.Waits {
				s = append(s, w.What)
			}
			return s
		}
		if strings.Join(shape(ca[0]), "\x00") != strings.Join(shape(cb[0]), "\x00") {
			waitShape++
			fmt.Fprintf(b, "WAITS DIFFER %s: %d waits in A, %d in B\n", n, len(ca[0].counts.Waits), len(cb[0].counts.Waits))
			continue
		}
		for i, wb := range cb[0].counts.Waits {
			k := wb.Kind
			if tl := kinds[k]; tl == nil {
				kinds[k] = &tally{}
			}
			tl := kinds[k]
			tl.waits++
			pa, pb := 0, 0
			for _, c := range ca {
				if i < len(c.counts.Waits) {
					pa += c.counts.Waits[i].Passes
				}
			}
			for _, c := range cb {
				if i < len(c.counts.Waits) {
					pb += c.counts.Waits[i].Passes
					if c.counts.Waits[i].Passes == 0 {
						tl.zeroB++
					}
				}
			}
			tl.ticksA += pa
			tl.ticksB += pb
			// Per wait, the band means against each other.
			ma, mb := float64(pa)/float64(len(ca)), float64(pb)/float64(len(cb))
			switch {
			case mb < ma:
				tl.lessB++
			case mb > ma:
				tl.moreB++
			default:
				tl.sameB++
			}
		}
	}
	fmt.Fprintf(b, "\n%d cells: %d with a B transcript A never had; %d noisy in A, %d noisy in B; %d verdicts differ; "+
		"%d in one band only; %d with a different wait sequence\n\n", len(names), changed, noisyA, noisyB, verdicts,
		missing, waitShape)
	if !withWaits {
		return
	}
	fmt.Fprintf(b, "| wait kind (B) | waits | ticks A (all runs) | ticks B | B waits met with no tick | waits B fewer / same / more ticks (band means) |\n")
	fmt.Fprintf(b, "|---|---|---|---|---|---|\n")
	ks := make([]string, 0, len(kinds))
	for k := range kinds {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		tl := kinds[k]
		fmt.Fprintf(b, "| %s | %d | %d | %d | %d | %d / %d / %d |\n", k, tl.waits, tl.ticksA, tl.ticksB, tl.zeroB,
			tl.lessB, tl.sameB, tl.moreB)
	}
	fmt.Fprintln(b)
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
	var raw []string
	for _, l := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
		if l != "" {
			raw = append(raw, l)
		}
	}
	v := buildViews(raw)
	cb, err := os.ReadFile(base + ".counts.json")
	if err != nil {
		return err
	}
	var c recordCounts
	if err := json.Unmarshal(cb, &c); err != nil {
		return err
	}
	c.Transcript, c.Orders, c.Messages, c.Timers = sha(v.transcript), sha(v.orders), v.messages, v.timers
	out, err := json.MarshalIndent(c, "", " ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(base+".transcript", []byte(v.transcript), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(base+".orders", []byte(v.orders), 0o644); err != nil {
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
func writeEqualCells(b *strings.Builder, runs []recordRun, v recordView) {
	fmt.Fprintf(b, "## Cells with equal %s\n\n", v.name)
	for _, run := range runs {
		groups := map[string][]string{}
		for _, n := range sortedCells(run) {
			sha := v.sha(run.cells[n].counts)
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
func writeRunDiffs(b *strings.Builder, runs []recordRun, v recordView) {
	if len(runs) < 2 {
		return
	}
	fmt.Fprintf(b, "## Same cell, run to run (%d runs): %s\n\n", len(runs), v.name)
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
				shas[v.sha(c.counts)] = append(shas[v.sha(c.counts)], i)
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
		fmt.Fprintf(b, "  %s\n", firstDiff(a.file+v.ext, z.file+v.ext))
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

// recordView is one of the two views a cell is written in (buildViews).
type recordView struct {
	name, ext string
	sha       func(recordCounts) string
}

var recordViewKinds = []recordView{
	{"transcripts (delivered order)", ".transcript", func(c recordCounts) string { return c.Transcript }},
	{"orders views (by order)", ".orders", func(c recordCounts) string { return c.Orders }},
}

// lineDiff is the multiset difference of two files' lines: the lines a has
// more of than z, and those z has more of than a. Section headers ("order:",
// "no order:") are left out.
func lineDiff(t *testing.T, a, z string) (onlyA, onlyZ []string) {
	t.Helper()
	ab, err := os.ReadFile(a)
	if err != nil {
		t.Fatalf("read %s: %v", a, err)
	}
	zb, err := os.ReadFile(z)
	if err != nil {
		t.Fatalf("read %s: %v", z, err)
	}
	n := map[string]int{}
	for _, l := range strings.Split(string(ab), "\n") {
		n[l]++
	}
	for _, l := range strings.Split(string(zb), "\n") {
		n[l]--
	}
	for l, k := range n {
		if l == "" || strings.HasSuffix(l, ":") {
			continue
		}
		for ; k > 0; k-- {
			onlyA = append(onlyA, l)
		}
		for ; k < 0; k++ {
			onlyZ = append(onlyZ, l)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyZ)
	return onlyA, onlyZ
}
