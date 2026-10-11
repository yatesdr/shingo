package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// [S.6] EDGE WORK: what each Edge did per PLC tick, as the run's total, and per
// completed order, on every run.
//
// PER TICK FIRST. The Pi's work is read per PLC tick and as the run's total; a
// count per completed order moves with throughput (more orders make the same
// work read smaller, and a fall in work can read as a rise), so it is the third
// column, kept so runs compare with the ones before.
//
// STATEMENTS come from edge-statements.tsv beside the Edge's database: a sim
// build of the Edge opens its store through the statement counter and rewrites
// that file every 30 s with its running totals
// (shingo-edge/cmd/shingoedge/sim_statements.go). Two formats:
//   - tagged (a header line "tag<TAB>verb<TAB>table<TAB>statements<TAB>path"):
//     one row per caller tag, verb, principal table and code path. The tag says
//     which part of the Edge sent it (tick, order, loop, poll, sim, other); the
//     path is "entry > reader", the frame that classified it and the code that
//     issued the read.
//   - old ("VERB table<TAB>count", before the tags): no tag, no path, and the
//     table is the first one in the SQL text, so a sub-select's table took the
//     whole statement (every claim read read as payload_catalog). Still read,
//     with a note.
//
// A production build has no counter, so the Pi pays nothing for this number.
// The file's last write is up to 30 s behind the collect: the figure is a floor
// by that much.
//
// PLC TICKS are the Edge database's counter_snapshots rows with a positive
// delta. The PLC manager writes one row per counter change it reads (the
// production tick feed's queue), and a positive delta is exactly when it emits
// the counter-delta event the engine's tick path runs on. Rows are kept 14 days
// (counters.SnapshotRetention), longer than any run, and a run's Edge database
// is fresh. The "[plc] counter delta" log line is not used: it is a debug line
// the log gate can switch off.
//
// LOG LINES come from the Edge's own log, by the [component] tag each line
// carries; a line without one is "other".
//
// THE ORDER DENOMINATOR is Core's completed orders for the whole run (ORDERS
// completed), the same one C2c's per-order pair used.
const (
	measureEdgeWork = "[S.6] EDGE WORK"
	edgeWorkTop     = 8
	edgeStmtTop     = 10
	edgeStmtHeader  = "tag\tverb\ttable\tstatements\tpath"
)

// edgeTags is the counter's caller tags in print order.
var edgeTags = []string{"tick", "order", "loop", "poll", "sim", "other"}

var edgeLogComponentRe = regexp.MustCompile(`^\S+ \[([a-z0-9_.-]+)\]`)

// edgeStmtRow is one row of the tagged statement file.
type edgeStmtRow struct {
	tag, key, path string // key is "VERB table"
	n              int64
}

// edgeWork is one Edge's statement, tick and log totals.
type edgeWork struct {
	stmts     map[string]int64 // "VERB table" -> statements, either format
	rows      []edgeStmtRow    // the tagged format's rows; nil for the old format
	tagged    bool
	stmtTotal int64
	stmtsRead bool

	ticks     int64 // PLC ticks: counter_snapshots rows with delta > 0
	ticksRead bool

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
		if err := w.parseStatements(p, string(b)); err != nil {
			return nil, err
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

// parseStatements reads either format of the counter's file.
func (w *edgeWork) parseStatements(p, text string) error {
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && strings.TrimRight(lines[0], "\r") == edgeStmtHeader {
		w.tagged = true
		lines = lines[1:]
	}
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		want := 2
		if w.tagged {
			want = 5
		}
		if len(f) != want {
			return fmt.Errorf("%s: %q: want %d tab-separated fields", p, line, want)
		}
		key, count := f[0], f[1]
		var row edgeStmtRow
		if w.tagged {
			key, count = f[1]+" "+f[2], f[3]
			row = edgeStmtRow{tag: f[0], key: key, path: f[4]}
		}
		n, err := strconv.ParseInt(strings.TrimSpace(count), 10, 64)
		if err != nil {
			return fmt.Errorf("%s: %q: %w", p, line, err)
		}
		w.stmts[key] += n
		w.stmtTotal += n
		if w.tagged {
			row.n = n
			w.rows = append(w.rows, row)
		}
	}
	return nil
}

// readPLCTicks counts the Edge's PLC ticks (see the section comment).
func readPLCTicks(db *sql.DB) (int64, error) {
	var n int64
	err := db.QueryRow(`SELECT COUNT(*) FROM counter_snapshots WHERE delta > 0`).Scan(&n)
	return n, err
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

// sortedCounts is m's entries by count descending, then key; all of them when
// n < 0.
func sortedCounts(m map[string]int64, n int) []keyCount {
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
	if n >= 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// edgeRates divides by an Edge's (or the run's) ticks and completed orders.
type edgeRates struct {
	ticks, orders int64 // 0: not read
}

func (r edgeRates) perTick(n int64) string {
	if r.ticks <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", float64(n)/float64(r.ticks))
}

func (r edgeRates) perOrder(n int64) string {
	if r.orders <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", float64(n)/float64(r.orders))
}

// row is "<per tick>/tick <total> total <per order>/order", aligned.
func (r edgeRates) row(n int64) string {
	return fmt.Sprintf("%10s/tick %10d total %10s/order", r.perTick(n), n, r.perOrder(n))
}

// printEdgeWork writes the section: per Edge, then the run's total.
func printEdgeWork(out io.Writer, edges []*edgeReading, completed reading) {
	if len(edges) == 0 {
		return
	}
	var orders int64
	ordersNote := "n/a: completed orders not read"
	if !completed.failed && completed.n > 0 {
		orders = int64(completed.n)
		ordersNote = strconv.Itoa(completed.n)
	}
	fmt.Fprintf(out, "\n%s  per PLC tick, run total, per completed order (%s); statements from each Edge's sim counter,"+
		" PLC ticks from its counter_snapshots, log lines from its log\n", measureEdgeWork, ordersNote)

	var stmts, logs, ticks int64
	stmtsAll, logsAll, ticksAll := true, true, true
	for _, e := range edges {
		if e == nil || e.work == nil {
			continue
		}
		w := e.work
		tk := "n/a (counter_snapshots not read)"
		if w.ticksRead {
			tk = fmt.Sprintf("%d (counter_snapshots rows with delta > 0)", w.ticks)
			ticks += w.ticks
		} else {
			ticksAll = false
		}
		rates := edgeRates{ticks: w.ticks, orders: orders}
		fmt.Fprintf(out, "        %s (%s): PLC ticks %s\n", e.station, e.src.label, tk)
		if w.stmtsRead {
			stmts += w.stmtTotal
			fmt.Fprintf(out, "          statements %s\n", rates.row(w.stmtTotal))
		} else {
			stmtsAll = false
			fmt.Fprintln(out, "          statements n/a (no edge-statements.tsv: not a sim build, or not collected)")
		}
		if e.logRead {
			logs += int64(w.logLines)
			fmt.Fprintf(out, "          log lines  %s\n", rates.row(int64(w.logLines)))
		} else {
			logsAll = false
			fmt.Fprintln(out, "          log lines  n/a (no edge.log)")
		}
		if w.stmtsRead {
			printEdgeStatements(out, w, rates)
		}
		if e.logRead {
			comp := make(map[string]int64, len(w.logByComp))
			for k, v := range w.logByComp {
				comp[k] = int64(v)
			}
			var parts []string
			for _, kc := range sortedCounts(comp, edgeWorkTop) {
				parts = append(parts, fmt.Sprintf("%s %s", kc.key, rates.perTick(kc.n)))
			}
			fmt.Fprintf(out, "          log lines by component, per tick: %s\n", strings.Join(parts, " · "))
		}
	}
	run := edgeRates{ticks: ticks, orders: orders}
	if !ticksAll {
		run.ticks = 0
	}
	total := func(ok bool, n int64) string {
		if !ok {
			return "incomplete"
		}
		return fmt.Sprintf("%s/tick · %d total · %s/order", run.perTick(n), n, run.perOrder(n))
	}
	tk := "incomplete"
	if ticksAll {
		tk = strconv.FormatInt(ticks, 10)
	}
	fmt.Fprintf(out, "        EDGE WORK (all Edges): PLC ticks %s · statements %s · log lines %s\n",
		tk, total(stmtsAll, stmts), total(logsAll, logs))
}

// printEdgeStatements writes one Edge's split by caller tag and its top
// statements by table, each with its tag mix and its most frequent code path.
func printEdgeStatements(out io.Writer, w *edgeWork, rates edgeRates) {
	byTag := map[string]int64{}
	keyTags := map[string]map[string]int64{}
	keyPaths := map[string]map[string]int64{}
	for _, r := range w.rows {
		byTag[r.tag] += r.n
		if keyTags[r.key] == nil {
			keyTags[r.key], keyPaths[r.key] = map[string]int64{}, map[string]int64{}
		}
		keyTags[r.key][r.tag] += r.n
		keyPaths[r.key][r.path] += r.n
	}
	share := func(n, of int64) float64 {
		if of == 0 {
			return 0
		}
		return 100 * float64(n) / float64(of)
	}
	if w.tagged {
		parts := make([]string, 0, len(edgeTags)+1)
		for _, t := range edgeTags {
			parts = append(parts, fmt.Sprintf("%s %d (%.1f%%)", t, byTag[t], share(byTag[t], w.stmtTotal)))
			delete(byTag, t)
		}
		for _, kc := range sortedCounts(byTag, -1) { // a tag this reader does not know: shown, not dropped
			parts = append(parts, fmt.Sprintf("%s %d (%.1f%%)", kc.key, kc.n, share(kc.n, w.stmtTotal)))
		}
		fmt.Fprintf(out, "          by tag: %s\n", strings.Join(parts, " · "))
		fmt.Fprintf(out, "          top %d statements by table (principal table), each with its tags and its most frequent code path:\n", edgeStmtTop)
	} else {
		fmt.Fprintln(out, "          by tag: n/a (old two-column edge-statements.tsv: no caller tags or code paths;"+
			" each statement is named by the first table in its text)")
		fmt.Fprintf(out, "          top %d statements by table (first table in the text):\n", edgeStmtTop)
	}
	for i, kc := range sortedCounts(w.stmts, edgeStmtTop) {
		line := fmt.Sprintf("          %2d. %-36s %s", i+1, kc.key, rates.row(kc.n))
		if !w.tagged {
			fmt.Fprintln(out, line)
			continue
		}
		var tags []string
		for _, tc := range sortedCounts(keyTags[kc.key], -1) {
			tags = append(tags, fmt.Sprintf("%s %.0f%%", tc.key, share(tc.n, kc.n)))
		}
		fmt.Fprintf(out, "%s  %s\n", line, strings.Join(tags, " · "))
		if top := sortedCounts(keyPaths[kc.key], 1); len(top) == 1 {
			fmt.Fprintf(out, "              %s  (%.0f%% of these)\n", top[0].key, share(top[0].n, kc.n))
		}
	}
}
