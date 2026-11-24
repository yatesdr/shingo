package main

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"

	"shingo/shared/scripts/internal/testset"
)

// --- test-costs ------------------------------------------------------------
//
// A top-level test that costs more than 10 s run alone (the larger of its own
// wall and its leaves' seconds summed) says why, in its doc comment:
//
//	// cost: 58 leaves, each its own Core, Edge and databases, ...
//	func TestScenario_KeepStagedRecovery_AfterCancelledChangeover(t *testing.T) {
//
// WHY. A test's seconds are paid by every gate run after it lands, and nobody
// sees them at review: 47-50 top-level tests over 10 s held 73% of the docker
// suites' slot-seconds (the gate-time census, 2026-11-22). The price is a
// sentence a reviewer can check against the code: what needs the time, and
// what a cheaper tier cannot assert (owner, 2026-11-24, T10).
//
// TWO HALVES. This rule is the static one, read in every gate: where a reason
// stands, and the list of the tests that were over the line when the rule came
// in. The time is read by scripts/check-test-cost.sh (shared/scripts/testcost),
// which times the tests new since the base that carry no reason, alone, and
// fails one over the line.
//
// WHAT IS READ. Every tracked _test.go file that holds testset.CostMarker; and
// the list, costListFile, whose `over <dir> <test> <seconds> <reason>` lines
// name the tests over the line at the rule's start that carried no marker then
// (the seconds are the larger of wall and leaves, alone, as the file's header
// says where they were read). A reason the list could not establish from the
// test starts with costUnestablished.
//
// WHAT FIRES:
//   - a `// cost:` line that is not in the doc comment of a top-level test (on
//     a helper, inside a body, a blank line from its func): nothing reads it;
//   - a marker with no reason after the colon;
//   - in the list: a line not of that shape or with no reason; a test named
//     twice; a test that is not a top-level test of its directory (gone or
//     renamed: the line goes); a listed test that carries a marker now (the
//     marker says it, so the line goes).
//
// HOW IT SHRINKS. A listed test leaves the list when someone writes its reason
// at the test, or makes it cheaper. A new test is never added to the list; it
// carries the marker.
const costListFile = "shared/scripts/repolint/test-costs.txt"

// costUnestablished starts a listed reason that the test's own code and
// comments did not give when the list was written.
const costUnestablished = "UNESTABLISHED"

// costEntry is one `over` line of the list.
type costEntry struct {
	line      int
	dir, test string
}

func testCosts(t *tree) result {
	var (
		fs       []Finding
		files    []*file
		byDir    = map[string][]*file{}
		listData []byte
		listed   bool
	)
	for _, f := range t.files {
		if f.path == costListFile {
			listData, listed = f.data, f.data != nil
		}
		if f.data == nil || !testset.IsTestFile(f.path) {
			continue
		}
		byDir[path.Dir(f.path)] = append(byDir[path.Dir(f.path)], f)
		if strings.Contains(string(f.data), testset.CostMarker) {
			files = append(files, f)
		}
	}
	if !listed {
		return result{findings: []Finding{{File: costListFile, Why: "the list is not in the tree, so the tests over the line at the rule's start are named nowhere"}}}
	}

	// The markers, and the tests carrying one.
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		marked = map[string]bool{} // "<dir> <test>"
		sem    = make(chan struct{}, 16)
	)
	for _, f := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func(f *file) {
			defer func() { <-sem; wg.Done() }()
			ls, err := testset.CostLines(f.path, f.data)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				fs = append(fs, Finding{File: f.path, Why: "does not parse, so its `" + testset.CostMarker + "` lines cannot be read: " + err.Error()})
				return
			}
			for _, l := range ls {
				switch {
				case l.Test == "":
					fs = append(fs, Finding{File: f.path, Line: l.Line,
						Why: "a `" + testset.CostMarker + "` line outside a top-level test's doc comment: nothing reads it there"})
				case l.Reason == "":
					fs = append(fs, Finding{File: f.path, Line: l.Line, Why: l.Test + "'s `" + testset.CostMarker + "` marker gives no reason"})
				default:
					marked[path.Dir(f.path)+" "+l.Test] = true
				}
			}
		}(f)
	}
	wg.Wait()

	// The list.
	var entries []costEntry
	seen := map[string]int{}
	unestablished := 0
	eachLine(listData, func(n int, line []byte) {
		l := strings.TrimSpace(strings.TrimRight(string(line), "\r"))
		if l == "" || strings.HasPrefix(l, "#") {
			return
		}
		f := strings.Fields(l)
		if len(f) < 5 || f[0] != "over" {
			fs = append(fs, Finding{File: costListFile, Line: n, Why: "is not `over <dir> <test> <seconds> <reason>`"})
			return
		}
		if s, err := strconv.ParseFloat(f[3], 64); err != nil || s <= 0 {
			fs = append(fs, Finding{File: costListFile, Line: n, Why: "names no seconds: " + f[3]})
			return
		}
		key := f[1] + " " + f[2]
		if first, dup := seen[key]; dup {
			fs = append(fs, Finding{File: costListFile, Line: n, Why: fmt.Sprintf("names %s again (first on line %d)", key, first)})
			return
		}
		seen[key] = n
		if f[4] == costUnestablished || strings.HasPrefix(f[4], costUnestablished+":") {
			unestablished++
		}
		entries = append(entries, costEntry{n, f[1], f[2]})
	})
	tops := map[string]map[string]bool{}
	for _, e := range entries {
		if tops[e.dir] == nil {
			tops[e.dir] = map[string]bool{}
			for _, f := range byDir[e.dir] {
				names, err := testset.TestFuncs(f.path, f.data)
				if err != nil {
					continue // reported above when it holds a marker; the test step reports the rest
				}
				for n := range names {
					tops[e.dir][n] = true
				}
			}
		}
		switch key := e.dir + " " + e.test; {
		case !tops[e.dir][e.test]:
			fs = append(fs, Finding{File: costListFile, Line: e.line,
				Why: "over " + key + ": no top-level test of that name in " + e.dir + ", so the line goes"})
		case marked[key]:
			fs = append(fs, Finding{File: costListFile, Line: e.line,
				Why: "over " + key + ": the test says why at its func now, so the line goes"})
		}
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].where() < fs[j].where() })
	return result{fs, fmt.Sprintf("%d tests say why they cost; %d listed from the rule's start, %d of them with no reason established",
		len(marked), len(entries), unestablished)}
}
