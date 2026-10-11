package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// [S.6] EDGE WORK: what each Edge did per completed order, on every run.
//
// STATEMENTS come from edge-statements.tsv beside the Edge's database: a sim
// build of the Edge opens its store through the statement counter and rewrites
// that file every 30 s with its running totals by verb and first table
// (shingo-edge/cmd/shingoedge/sim_statements.go). A production build has no
// counter, so the Pi pays nothing for this number. The file's last write is up
// to 30 s behind the collect: the figure is a floor by that much.
//
// LOG LINES come from the Edge's own log, by the [component] tag each line
// carries; a line without one is "other".
//
// THE DENOMINATOR is Core's completed orders for the whole run (ORDERS
// completed), the same one C2c's per-order pair used, so two runs compare even
// when one has two Edges.
const (
	measureEdgeWork = "[S.6] EDGE WORK PER COMPLETED ORDER"
	edgeWorkTop     = 8
)

var edgeLogComponentRe = regexp.MustCompile(`^\S+ \[([a-z0-9_.-]+)\]`)

// edgeWork is one Edge's statement and log totals.
type edgeWork struct {
	stmts     map[string]int64 // verb + table -> statements
	stmtTotal int64
	stmtsRead bool
	logLines  int
	logByComp map[string]int
}

// readEdgeWork reads the statement file beside dbPath (absent: not counted)
// and, when log is non-nil, folds its lines by component.
func readEdgeWork(dbPath string, log []byte) (*edgeWork, error) {
	w := &edgeWork{stmts: map[string]int64{}, logByComp: map[string]int{}}
	p := filepath.Join(filepath.Dir(dbPath), "edge-statements.tsv")
	if b, err := os.ReadFile(p); err == nil {
		w.stmtsRead = true
		for _, line := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(line, "\t")
			if !ok {
				continue
			}
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%s: %q: %w", p, line, err)
			}
			w.stmts[k] += n
			w.stmtTotal += n
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if log != nil {
		sc := bufio.NewScanner(bytes.NewReader(log))
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			w.logLines++
			w.logByComp[edgeLogComponent(sc.Text())]++
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	return w, nil
}

// edgeLogComponent is a line's [component], after any docker prefix
// ("edge-1  | <ts> ") and the Edge's own timestamp.
func edgeLogComponent(line string) string {
	if _, rest, ok := strings.Cut(line, " | "); ok {
		line = rest
	}
	// docker's -t timestamp, then the Edge's: drop the first when two lead.
	if f := strings.SplitN(line, " ", 3); len(f) == 3 && looksLikeTS(f[0]) && looksLikeTS(f[1]) {
		line = f[1] + " " + f[2]
	}
	if m := edgeLogComponentRe.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return "other"
}

func looksLikeTS(s string) bool {
	return len(s) >= 19 && s[4] == '-' && s[7] == '-' && s[10] == 'T'
}

type keyCount struct {
	key string
	n   int64
}

func topCounts(m map[string]int64, n int) []keyCount {
	out := make([]keyCount, 0, len(m))
	for k, v := range m {
		out = append(out, keyCount{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].key < out[j].key
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// printEdgeWork writes the section: per Edge, then the run's total.
func printEdgeWork(edges []*edgeReading, completed reading) {
	if len(edges) == 0 {
		return
	}
	fmt.Printf("\n%s", measureEdgeWork)
	if completed.failed || completed.n == 0 {
		fmt.Println("  n/a (no completed orders read)")
		return
	}
	fmt.Printf("  per Core completed order (%d); statements from each Edge's sim counter, log lines from its log\n", completed.n)
	per := func(n int64) float64 { return float64(n) / float64(completed.n) }
	var stmts, logs int64
	stmtsAll, logsAll := true, true
	for _, e := range edges {
		if e == nil || e.work == nil {
			continue
		}
		w := e.work
		st := "n/a (no edge-statements.tsv: not a sim build, or not collected)"
		if w.stmtsRead {
			st = fmt.Sprintf("%.2f (%d)", per(w.stmtTotal), w.stmtTotal)
			stmts += w.stmtTotal
		} else {
			stmtsAll = false
		}
		lg := "n/a (no edge.log)"
		if e.logRead {
			lg = fmt.Sprintf("%.2f (%d)", per(int64(w.logLines)), w.logLines)
			logs += int64(w.logLines)
		} else {
			logsAll = false
		}
		fmt.Printf("        %s (%s): statements %s · log lines %s\n", e.station, e.src.label, st, lg)
		if w.stmtsRead {
			var parts []string
			for _, kc := range topCounts(w.stmts, edgeWorkTop) {
				parts = append(parts, fmt.Sprintf("%s %.2f", kc.key, per(kc.n)))
			}
			fmt.Printf("          statements: %s\n", strings.Join(parts, " · "))
		}
		if e.logRead {
			comp := make(map[string]int64, len(w.logByComp))
			for k, v := range w.logByComp {
				comp[k] = int64(v)
			}
			var parts []string
			for _, kc := range topCounts(comp, edgeWorkTop) {
				parts = append(parts, fmt.Sprintf("%s %.2f", kc.key, per(kc.n)))
			}
			fmt.Printf("          log lines:  %s\n", strings.Join(parts, " · "))
		}
	}
	total := func(ok bool, n int64) string {
		if !ok {
			return "incomplete"
		}
		return fmt.Sprintf("%.2f", per(n))
	}
	fmt.Printf("        EDGE WORK: statements/order %s · log lines/order %s\n", total(stmtsAll, stmts), total(logsAll, logs))
}
